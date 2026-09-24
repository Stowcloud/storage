//go:build linux

package local

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	MaxPathBytes      = 4 << 10
	MaxPathComponents = 256
	MaxNameBytes      = 255
)

// Path is a hierarchy-relative path suitable for Linux descriptor resolution.
// Unlike storage.Path it deliberately permits backslashes and non-UTF-8 bytes,
// because an existing POSIX entry must remain addressable by the product VFS.
type Path struct{ comps []string }

func RootPath() Path { return Path{} }
func ParsePath(raw string) (Path, error) {
	if len(raw) > MaxPathBytes {
		return Path{}, fmt.Errorf("%w: path exceeds %d bytes", ErrInvalidPath, MaxPathBytes)
	}
	if raw == "" {
		return Path{}, nil
	}
	if strings.HasPrefix(raw, "/") {
		return Path{}, fmt.Errorf("%w: absolute path", ErrInvalidPath)
	}
	parts := strings.Split(raw, "/")
	if len(parts) > MaxPathComponents {
		return Path{}, fmt.Errorf("%w: too many components", ErrInvalidPath)
	}
	for _, c := range parts {
		if err := validateComponent(c); err != nil {
			return Path{}, err
		}
	}
	return Path{comps: parts}, nil
}
func validateComponent(c string) error {
	if c == "" || c == "." || c == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidName, c)
	}
	if strings.IndexByte(c, 0) >= 0 {
		return fmt.Errorf("%w: NUL", ErrInvalidName)
	}
	if len(c) > MaxNameBytes {
		return fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidName, MaxNameBytes)
	}
	return nil
}
func (p Path) String() string { return strings.Join(p.comps, "/") }
func (p Path) Components() []string {
	out := make([]string, len(p.comps))
	copy(out, p.comps)
	return out
}
func (p Path) IsRoot() bool { return len(p.comps) == 0 }
func (p Path) Name() string {
	if p.IsRoot() {
		return ""
	}
	return p.comps[len(p.comps)-1]
}
func (p Path) Parent() Path {
	if p.IsRoot() {
		return p
	}
	out := make([]string, len(p.comps)-1)
	copy(out, p.comps[:len(p.comps)-1])
	return Path{comps: out}
}
func (p Path) Equal(q Path) bool {
	if len(p.comps) != len(q.comps) {
		return false
	}
	for i := range p.comps {
		if p.comps[i] != q.comps[i] {
			return false
		}
	}
	return true
}
func (p Path) Join(name string) (Path, error) {
	if err := validateComponent(name); err != nil {
		return Path{}, err
	}
	if len(p.comps)+1 > MaxPathComponents {
		return Path{}, fmt.Errorf("%w: too many components", ErrInvalidPath)
	}
	q := Path{comps: make([]string, len(p.comps)+1)}
	copy(q.comps, p.comps)
	q.comps[len(p.comps)] = name
	if len(q.String()) > MaxPathBytes {
		return Path{}, fmt.Errorf("%w: path exceeds %d bytes", ErrInvalidPath, MaxPathBytes)
	}
	return q, nil
}

func lookupCandidates(name string) []string {
	// POSIX names are byte strings. No Unicode normalization is applied here;
	// that policy belongs to Stowcloud's VFS path layer.
	return []string{name}
}
func pathCandidates(comps []string) []string {
	if len(comps) == 0 {
		return []string{"."}
	}
	return []string{strings.Join(comps, "/")}
}

const defaultStagingPrefix = ".scpart-"

func stagingName(prefix string) (string, error) {
	if prefix == "" {
		prefix = defaultStagingPrefix
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("local: mint staging name: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func IsStagingName(name string) bool {
	return strings.HasPrefix(name, defaultStagingPrefix) && len(name) > len(defaultStagingPrefix)
}

var errNoPath = errors.New("local: empty path")
