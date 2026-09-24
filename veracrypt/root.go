// Package veracrypt implements VeraCrypt containers as backend-neutral
// hierarchies. Product policy such as ACLs, reserved names and file identity
// remains with the caller; this package owns container and FAT mechanics.
package veracrypt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stowcloud/storage"
	format "github.com/stowcloud/veracrypt"
)

const (
	MinContainerDataMiB   uint64 = 16
	MaxContainerDataMiB   uint64 = 1 << 20
	maxPIM                       = 10_000
	maxConfigBytes               = 16 << 10
	maxContainerPathBytes        = 4096
)

var (
	ErrContainerSize         = format.ErrContainerSize
	ErrUnknownHash           = format.ErrUnknownHash
	ErrWrongPassword         = format.ErrWrongPassword
	ErrHeaderCorrupt         = format.ErrHeaderCorrupt
	ErrUnsupportedVolume     = format.ErrUnsupportedVolume
	ErrHeaderFieldsInvalid   = format.ErrHeaderFieldsInvalid
	ErrUnsupportedFilesystem = format.ErrUnsupportedFilesystem
	ErrNotFound              = format.ErrNotFound
	ErrDenied                = format.ErrDenied
	ErrExists                = format.ErrExists
	ErrNotEmpty              = format.ErrNotEmpty
	ErrNoSpace               = format.ErrNoSpace
	ErrNotADirectory         = format.ErrNotADirectory
	ErrIsDirectory           = format.ErrIsDirectory
)

// Config is the secret-free persisted configuration for one container.
type Config struct {
	Container     string `json:"container"`
	CreateSizeMiB uint64 `json:"create_size_mib"`
	PIM           uint32 `json:"pim,omitempty"`
	Hash          string `json:"hash,omitempty"`
}

func ParseConfig(b []byte) (Config, error) {
	if len(b) == 0 {
		return Config{}, fmt.Errorf("veracrypt: empty config")
	}
	if len(b) > maxConfigBytes {
		return Config{}, fmt.Errorf("veracrypt: config exceeds %d bytes", maxConfigBytes)
	}
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("veracrypt: parse config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("veracrypt: parse config: trailing data")
		}
		return Config{}, fmt.Errorf("veracrypt: parse config: trailing data: %w", err)
	}
	if c.Container == "" {
		return Config{}, fmt.Errorf("veracrypt: container path is required")
	}
	if len(c.Container) > maxContainerPathBytes {
		return Config{}, fmt.Errorf("veracrypt: container path exceeds %d bytes", maxContainerPathBytes)
	}
	if !filepath.IsAbs(c.Container) {
		return Config{}, fmt.Errorf("veracrypt: container path %q must be absolute", c.Container)
	}
	if containsNUL(c.Container) {
		return Config{}, fmt.Errorf("veracrypt: container path contains a NUL byte")
	}
	if c.CreateSizeMiB != 0 && (c.CreateSizeMiB < MinContainerDataMiB || c.CreateSizeMiB > MaxContainerDataMiB) {
		return Config{}, ErrContainerSize
	}
	if c.PIM > maxPIM {
		return Config{}, fmt.Errorf("veracrypt: PIM %d exceeds %d", c.PIM, maxPIM)
	}
	if err := validateHashToken(c.Hash); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Marshal() ([]byte, error) { return json.Marshal(c) }
func (c Config) Describe() string         { return c.Container }

func validateHashToken(token string) error {
	switch token {
	case "", "sha512", "sha256", "blake2s", "whirlpool", "streebog", "argon2":
		return nil
	default:
		return errors.Join(ErrUnknownHash, errors.New(token))
	}
}

// Options controls opening or creating a container. Password is borrowed only
// for the duration of Open and is never retained by the returned Root.
type Options struct {
	Config     Config
	Password   []byte
	Create     bool
	ScratchDir string
	Clock      format.Clock
}

// StatInfo is the identity-independent metadata needed by a product adapter.
type StatInfo struct {
	IsDir   bool
	Size    uint64
	MtimeNs int64
	Ino     uint64
}

type Dirent struct {
	Name  string
	IsDir bool
	Ino   uint64
}

