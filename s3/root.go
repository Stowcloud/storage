package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stowcloud/storage"
)

// Options configures one public S3 backend. Secret is the secret access key;
// it is copied on Open and never retained in Config.
type Options struct {
	Config     Config
	Secret     string
	ScratchDir string
	Client     *http.Client
}

// Root serves one S3-compatible bucket through storage's portable contracts.
type Root struct {
	cfg            Config
	http           *http.Client
	endpointScheme string
	endpointHost   string
	client         *s3.Client
	presign        *s3.PresignClient
	scratch        string
	ownScratch     bool
	mu             sync.Mutex
}

var _ storage.ReadHierarchy = (*Root)(nil)
var _ storage.Materializer = (*Root)(nil)
var _ storage.Renamer = (*Root)(nil)

// Open validates options, creates scratch space, and probes the configured bucket.
func Open(ctx context.Context, opt Options) (*Root, error) {
	if opt.Config.Bucket == "" {
		return nil, errors.New("s3: open: bucket is required")
	}
	if err := validateEndpoint(opt.Config.Endpoint); err != nil {
		return nil, err
	}
	if !validRegion(opt.Config.Region) {
		return nil, errors.New("s3: open: invalid region")
	}
	endpoint, err := url.Parse(opt.Config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: open endpoint: %w", err)
	}
	scratch := opt.ScratchDir
	own := false
	if scratch == "" {
		scratch, err = os.MkdirTemp("", "stowcloud-s3-")
		if err != nil {
			return nil, fmt.Errorf("s3: create scratch: %w", err)
		}
		own = true
	} else if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("s3: create scratch: %w", err)
	}
	client := opt.Client
	if client == nil {
		client = defaultHTTPClient()
	}
	root := &Root{cfg: opt.Config, http: client, endpointScheme: endpoint.Scheme, endpointHost: endpoint.Host, scratch: scratch, ownScratch: own}
	if err := root.ensureSDK(opt.Secret); err != nil {
		root.Close()
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, metadataRequestTimeout)
	defer cancel()
	if _, err := root.listObjects(probeCtx, root.cfg.Prefix, "", "", 0); err != nil {
		root.Close()
		return nil, fmt.Errorf("s3: open %s: %w", opt.Config.Describe(), err)
	}
	return root, nil
}

func (r *Root) ensureSDK(secret string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return nil
	}
	if r.cfg.AccessKey == "" || secret == "" {
		return errors.New("s3: credentials are required")
	}
	cfg, err := sdkconfig.LoadDefaultConfig(context.Background(), sdkconfig.WithRegion(r.cfg.Region), sdkconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(r.cfg.AccessKey, secret, "")), sdkconfig.WithHTTPClient(r.http))
	if err != nil {
		return fmt.Errorf("s3: load client config: %w", err)
	}
	r.client = s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(r.cfg.Endpoint); o.UsePathStyle = r.cfg.PathStyle })
	r.presign = s3.NewPresignClient(r.client)
	return nil
}

func (r *Root) Close() error {
	if r == nil || !r.ownScratch {
		return nil
	}
	return os.RemoveAll(r.scratch)
}

func (r *Root) Config() Config { return r.cfg }
func (r *Root) ObjectKey(path storage.Path) string {
	if path.IsRoot() {
		return r.cfg.Prefix
	}
	if r.cfg.Prefix == "" {
		return path.String()
	}
	return r.cfg.Prefix + "/" + path.String()
}
func (r *Root) DirectoryPrefix(path storage.Path) string {
	key := r.ObjectKey(path)
	if key == "" {
		return ""
	}
	return key + "/"
}
func (r *Root) requestTarget(key string) (scheme, host, path string) {
	scheme = r.endpointScheme
	if r.cfg.PathStyle {
		host = r.endpointHost
		path = "/" + r.cfg.Bucket
		if key != "" {
			path += "/" + key
		}
		return
	}
	host = r.cfg.Bucket + "." + r.endpointHost
	if key == "" {
		return scheme, host, "/"
	}
	return scheme, host, "/" + key
}

func (r *Root) materializeFile() (*os.File, string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, "", fmt.Errorf("s3: mint scratch name: %w", err)
	}
	name := filepath.Join(r.scratch, ".scpart-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("s3: create scratch file: %w", err)
	}
	return f, name, nil
}

