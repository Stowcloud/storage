//go:build linux

package local

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func (r *RootHandle) CreatePart(p Path) (*File, error) {
	if p.IsRoot() || !IsReservedName(p.Name()) {
		return nil, fmt.Errorf("create part: %w", ErrDenied)
	}
	parent, e := r.resolveDir(p.Parent().comps)
	if e != nil {
		return nil, e
	}
	defer parent.Close()
	f, e := openat2Raw(parent, p.Name(), unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint64(r.policy.ModeFile), resolveFlags(r.policy))
	if e != nil {
		return nil, mapErrno("create part", e)
	}
	h := &File{f: f}
	if e = h.SetMode(r.policy.ModeFile); e != nil {
		_ = f.Close()
		return nil, e
	}
	return h, nil
}