type Durable struct{ Replaced bool }

type DurableOptions struct {
	NoClobber bool
	MtimeNs   int64
}

// Part is a caller-owned scratch upload. It is closed by PublishPart or Close.
type Part struct {
	file *os.File
	once sync.Once
}

func (p *Part) ReadAt(b []byte, off int64) (int, error)  { return p.file.ReadAt(b, off) }
func (p *Part) WriteAt(b []byte, off int64) (int, error) { return p.file.WriteAt(b, off) }
func (p *Part) Sync() error                              { return p.file.Sync() }
func (p *Part) Close() error {
	if p == nil || p.file == nil {
		return nil
	}
	var err error
	p.once.Do(func() { err = p.file.Close() })
	return err
}

// Root is a mounted FAT hierarchy inside one VeraCrypt container.
type Root struct {
	dev       *format.Container
	fs        format.Filesystem
	container string
	scratch   string
	devID     uint64
	mu        sync.Mutex
}

var _ storage.ReadHierarchy = (*RootAdapter)(nil)
var _ storage.Materializer = (*RootAdapter)(nil)
var _ storage.SpaceReporter = (*RootAdapter)(nil)
var _ storage.Renamer = (*RootAdapter)(nil)

// RootAdapter exposes Root's read-only neutral capability while Root keeps
// the richer mutation surface required by the product adapter.
type RootAdapter struct{ *Root }

func (r *RootAdapter) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	st, err := r.Root.Stat(ctx, path)
	if err != nil {
		return storage.Entry{}, err
	}
	kind := storage.KindFile
	if st.IsDir {
		kind = storage.KindDirectory
	}
	return storage.Entry{Path: path, Kind: kind, Size: int64(st.Size), ModTime: time.Unix(0, st.MtimeNs).UTC()}, nil
}
func (r *RootAdapter) ReadDir(ctx context.Context, path storage.Path) ([]storage.Entry, error) {
	es, err := r.Root.ReadDir(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Entry, 0, len(es))
	for _, e := range es {
		child, jerr := path.Join(e.Name)
		if jerr != nil {
			return nil, jerr
		}
		kind := storage.KindFile
		if e.IsDir {
			kind = storage.KindDirectory
		}
		st, serr := r.Root.Stat(ctx, child)
		if serr != nil {
			return nil, serr
		}
		out = append(out, storage.Entry{Path: child, Kind: kind, Size: int64(st.Size), ModTime: time.Unix(0, st.MtimeNs).UTC()})
	}
	return out, nil
}
func (r *RootAdapter) OpenRead(ctx context.Context, path storage.Path) (io.ReadCloser, error) {
	return r.Root.OpenRead(ctx, path)
}
func (r *RootAdapter) Materialize(ctx context.Context, path storage.Path) (*storage.Materialized, error) {
	return r.Root.Materialize(ctx, path)
}
func (r *RootAdapter) Rename(ctx context.Context, from, to storage.Path) error {
	return r.Root.Rename(ctx, from, to)
}
func (r *RootAdapter) Space(ctx context.Context, path storage.Path) (storage.Space, error) {
	return r.Root.Space(ctx, path)
}

