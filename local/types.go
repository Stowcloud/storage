//go:build linux

// Package local provides descriptor-confined Linux filesystem storage.
//
// Every operation starts from an explicitly opened root descriptor and uses
// openat2 for path resolution. No operation joins a caller path to the host
// root after construction.
package local

import (
	"fmt"
	"strings"
)

type Kind uint8

const (
	KindOther Kind = iota
	KindFile
	KindDir
	KindSymlink
)

func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "dir"
	case KindSymlink:
		return "symlink"
	default:
		return "other"
	}
}
func (k Kind) IsDir() bool { return k == KindDir }

type Stat struct {
	Dev     uint64
	Ino     uint64
	BtimeNs *int64
	MtimeNs int64
	CtimeNs *int64
	Size    uint64
	Mode    uint32
	UID     uint32
	GID     uint32
	Nlink   uint32
	Kind    Kind
}

type SymlinkPolicy uint8

const (
	SymlinkDeny SymlinkPolicy = iota
	SymlinkWithinRoot
	SymlinkFollow
)

func (p SymlinkPolicy) String() string {
	switch p {
	case SymlinkWithinRoot:
		return "within_root"
	case SymlinkFollow:
		return "follow"
	default:
		return "deny"
	}
}
func ParseSymlinkPolicy(s string) (SymlinkPolicy, error) {
	switch s {
	case "deny":
		return SymlinkDeny, nil
	case "within_root":
		return SymlinkWithinRoot, nil
	case "follow":
		return SymlinkFollow, nil
	default:
		return 0, fmt.Errorf("local: unknown symlink policy %q", s)
	}
}

type Owner struct{ UID, GID uint32 }

type Policy struct {
	Symlink    SymlinkPolicy
	CrossMount bool
	ModeFile   uint32
	ModeDir    uint32
	Chown      *Owner
}

func DefaultPolicy() Policy {
	return Policy{Symlink: SymlinkDeny, CrossMount: true, ModeFile: 0o664, ModeDir: 0o775}
}

type ReservedPolicy uint8

const (
	HideReserved ReservedPolicy = iota
	IncludeReserved
)

type AccessIntent uint8

const (
	IntentRead AccessIntent = iota
	IntentReadWrite
)

type FsSpace struct{ Total, Free, Available uint64 }

func (s FsSpace) Used() uint64 {
	if s.Free > s.Total {
		return 0
	}
	return s.Total - s.Free
}

type FsType uint64

const (
	FsExt4     FsType = 0xEF53
	FsBtrfs    FsType = 0x9123683E
	FsXfs      FsType = 0x58465342
	FsZfs      FsType = 0x2FC12FC1
	FsF2fs     FsType = 0xF2F52010
	FsTmpfs    FsType = 0x01021994
	FsOverlay  FsType = 0x794C7630
	FsFuse     FsType = 0x65735546
	FsNfs      FsType = 0x6969
	FsCifs     FsType = 0xFF534D42
	FsSmb2     FsType = 0xFE534D42
	FsSquashfs FsType = 0x73717368
	FsNtfs     FsType = 0x5346544E
)

func (t FsType) String() string {
	switch t {
	case FsExt4:
		return "ext4"
	case FsBtrfs:
		return "btrfs"
	case FsXfs:
		return "xfs"
	case FsZfs:
		return "zfs"
	case FsF2fs:
		return "f2fs"
	case FsTmpfs:
		return "tmpfs"
	case FsOverlay:
		return "overlay"
	case FsFuse:
		return "fuse"
	case FsNfs:
		return "nfs"
	case FsCifs, FsSmb2:
		return "cifs"
	case FsSquashfs:
		return "squashfs"
	case FsNtfs:
		return "ntfs"
	default:
		return "unknown"
	}
}
func ParseFsType(name string) FsType {
	switch strings.ToLower(name) {
	case "ext4":
		return FsExt4
	case "btrfs":
		return FsBtrfs
	case "xfs":
		return FsXfs
	case "zfs":
		return FsZfs
	case "f2fs":
		return FsF2fs
	case "tmpfs":
		return FsTmpfs
	case "overlay":
		return FsOverlay
	case "fuse":
		return FsFuse
	case "nfs":
		return FsNfs
	case "cifs":
		return FsCifs
	case "squashfs":
		return FsSquashfs
	case "ntfs":
		return FsNtfs
	default:
		return 0
	}
}