func (r *Root) Materialize(ctx context.Context, path storage.Path) (*storage.Materialized, error) {
	f, name, err := r.materializeFile()
	if err != nil {
		return nil, err
	}
	n, err := r.getObjectTo(ctx, r.ObjectKey(path), f)
	if err != nil {
		return nil, errors.Join(err, f.Close(), os.Remove(name))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, f.Close(), os.Remove(name))
	}
	return storage.NewMaterialized(f, n, func() error { return errors.Join(f.Close(), os.Remove(name)) }), nil
}

func (r *Root) OpenRead(ctx context.Context, path storage.Path) (io.ReadCloser, error) {
	m, err := r.Materialize(ctx, path)
	if err != nil {
		return nil, err
	}
	return &materializedReadCloser{Materialized: m}, nil
}

type materializedReadCloser struct{ *storage.Materialized }

func (m *materializedReadCloser) Close() error { return m.Release() }

func syntheticIno(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

func (r *Root) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if path.IsRoot() {
		return storage.Entry{Path: path, Kind: storage.KindDirectory}, nil
	}
	found, size, mt, err := r.headObject(ctx, r.ObjectKey(path))
	if err != nil {
		return storage.Entry{}, err
	}
	if found {
		return storage.Entry{Path: path, Kind: storage.KindFile, Size: size, ModTime: mt}, nil
	}
	isDir, mt, err := r.isDirectory(ctx, r.ObjectKey(path))
	if err != nil {
		return storage.Entry{}, err
	}
	if !isDir {
		return storage.Entry{}, fmt.Errorf("s3: stat: %w", ErrNotFound)
	}
	return storage.Entry{Path: path, Kind: storage.KindDirectory, ModTime: mt}, nil
}

func (r *Root) ReadDir(ctx context.Context, path storage.Path) ([]storage.Entry, error) {
	dirKey := r.DirectoryPrefix(path)
	token := ""
	out := make([]storage.Entry, 0, 64)
	for {
		result, err := r.listObjects(ctx, dirKey, "/", token, 1000)
		if err != nil {
			return nil, err
		}
		for _, p := range result.CommonPrefixes {
			rest, err := listedRemainder(p, dirKey)
			if err != nil {
				return nil, err
			}
			name := strings.TrimSuffix(rest, "/")
			if name == "" {
				continue
			}
			child, err := path.Join(name)
			if err != nil {
				return nil, fmt.Errorf("s3: invalid listed directory: %w", err)
			}
			out = append(out, storage.Entry{Path: child, Kind: storage.KindDirectory})
			if len(out) > maxListEntries {
				return nil, errors.New("s3: directory exceeds entry limit")
			}
		}
		for _, o := range result.Contents {
			rest, err := listedRemainder(o.Key, dirKey)
			if err != nil {
				return nil, err
			}
			if rest == "" || strings.HasSuffix(rest, "/") {
				continue
			}
			child, err := path.Join(rest)
			if err != nil {
				return nil, fmt.Errorf("s3: invalid listed file: %w", err)
			}
			mt, err := parseListedTime(o.LastModified)
			if err != nil {
				return nil, fmt.Errorf("s3: invalid LastModified for %q: %w", o.Key, err)
			}
			out = append(out, storage.Entry{Path: child, Kind: storage.KindFile, Size: int64(o.Size), ModTime: mt})
			if len(out) > maxListEntries {
				return nil, errors.New("s3: directory exceeds entry limit")
			}
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return out, nil
		}
		token = result.NextContinuationToken
	}
}

func listedRemainder(key, prefix string) (string, error) {
	if !strings.HasPrefix(key, prefix) {
		return "", errors.New("s3: server returned key outside requested prefix")
	}
	rest := strings.TrimPrefix(key, prefix)
	if rest == "" {
		return "", nil
	}
	if len(rest) > maxObjectKeyBytes {
		return "", errors.New("s3: listed key is too long")
	}
	if _, err := storage.ParsePath(strings.TrimSuffix(rest, "/")); err != nil {
		return "", fmt.Errorf("s3: invalid listed key: %w", err)
	}
	return rest, nil
}

func parseListedTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errors.New("missing LastModified")
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func (r *Root) Space(ctx context.Context, _ storage.Path) (storage.Space, error) {
	if err := ctx.Err(); err != nil {
		return storage.Space{}, err
	}
	return spaceCapacity(r.scratch)
}

func (r *Root) Health(ctx context.Context) storage.Health {
	if err := r.Alive(ctx); err != nil {
		return storage.Health{Status: storage.HealthFailing, Err: err}
	}
	return storage.Health{Status: storage.HealthOK}
}
func (r *Root) Alive(ctx context.Context) error {
	_, err := r.listObjects(ctx, r.cfg.Prefix, "", "", 0)
	return err
}
func (r *Root) Watch(context.Context, storage.Path) (<-chan storage.Event, error) {
	return nil, errors.New("s3: watch unsupported")
}

// MultipartPart is a provider-neutral observation of an S3 multipart part.
type MultipartPart struct {
	PartNumber int
	ETag       string
	Size       int64
	Checksum   string
}

// TransferReceipt is the provider's durable publication observation.
type TransferReceipt struct {
	Size     uint64
	ETag     string
	Checksum string
}

func (r *Root) Rename(ctx context.Context, from, to storage.Path) error {
	return r.rename(ctx, from, to, false)
}
func (r *Root) RenameNoReplace(ctx context.Context, from, to storage.Path) error {
	return r.rename(ctx, from, to, true)
}

// DirectTransfer reports whether credentials are available for provider-side transfer.
func (r *Root) DirectTransfer() bool { return r != nil && r.client != nil && r.cfg.AccessKey != "" }

// PutObject uploads a complete object from a reader.
func (r *Root) PutObject(ctx context.Context, path storage.Path, body io.Reader, size int64) error {
	return r.putReader(ctx, r.ObjectKey(path), body, size)
}

// PutObjectNoClobber creates an object only when the destination is absent.
// Providers report a precondition failure as ErrExists.
func (r *Root) PutObjectNoClobber(ctx context.Context, path storage.Path, body io.Reader, size int64) error {
	return r.putReaderConditional(ctx, r.ObjectKey(path), body, size, "*")
}

func (r *Root) Mkdir(ctx context.Context, path storage.Path) error {
	if path.IsRoot() {
		return fmt.Errorf("s3: mkdir: %w", ErrExists)
	}
	found, _, _, err := r.headObject(ctx, r.DirectoryPrefix(path))
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("s3: mkdir: %w", ErrExists)
	}
	return r.putEmpty(ctx, r.DirectoryPrefix(path))
}
func (r *Root) Rmdir(ctx context.Context, path storage.Path) error {
	if path.IsRoot() {
		return errors.New("s3: rmdir: denied")
	}
	prefix := r.DirectoryPrefix(path)
	result, err := r.listObjects(ctx, prefix, "/", "", 2)
	if err != nil {
		return err
	}
	marker := false
	for _, o := range result.Contents {
		if o.Key == prefix {
			marker = true
		} else {
			return fmt.Errorf("s3: rmdir: %w", ErrNotEmpty)
		}
	}
	if len(result.CommonPrefixes) > 0 {
		return fmt.Errorf("s3: rmdir: %w", ErrNotEmpty)
	}
	if !marker {
		return fmt.Errorf("s3: rmdir: %w", ErrNotFound)
	}
	return r.deleteObject(ctx, prefix)
}
func (r *Root) Unlink(ctx context.Context, path storage.Path) error {
	if path.IsRoot() {
		return errors.New("s3: unlink: denied")
	}
	key := r.ObjectKey(path)
	found, _, _, err := r.headObject(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		if dir, _, e := r.isDirectory(ctx, key); e != nil {
			return e
		} else if dir {
			return errors.New("s3: unlink: is a directory")
		}
		return fmt.Errorf("s3: unlink: %w", ErrNotFound)
	}
	return r.deleteObject(ctx, key)
}

type listObject struct {
	Key, LastModified, ETag string
	Size                    uint64
}
type listResult struct {
	IsTruncated           bool
	NextContinuationToken string
	Contents              []listObject
	CommonPrefixes        []string
}