func Open(ctx context.Context, opt Options) (*Root, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if opt.Config.Container == "" {
		return nil, fmt.Errorf("veracrypt: container path is required")
	}
	if !filepath.IsAbs(opt.Config.Container) || containsNUL(opt.Config.Container) {
		return nil, fmt.Errorf("veracrypt: container path is invalid")
	}
	if opt.ScratchDir == "" {
		return nil, fmt.Errorf("veracrypt: scratch dir is required")
	}
	st, statErr := os.Stat(opt.ScratchDir)
	if statErr != nil {
		return nil, fmt.Errorf("veracrypt: stat scratch dir: %w", statErr)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("veracrypt: scratch path is not a directory")
	}

	_, containerErr := os.Stat(opt.Config.Container)
	missing := errors.Is(containerErr, os.ErrNotExist)
	switch {
	case containerErr == nil:
	case missing && opt.Create:
		sizeMiB := opt.Config.CreateSizeMiB
		if sizeMiB == 0 {
			sizeMiB = MinContainerDataMiB
		}
		if err := createContainer(opt.Config.Container, sizeMiB, opt.Password); err != nil {
			return nil, err
		}
	case missing:
		return nil, fmt.Errorf("veracrypt: container %q: %w", opt.Config.Container, ErrNotFound)
	default:
		return nil, fmt.Errorf("veracrypt: stat container %q: %w", opt.Config.Container, containerErr)
	}

	dev, dataSize, err := openContainer(opt.Config.Container, opt.Password, opt.Config.PIM, opt.Config.Hash)
	if err != nil {
		return nil, err
	}
	closeDev := true
	defer func() {
		if closeDev {
			_ = dev.Close()
		}
	}()
	if missing && opt.Create {
		if err := format.FormatFAT(dev, dataSize); err != nil {
			return nil, fmt.Errorf("veracrypt: format new container: %w", mapError(err))
		}
	}
	clk := opt.Clock
	if clk == nil {
		clk = format.SystemClock()
	}
	fs, err := format.MountFilesystem(dev, dataSize, clk)
	if err != nil {
		return nil, mapError(err)
	}
	closeDev = false
	return &Root{dev: dev, fs: fs, container: opt.Config.Container, scratch: opt.ScratchDir, devID: syntheticDevice(opt.Config.Container)}, nil
}

func createContainer(path string, size uint64, password []byte) error {
	pw := append([]byte(nil), password...)
	defer clear(pw)
	return mapError(format.Create(path, size, pw))
}

func openContainer(path string, password []byte, pim uint32, hash string) (*format.Container, uint64, error) {
	pw := append([]byte(nil), password...)
	defer clear(pw)
	dev, size, err := format.Open(path, pw, pim, hash)
	return dev, size, mapError(err)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	return err
}

func (r *Root) safePath(path storage.Path) (format.SafePath, error) {
	return format.ParseSafePath(path.String())
}

func (r *Root) Stat(ctx context.Context, path storage.Path) (StatInfo, error) {
	if err := contextErr(ctx); err != nil {
		return StatInfo{}, err
	}
	p, err := r.safePath(path)
	if err != nil {
		return StatInfo{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.fs.Stat(p)
	if err != nil {
		return StatInfo{}, mapError(err)
	}
	return StatInfo{IsDir: st.IsDir, Size: st.Size, MtimeNs: st.MtimeNs, Ino: st.Ino}, nil
}

func (r *Root) ReadDir(ctx context.Context, path storage.Path) ([]Dirent, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	p, err := r.safePath(path)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.fs.ReadDir(p)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]Dirent, len(entries))
	for i, e := range entries {
		out[i] = Dirent{Name: e.Name, IsDir: e.IsDir, Ino: e.Ino}
	}
	return out, nil
}

func (r *Root) ReadFile(ctx context.Context, path storage.Path, w io.Writer) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("veracrypt: nil read destination")
	}
	p, err := r.safePath(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return mapError(r.fs.ReadFile(p, w))
}

func (r *Root) OpenRead(ctx context.Context, path storage.Path) (io.ReadCloser, error) {
	m, err := r.Materialize(ctx, path)
	if err != nil {
		return nil, err
	}
	return &leaseReadCloser{Materialized: m}, nil
}

func (r *Root) Materialize(ctx context.Context, path storage.Path) (*storage.Materialized, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	p, err := r.safePath(path)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(r.scratch, ".scpart-")
	if err != nil {
		return nil, fmt.Errorf("veracrypt: create materialization: %w", err)
	}
	name := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(name) }
	r.mu.Lock()
	err = r.fs.ReadFile(p, f)
	r.mu.Unlock()
	if err != nil {
		cleanup()
		return nil, mapError(err)
	}
	st, err := f.Stat()
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return storage.NewMaterialized(f, st.Size(), func() error { return errors.Join(f.Close(), os.Remove(name)) }), nil
}

