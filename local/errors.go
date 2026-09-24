//go:build linux

package local

import (
	"errors"
	"fmt"
	"syscall"
)

var (
	ErrNotFound            = errors.New("local: not found")
	ErrDenied              = errors.New("local: permission denied")
	ErrExists              = errors.New("local: already exists")
	ErrNotEmpty            = errors.New("local: directory not empty")
	ErrNoSpace             = errors.New("local: no space left on device")
	ErrCrossDevice         = errors.New("local: crosses a device boundary")
	ErrSymlinkDenied       = errors.New("local: symlink traversal denied")
	ErrIsDirectory         = errors.New("local: is a directory")
	ErrNotADirectory       = errors.New("local: not a directory")
	ErrInvalidPath         = errors.New("local: invalid path")
	ErrInvalidName         = errors.New("local: invalid path component")
	ErrResolverUnavailable = errors.New("local: the atomic path resolver is unavailable")
	ErrSandboxDenied       = errors.New("local: the sandbox does not grant this path")
)

func mapErrno(op string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	switch errno {
	case syscall.ENOENT, syscall.ENOTDIR:
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	case syscall.EACCES, syscall.EPERM:
		return fmt.Errorf("%s: %w", op, ErrDenied)
	case syscall.EEXIST:
		return fmt.Errorf("%s: %w", op, ErrExists)
	case syscall.ENOTEMPTY:
		return fmt.Errorf("%s: %w", op, ErrNotEmpty)
	case syscall.ENOSPC:
		return fmt.Errorf("%s: %w", op, ErrNoSpace)
	case syscall.EXDEV:
		return fmt.Errorf("%s: %w", op, ErrCrossDevice)
	case syscall.ELOOP:
		return fmt.Errorf("%s: %w", op, ErrSymlinkDenied)
	case syscall.EISDIR:
		return fmt.Errorf("%s: %w", op, ErrIsDirectory)
	default:
		return fmt.Errorf("%s: %w", op, errno)
	}
}

func isMissing(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR)
}

// ResolverError describes why openat2 cannot provide descriptor confinement.
type ResolverError struct {
	Support Support
	Kernel  string
}

func (e *ResolverError) Error() string {
	switch e.Support {
	case SupportMissing:
		return fmt.Sprintf("local: kernel %s has no openat2; Upgrade the kernel. There is no fallback for confined path resolution.", e.Kernel)
	case SupportBlocked:
		return fmt.Sprintf("local: kernel %s has openat2 but a sandbox profile blocked it. Allow openat2 in the profile. There is no fallback for confined path resolution.", e.Kernel)
	case SupportDenied:
		return fmt.Sprintf("local: kernel %s has openat2 but the running uid lacks permission to search the probe path. Check the uid. There is no fallback for confined path resolution.", e.Kernel)
	default:
		return fmt.Sprintf("local: openat2 is not usable on kernel %s. There is no fallback for confined path resolution.", e.Kernel)
	}
}
func (e *ResolverError) Is(target error) bool { return target == ErrResolverUnavailable }