type DirEntry struct {
	Name string
	Kind Kind
	Ino  uint64
}

type Support uint8

const (
	SupportUnknown Support = iota
	SupportPresent
	SupportMissing
	SupportBlocked
	SupportDenied
)

func (s Support) String() string {
	switch s {
	case SupportPresent:
		return "present"
	case SupportMissing:
		return "missing, this kernel does not implement it"
	case SupportBlocked:
		return "blocked, a policy refused it"
	case SupportDenied:
		return "denied, the filesystem refused the operand"
	default:
		return "unknown"
	}
}

type Caps struct {
	Kernel                                        string
	Openat2, StatxBtime, Renameat2, CopyFileRange Support
}

type Admission struct {
	OK      bool
	Warn    string
	Reflink bool
}
type AdmissionError struct {
	Path   string
	Type   FsType
	Reason string
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("local: %s is on %s, which is not supported: %s", e.Path, e.Type, e.Reason)
}
func (e *AdmissionError) Is(target error) bool { return target == ErrUnsupportedFilesystem }

var ErrUnsupportedFilesystem = fmt.Errorf("local: filesystem is not supported")

func AdmitFsType(t FsType) (Admission, string) {
	switch t {
	case FsExt4, FsZfs, FsF2fs:
		return Admission{OK: true}, ""
	case FsBtrfs, FsXfs:
		return Admission{OK: true, Reflink: true}, ""
	case FsTmpfs:
		return Admission{OK: true, Warn: "everything on this root is lost when the machine restarts"}, ""
	case FsOverlay:
		return Admission{}, "a container writable layer does not provide restart-stable identity"
	case FsFuse:
		return Admission{}, "identity cannot be proven to survive restart or remount"
	case FsNfs:
		return Admission{}, "identity cannot be proven to survive remount"
	case FsCifs, FsSmb2:
		return Admission{}, "identity and remote name rules are not established"
	case FsSquashfs:
		return Admission{}, "read-only roots are not supported"
	case FsNtfs:
		return Admission{}, "identity, name and notification behavior are not established"
	default:
		return Admission{}, "this filesystem type is unknown"
	}
}
func AdmitMount(path string, t FsType, hasBtime bool) (Admission, error) {
	a, reason := AdmitFsType(t)
	if !a.OK {
		return Admission{}, &AdmissionError{Path: path, Type: t, Reason: reason}
	}
	if !hasBtime {
		return Admission{}, &AdmissionError{Path: path, Type: t, Reason: "this mount reports no birth time"}
	}
	return a, nil
}

type DurableOpts struct {
	Mode      uint32
	Owner     *Owner
	NoClobber bool
}
type Durable struct {
	Replaced     bool
	OwnerRestore error
}

type Root interface {
	Stat(Path) (Stat, error)
	ReadDir(Path, ReservedPolicy) ([]DirEntry, error)
	ReadDirFunc(Path, ReservedPolicy, func(DirEntry) bool) error
	OpenRead(Path, AccessIntent) (*File, error)
	CreatePart(Path) (*File, error)
	WriteDurable(Path, DurableOpts, func(*File) error) (Durable, error)
	PublishPart(part, dest Path, replacing bool) (Durable, error)
	SetTimes(Path, int64) error
	Mkdir(Path) error
	Rmdir(Path) error
	Unlink(Path) error
	Rename(from, to Path, noReplace bool) error
	Space(Path) (FsSpace, error)
	DirDev(Path) (uint64, error)
	Policy() Policy
	Dev() uint64
	FsType() FsType
	HasBtime() bool
	IsScratch() bool
	Alive() error
	Close() error
}