func (r *Root) CreatePart(ctx context.Context) (*Part, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(r.scratch, ".scpart-")
	if err != nil {
		return nil, fmt.Errorf("veracrypt: create part: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("veracrypt: unlink part: %w", err)
	}
	return &Part{file: f}, nil
}

func (r *Root) WriteDurable(ctx context.Context, path storage.Path, opt DurableOptions, src io.Reader) (Durable, error) {
	if err := contextErr(ctx); err != nil {
		return Durable{}, err
	}
	if src == nil {
		return Durable{}, fmt.Errorf("veracrypt: nil durable source")
	}
	p, err := r.safePath(path)
	if err != nil {
		return Durable{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced, err := r.fs.WriteFileStaged(p, src, opt.NoClobber, opt.MtimeNs)
	return Durable{Replaced: replaced}, mapError(err)
}

func (r *Root) PublishPart(ctx context.Context, part *Part, dest storage.Path, replacing bool) (Durable, error) {
	if err := contextErr(ctx); err != nil {
		return Durable{}, err
	}
	if part == nil || part.file == nil {
		return Durable{}, fmt.Errorf("veracrypt: nil part")
	}
	if _, err := part.file.Seek(0, io.SeekStart); err != nil {
		return Durable{}, err
	}
	p, err := r.safePath(dest)
	if err != nil {
		return Durable{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced, err := r.fs.WriteFileStaged(p, part.file, !replacing, time.Now().UnixNano())
	closeErr := part.Close()
	return Durable{Replaced: replaced}, errors.Join(mapError(err), closeErr)
}

func (r *Root) SetTimes(ctx context.Context, path storage.Path, mtimeNs int64) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	p, err := r.safePath(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return mapError(r.fs.SetModTime(p, mtimeNs))
}
func (r *Root) Mkdir(ctx context.Context, path storage.Path) error {
	return r.simplePath(ctx, path, func(p format.SafePath) error { return r.fs.Mkdir(p) })
}
func (r *Root) Rmdir(ctx context.Context, path storage.Path) error {
	return r.simplePath(ctx, path, func(p format.SafePath) error { return r.fs.Rmdir(p) })
}
func (r *Root) Unlink(ctx context.Context, path storage.Path) error {
	return r.simplePath(ctx, path, func(p format.SafePath) error { return r.fs.Remove(p) })
}
func (r *Root) Rename(ctx context.Context, from, to storage.Path) error {
	return r.rename(ctx, from, to, false)
}

// RenameNoReplace moves an entry without replacing an existing destination.
// The filesystem performs the existence check and rename under its own lock.
func (r *Root) RenameNoReplace(ctx context.Context, from, to storage.Path) error {
	return r.rename(ctx, from, to, true)
}

func (r *Root) rename(ctx context.Context, from, to storage.Path, noReplace bool) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	a, err := r.safePath(from)
	if err != nil {
		return err
	}
	b, err := r.safePath(to)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return mapError(r.fs.Rename(a, b, noReplace))
}
func (r *Root) simplePath(ctx context.Context, path storage.Path, fn func(format.SafePath) error) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	p, err := r.safePath(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return mapError(fn(p))
}
func (r *Root) Space(ctx context.Context, path storage.Path) (storage.Space, error) {
	if err := contextErr(ctx); err != nil {
		return storage.Space{}, err
	}
	if _, err := r.safePath(path); err != nil {
		return storage.Space{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	total, free := r.fs.Space()
	return storage.Space{Total: total, Free: free}, nil
}
func (r *Root) Health(ctx context.Context) storage.Health {
	if err := contextErr(ctx); err != nil {
		return storage.Health{Status: storage.HealthFailing, Err: err}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fs.Alive(); err != nil {
		return storage.Health{Status: storage.HealthFailing, Err: mapError(err)}
	}
	return storage.Health{Status: storage.HealthOK}
}
func (r *Root) Alive() error   { r.mu.Lock(); defer r.mu.Unlock(); return mapError(r.fs.Alive()) }
func (r *Root) Device() uint64 { return r.devID }
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(mapError(r.fs.Sync()), r.dev.Sync(), r.dev.Close())
}

func syntheticDevice(container string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(container))
	return h.Sum64() | (1 << 63)
}
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}
func bytesReader(b []byte) *byteReader { return &byteReader{b: b} }

type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

type leaseReadCloser struct{ *storage.Materialized }

func (l *leaseReadCloser) Close() error { return l.Release() }
