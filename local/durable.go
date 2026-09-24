//go:build linux

package local

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func (r *RootHandle) WriteDurable(p Path, opt DurableOpts, write func(*File) error) (done Durable, err error) {
	if p.IsRoot() {
		return done, fmt.Errorf("durable write: %w", ErrDenied)
	}
	name, e := stagingName("")
	if e != nil {
		return done, e
	}
	parent, e := r.openLeaf(p.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if e != nil {
		return done, e
	}
	defer parent.Close()
	prior, priorName, replacing, e := r.priorEntry(parent, p.Name())
	if e != nil {
		return done, e
	}
	if replacing && opt.NoClobber {
		return done, fmt.Errorf("durable write: %w", ErrExists)
	}
	f, e := openat2Raw(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0, resolveFlags(r.policy))
	if e != nil {
		return done, mapErrno("create staging file", e)
	}
	stagePath := p.Parent().String()
	if stagePath != "" {
		stagePath += "/"
	}
	stagePath += name
	r.markInflight(stagePath)
	defer r.unmarkInflight(stagePath)
	h := &File{f: f}
	published := false
	defer func() {
		_ = f.Close()
		if !published {
			if uerr := withFdErr(parent, func(fd int) error { return unix.Unlinkat(fd, name, 0) }); uerr == nil {
				if serr := syncDirFd(parent); err == nil && serr != nil {
					err = serr
				}
			} else if err == nil {
				err = mapErrno("remove staging file", uerr)
			}
		}
	}()
	// Staging stays private while the callback writes. Final metadata is
	// applied only after the callback succeeds, immediately before the inode sync.
	if write != nil {
		if e = write(h); e != nil {
			return done, e
		}
	}
	dest := p.Name()
	if replacing {
		done.Replaced = true
		dest = priorName
		if e = h.SetMode(prior.Mode & 0o7777); e != nil {
			return done, e
		}
		if e = h.SetOwner(Owner{UID: prior.UID, GID: prior.GID}); e != nil {
			return done, e
		}
	} else {
		if e = h.SetMode(opt.Mode); e != nil {
			return done, e
		}
		if opt.Owner != nil {
			if e = h.SetOwner(*opt.Owner); e != nil {
				return done, e
			}
		}
	}
	if e = syncFileFd(f); e != nil {
		return done, e
	}
	flags := uint(0)
	if opt.NoClobber {
		flags = unix.RENAME_NOREPLACE
	}
	if e = renameWithin(parent, name, dest, flags); e != nil {
		return done, mapErrno("publish", e)
	}
	published = true
	if e = syncDirFd(parent); e != nil {
		return done, e
	}
	return done, nil
}
func (r *RootHandle) PublishPart(part, dest Path, replacing bool) (done Durable, err error) {
	if part.IsRoot() || dest.IsRoot() {
		return done, fmt.Errorf("publish part: %w", ErrDenied)
	}
	if !part.Parent().Equal(dest.Parent()) {
		return done, fmt.Errorf("publish part across directories: %w", ErrDenied)
	}
	dir, e := r.openLeaf(dest.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if e != nil {
		return done, e
	}
	defer dir.Close()
	prior, priorName, occupied, e := r.priorEntry(dir, dest.Name())
	if e != nil {
		return done, e
	}
	if occupied && !replacing {
		return done, fmt.Errorf("publish part: %w", ErrExists)
	}
	name := dest.Name()
	flags := uint(unix.RENAME_NOREPLACE)
	if occupied {
		f, oerr := openat2Raw(dir, part.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0, resolveFlags(r.policy))
		if oerr != nil {
			return done, mapErrno("reopen part file", oerr)
		}
		h := &File{f: f}
		merr := h.SetMode(prior.Mode & 0o7777)
		if merr == nil {
			merr = h.SetOwner(Owner{UID: prior.UID, GID: prior.GID})
		}
		if merr == nil {
			merr = syncFileFd(f)
		}
		_ = f.Close()
		if merr != nil {
			return done, merr
		}
		done.Replaced = true
		name = priorName
		flags = 0
	}
	if e = renameWithin(dir, part.Name(), name, flags); e != nil {
		return done, mapErrno("publish", e)
	}
	if e = syncDirFd(dir); e != nil {
		return done, e
	}
	return done, nil
}
func (r *RootHandle) priorEntry(dir *os.File, leaf string) (Stat, string, bool, error) {
	for _, cand := range lookupCandidates(leaf) {
		f, e := openat2Raw(dir, cand, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0, resolveFlags(r.policy))
		if e != nil {
			if isMissing(e) {
				continue
			}
			return Stat{}, "", false, mapErrno("stat entry being replaced", e)
		}
		st, se := statOf(f)
		_ = f.Close()
		if se != nil {
			return Stat{}, "", false, se
		}
		return st, cand, true, nil
	}
	return Stat{}, "", false, nil
}
func renameWithin(dir *os.File, from, to string, flags uint) error {
	err := withFdErr(dir, func(fd int) error { return unix.Renameat2(fd, from, fd, to, flags) })
	if errors.Is(err, syscall.ENOSYS) && flags == 0 {
		return withFdErr(dir, func(fd int) error { return unix.Renameat(fd, from, fd, to) })
	}
	return err
}
func syncFileFd(f *os.File) error {
	if err := withFdErr(f, unix.Fsync); err != nil {
		return mapErrno("sync file", err)
	}
	return nil
}

func syncDirFd(d *os.File) error {
	if err := withFdErr(d, unix.Fsync); err != nil {
		return mapErrno("sync directory", err)
	}
	return nil
}
func (r *RootHandle) Mkdir(p Path) error {
	if p.IsRoot() {
		return fmt.Errorf("mkdir: %w", ErrExists)
	}
	_, leaf := splitLeaf(p)
	parent, e := r.openLeaf(p.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if e != nil {
		return e
	}
	defer parent.Close()
	if e = withFdErr(parent, func(fd int) error { return unix.Mkdirat(fd, leaf, r.policy.ModeDir) }); e != nil {
		return mapErrno("mkdir", e)
	}
	created, e := openat2Raw(parent, leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0, resolveFlags(r.policy))
	if e != nil {
		return mapErrno("reopen created directory", e)
	}
	defer created.Close()
	if e = withFdErr(created, func(fd int) error { return unix.Fchmod(fd, r.policy.ModeDir) }); e != nil {
		return mapErrno("apply directory mode", e)
	}
	if r.policy.Chown != nil {
		if e = chownFd(created, *r.policy.Chown); e != nil {
			return e
		}
	}
	if e = syncDirFd(created); e != nil {
		return e
	}
	if e = syncDirFd(parent); e != nil {
		return e
	}
	return nil
}
func chownFd(f *os.File, o Owner) error {
	if err := withFdErr(f, func(fd int) error { return unix.Fchown(fd, int(o.UID), int(o.GID)) }); err != nil {
		return mapErrno("apply ownership", err)
	}
	return nil
}
func (r *RootHandle) Rename(from, to Path, noReplace bool) error {
	if from.IsRoot() || to.IsRoot() {
		return fmt.Errorf("rename: %w", ErrDenied)
	}
	_, fl := splitLeaf(from)
	_, tl := splitLeaf(to)
	fd, e := r.openLeaf(from.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if e != nil {
		return e
	}
	defer fd.Close()
	td, e := r.openLeaf(to.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if e != nil {
		return e
	}
	defer td.Close()
	flags := uint(0)
	if noReplace {
		flags = unix.RENAME_NOREPLACE
	}
	if e = withFd2Err(fd, td, func(f, t int) error { return unix.Renameat2(f, fl, t, tl, flags) }); e != nil {
		return mapErrno("rename", e)
	}
	if e = syncDirFd(fd); e != nil {
		return e
	}
	if !from.Parent().Equal(to.Parent()) {
		if e = syncDirFd(td); e != nil {
			return e
		}
	}
	return nil
}
func (r *RootHandle) Unlink(p Path) error { return r.unlinkAt(p, 0, "unlink") }
func (r *RootHandle) Rmdir(p Path) error  { return r.unlinkAt(p, unix.AT_REMOVEDIR, "rmdir") }
func (r *RootHandle) unlinkAt(p Path, flags int, op string) error {
	if p.IsRoot() {
		return fmt.Errorf("%s: %w", op, ErrDenied)
	}
	_, leaf := splitLeaf(p)
	d, err := r.openLeaf(p.Parent(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = withFdErr(d, func(fd int) error { return unix.Unlinkat(fd, leaf, flags) }); err != nil {
		return mapErrno(op, err)
	}
	if err = syncDirFd(d); err != nil {
		return err
	}
	return nil
}
