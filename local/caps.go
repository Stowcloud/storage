//go:build linux

package local

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/unix"
)

func Probe() Caps {
	return Caps{Kernel: kernelRelease(), Openat2: probeOpenat2(), StatxBtime: probeStatxBtime(), Renameat2: probeRenameat2(), CopyFileRange: probeCopyFileRange()}
}
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown"
	}
	return string(bytes.TrimRight(u.Release[:], "\x00"))
}
func classify(err error) Support {
	switch {
	case err == nil:
		return SupportPresent
	case errors.Is(err, unix.ENOSYS):
		return SupportMissing
	case errors.Is(err, unix.EPERM):
		return SupportBlocked
	case errors.Is(err, unix.EACCES):
		return SupportDenied
	default:
		return SupportPresent
	}
}
func probeOpenat2() Support {
	h := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS}
	fd, e := unix.Openat2(unix.AT_FDCWD, "/", &h)
	if e != nil {
		return classify(e)
	}
	if e = unix.Close(fd); e != nil {
		slog.Warn("local: closing openat2 probe failed", slog.Any("error", e))
	}
	return SupportPresent
}
func probeStatxBtime() Support {
	var st unix.Statx_t
	if e := unix.Statx(unix.AT_FDCWD, ".", 0, unix.STATX_BTIME, &st); e != nil {
		return classify(e)
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return SupportMissing
	}
	return SupportPresent
}
func probeRenameat2() Support {
	e := unix.Renameat2(unix.AT_FDCWD, ".", unix.AT_FDCWD, ".", unix.RENAME_NOREPLACE|unix.RENAME_EXCHANGE)
	if errors.Is(e, unix.EINVAL) {
		return SupportPresent
	}
	return classify(e)
}
func probeCopyFileRange() Support {
	_, e := unix.CopyFileRange(-1, nil, -1, nil, 0, 0)
	return classify(e)
}
func (c Caps) String() string {
	rows := []struct {
		name string
		s    Support
	}{{"kernel", SupportUnknown}, {"openat2", c.Openat2}, {"statx btime", c.StatxBtime}, {"renameat2", c.Renameat2}, {"copy_file_range", c.CopyFileRange}}
	lines := []string{fmt.Sprintf("%-15s %s", "kernel", c.Kernel)}
	for _, r := range rows[1:] {
		lines = append(lines, fmt.Sprintf("%-15s %s", r.name, r.s))
	}
	return strings.Join(lines, "\n")
}
func RequireResolver(c Caps) error {
	if c.Openat2 == SupportPresent {
		return nil
	}
	return &ResolverError{Support: c.Openat2, Kernel: c.Kernel}
}
