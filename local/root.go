//go:build linux

package local

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type RootHandle struct {
	anchor          *os.File
	policy          Policy
	dev, ino        uint64
	fsType          FsType
	hasBtime        bool
	scratch         bool
	protectReserved bool
	mu              sync.Mutex
	admitted        map[uint64]struct{}
	inflight        map[string]struct{}
	host            string
}

var _ Root = (*RootHandle)(nil)

func withFd[T any](f *os.File, fn func(int) (T, error)) (T, error) {
	v, err := fn(int(f.Fd()))
	runtime.KeepAlive(f)
	return v, err
}
func withFdErr(f *os.File, fn func(int) error) error {
	_, err := withFd(f, func(fd int) (struct{}, error) { return struct{}{}, fn(fd) })
	return err
}
func withFd2Err(a, b *os.File, fn func(int, int) error) error {
	_, err := withFd(a, func(x int) (struct{}, error) {
		return withFd(b, func(y int) (struct{}, error) { return struct{}{}, fn(x, y) })
	})
	return err
}
func closeFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}

func resolveFlags(p Policy) uint64 {
	flags := uint64(unix.RESOLVE_NO_MAGICLINKS)
	switch p.Symlink {
	case SymlinkDeny:
		flags |= unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS
	case SymlinkWithinRoot:
		flags |= unix.RESOLVE_IN_ROOT
	case SymlinkFollow:
		flags |= unix.RESOLVE_BENEATH
	}
	if !p.CrossMount {
		flags |= unix.RESOLVE_NO_XDEV
	}
	return flags
}
func openat2Raw(dir *os.File, path string, flags, mode, resolve uint64) (*os.File, error) {
	how := unix.OpenHow{Flags: flags, Mode: mode, Resolve: resolve}
	fd, err := withFd(dir, func(d int) (int, error) { return unix.Openat2(d, path, &how) })
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
func splitLeaf(p Path) ([]string, string) { return p.comps[:len(p.comps)-1], p.comps[len(p.comps)-1] }
func (r *RootHandle) resolveDirOrParent(p Path) (*os.File, error) {
	d, err := r.resolveDir(p.comps)
	if err == nil {
		return d, nil
	}
	if !p.IsRoot() && errors.Is(err, ErrNotFound) {
		return r.resolveDir(p.Parent().comps)
	}
	return nil, err
}

func OpenRoot(host string, policy Policy) (*RootHandle, error) {
	fd, err := unix.Open(host, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, mapErrno("open root", err)
	}
	anchor := os.NewFile(uintptr(fd), host)
	st, err := statOf(anchor)
	if err != nil {
		anchor.Close()
		return nil, err
	}
	var sfs unix.Statfs_t
	var typ FsType
	if e := withFdErr(anchor, func(fd int) error { return unix.Fstatfs(fd, &sfs) }); e == nil {
		typ = FsType(uint64(sfs.Type))
	}
	return &RootHandle{anchor: anchor, policy: policy, dev: st.Dev, ino: st.Ino, fsType: typ, hasBtime: st.BtimeNs != nil, admitted: map[uint64]struct{}{st.Dev: {}}, inflight: map[string]struct{}{}, host: host}, nil
}

// OpenRequestRoot opens the same confined root while additionally refusing
// descriptors whose resolved path crosses this package's reserved namespace.
// Trusted maintenance uses OpenRoot so it can still reach control files.
func OpenRequestRoot(host string, policy Policy) (*RootHandle, error) {
	r, err := OpenRoot(host, policy)
	if err != nil {
		return nil, err
	}
	r.protectReserved = true
	return r, nil
}
func RegisterRequestRoot(host string, policy Policy) (*RootHandle, Admission, error) {
	r, a, err := RegisterRoot(host, policy)
	if err != nil {
		return nil, Admission{}, err
	}
	r.protectReserved = true
	return r, a, nil
}
func RegisterRoot(host string, policy Policy) (*RootHandle, Admission, error) {
	r, err := OpenRoot(host, policy)
	if err != nil {
		return nil, Admission{}, err
	}
	a, err := AdmitMount(host, r.fsType, r.hasBtime)
	if err != nil {
		r.Close()
		return nil, Admission{}, err
	}
	if err := proveReadable(host); err != nil {
		r.Close()
		if errors.Is(err, ErrDenied) {
			return nil, Admission{}, fmt.Errorf("%w: %w", ErrSandboxDenied, err)
		}
		return nil, Admission{}, err
	}
	return r, a, nil
}
func OpenScratchRoot(host string, policy Policy) (*RootHandle, error) {
	r, _, err := RegisterRoot(host, policy)
	if err != nil {
		return nil, err
	}
	r.scratch = true
	return r, nil
}
func proveReadable(host string) error {
	f, err := os.Open(host)
	if err != nil {
		return mapErrno("read root", err)
	}
	return f.Close()
}
func (r *RootHandle) Close() error   { return r.anchor.Close() }
func (r *RootHandle) Policy() Policy { return r.policy }
func (r *RootHandle) Dev() uint64    { return r.dev }
func (r *RootHandle) resolveDir(comps []string) (*os.File, error) {
	var last error
	for _, cand := range pathCandidates(comps) {
		f, err := openat2Raw(r.anchor, cand, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0, resolveFlags(r.policy))
		if err == nil {
			if aerr := r.admitResolved(f); aerr != nil {
				_ = f.Close()
				return nil, aerr
			}
			if aerr := r.admitRequestPath(f, Path{comps: comps}); aerr != nil {
				_ = f.Close()
				return nil, aerr
			}
			return f, nil
		}
		if isMissing(err) {
			last = mapErrno("resolve directory", err)
			continue
		}
		return nil, mapErrno("resolve directory", err)
	}
	if last != nil {
		return nil, last
	}
	return nil, fmt.Errorf("resolve directory: %w", ErrNotFound)
}
func (r *RootHandle) openLeaf(p Path, flags uint64) (*os.File, error) {
	if p.IsRoot() {
		f, err := openat2Raw(r.anchor, ".", flags, 0, resolveFlags(r.policy))
		if err != nil {
			return nil, mapErrno("open root", err)
		}
		if err := r.admitResolved(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := r.admitRequestPath(f, p); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	parentComps, leaf := splitLeaf(p)
	parent, err := r.resolveDir(parentComps)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	var last error
	for _, cand := range lookupCandidates(leaf) {
		f, e := openat2Raw(parent, cand, flags, 0, resolveFlags(r.policy))
		if e == nil {
			if aerr := r.admitResolved(f); aerr != nil {
				_ = f.Close()
				return nil, aerr
			}
			if aerr := r.admitRequestPath(f, p); aerr != nil {
				_ = f.Close()
				return nil, aerr
			}
			return f, nil
		}
		if isMissing(e) {
			last = mapErrno("open", e)
			continue
		}
		return nil, mapErrno("open", e)
	}
	if last != nil {
		return nil, last
	}
	return nil, fmt.Errorf("open: %w", ErrNotFound)
}
func (r *RootHandle) FsType() FsType  { return r.fsType }
func (r *RootHandle) HasBtime() bool  { return r.hasBtime }
func (r *RootHandle) IsScratch() bool { return r.scratch }

func (r *RootHandle) markInflight(path string) {
	r.mu.Lock()
	r.inflight[path] = struct{}{}
	r.mu.Unlock()
}

func (r *RootHandle) unmarkInflight(path string) {
	r.mu.Lock()
	delete(r.inflight, path)
	r.mu.Unlock()
}

func (r *RootHandle) isInflight(path string) bool {
	r.mu.Lock()
	_, ok := r.inflight[path]
	r.mu.Unlock()
	return ok
}

func (r *RootHandle) admitRequestPath(f *os.File, requested Path) error {
	if !r.protectReserved {
		return nil
	}
	for _, comp := range requested.comps {
		if IsReservedName(comp) {
			return nil
		}
	}
	root, err := os.Readlink("/proc/self/fd/" + fmt.Sprint(r.anchor.Fd()))
	if err != nil {
		return mapErrno("inspect root path", err)
	}
	target, err := os.Readlink("/proc/self/fd/" + fmt.Sprint(f.Fd()))
	if err != nil {
		return mapErrno("inspect resolved path", err)
	}
	if target == root {
		return nil
	}
	prefix := root + "/"
	if !strings.HasPrefix(target, prefix) {
		return fmt.Errorf("resolved path outside root: %w", ErrDenied)
	}
	for _, comp := range strings.Split(strings.TrimPrefix(target, prefix), "/") {
		if IsReservedName(comp) {
			return fmt.Errorf("resolved reserved path: %w", ErrDenied)
		}
	}
	return nil
}
func (r *RootHandle) Alive() error {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, r.host, 0, unix.STATX_TYPE|unix.STATX_INO, &st); err != nil {
		return mapErrno("probe root", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("probe root: %w", ErrNotADirectory)
	}
	if unix.Mkdev(st.Dev_major, st.Dev_minor) != r.dev || st.Ino != r.ino {
		return fmt.Errorf("probe root: %w", ErrNotFound)
	}
	return nil
}
func (r *RootHandle) admitResolved(f *os.File) error {
	if !r.policy.CrossMount {
		return nil
	}
	st, err := statOf(f)
	if err != nil {
		return err
	}
	if st.Dev == r.dev {
		return nil
	}
	r.mu.Lock()
	_, ok := r.admitted[st.Dev]
	r.mu.Unlock()
	if ok {
		return nil
	}
	var sfs unix.Statfs_t
	if err := withFdErr(f, func(fd int) error { return unix.Fstatfs(fd, &sfs) }); err != nil {
		return mapErrno("statfs resolved path", err)
	}
	a, err := AdmitMount(f.Name(), FsType(uint64(sfs.Type)), st.BtimeNs != nil)
	if err != nil {
		return err
	}
	if !a.OK {
		return fmt.Errorf("%w: %s", ErrUnsupportedFilesystem, a.Warn)
	}
	r.mu.Lock()
	r.admitted[st.Dev] = struct{}{}
	r.mu.Unlock()
	return nil
}

func (r *RootHandle) ID() uint32 { return 0 }
