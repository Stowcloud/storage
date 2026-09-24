//go:build linux

package local

import "golang.org/x/sys/unix"

func (r *RootHandle) SetTimes(p Path, mtimeNs int64) error {
	if p.IsRoot() {
		return mapErrno("set times", ErrDenied)
	}
	parentComps, leaf := splitLeaf(p)
	parent, err := r.resolveDir(parentComps)
	if err != nil {
		return err
	}
	defer parent.Close()
	sec := mtimeNs / 1_000_000_000
	nsec := mtimeNs % 1_000_000_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	ts := []unix.Timespec{{Sec: 0, Nsec: unix.UTIME_OMIT}, {Sec: sec, Nsec: nsec}}
	for _, cand := range lookupCandidates(leaf) {
		err = withFdErr(parent, func(fd int) error { return unix.UtimesNanoAt(fd, cand, ts, unix.AT_SYMLINK_NOFOLLOW) })
		if err == nil {
			return nil
		}
		if !isMissing(err) {
			return mapErrno("set times", err)
		}
	}
	return mapErrno("set times", err)
}
