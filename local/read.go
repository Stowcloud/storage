//go:build linux

package local

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

type File struct{ f *os.File }

func WrapFile(f *os.File) *File {
	if f == nil {
		return nil
	}
	return &File{f: f}
}
func (f *File) Close() error                             { return f.f.Close() }
func (f *File) OSFile() *os.File                         { return f.f }
func (f *File) ReadAt(p []byte, off int64) (int, error)  { return f.f.ReadAt(p, off) }
func (f *File) WriteAt(p []byte, off int64) (int, error) { return f.f.WriteAt(p, off) }
func (f *File) Truncate(n int64) error                   { return f.f.Truncate(n) }
func (f *File) Stat() (Stat, error)                      { return statOf(f.f) }
func (f *File) Space() (FsSpace, error)                  { return spaceOf(f.f) }
func (f *File) SyncData() error {
	if err := withFdErr(f.f, unix.Fdatasync); err != nil {
		return mapErrno("sync file data", err)
	}
	return nil
}
func (f *File) SetMode(mode uint32) error {
	if err := withFdErr(f.f, func(fd int) error { return unix.Fchmod(fd, mode) }); err != nil {
		return mapErrno("apply mode", err)
	}
	return nil
}
func (f *File) SetOwner(o Owner) error { return chownFd(f.f, o) }

const statMask = unix.STATX_BASIC_STATS | unix.STATX_BTIME | unix.STATX_CTIME

func statOf(f *os.File) (Stat, error) {
	var st unix.Statx_t
	if err := withFdErr(f, func(fd int) error { return unix.Statx(fd, "", unix.AT_EMPTY_PATH, statMask, &st) }); err != nil {
		return Stat{}, mapErrno("statx", err)
	}
	return statxToStat(&st), nil
}
func statxToStat(st *unix.Statx_t) Stat {
	s := Stat{Dev: unix.Mkdev(st.Dev_major, st.Dev_minor), Ino: st.Ino, MtimeNs: timestampNs(st.Mtime.Sec, st.Mtime.Nsec), Size: st.Size, Mode: uint32(st.Mode), UID: st.Uid, GID: st.Gid, Nlink: st.Nlink, Kind: kindOfMode(uint32(st.Mode))}
	if st.Mask&unix.STATX_BTIME != 0 {
		v := timestampNs(st.Btime.Sec, st.Btime.Nsec)
		s.BtimeNs = &v
	}
	if st.Mask&unix.STATX_CTIME != 0 {
		v := timestampNs(st.Ctime.Sec, st.Ctime.Nsec)
		s.CtimeNs = &v
	}
	return s
}
func timestampNs(sec int64, nsec uint32) int64 {
	const billion = int64(1e9)
	const bound = math.MaxInt64 / billion
	if sec >= bound {
		return math.MaxInt64
	}
	if sec <= -bound {
		return math.MinInt64
	}
	return sec*billion + int64(nsec)
}
func kindOfMode(mode uint32) Kind {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return KindDir
	case unix.S_IFREG:
		return KindFile
	case unix.S_IFLNK:
		return KindSymlink
	default:
		return KindOther
	}
}
func kindOfDirent(t uint8) Kind {
	switch t {
	case unix.DT_DIR:
		return KindDir
	case unix.DT_REG:
		return KindFile
	case unix.DT_LNK:
		return KindSymlink
	default:
		return KindOther
	}
}

func (r *RootHandle) Stat(p Path) (Stat, error) {
	f, err := r.openLeaf(p, unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return Stat{}, err
	}
	defer f.Close()
	return statOf(f)
}
func (r *RootHandle) OpenRead(p Path, intent AccessIntent) (*File, error) {
	if r.isInflight(p.String()) {
		return nil, fmt.Errorf("open staging file: %w", ErrDenied)
	}
	flags := uint64(unix.O_RDONLY | unix.O_CLOEXEC)
	if intent == IntentReadWrite {
		flags = unix.O_RDWR | unix.O_CLOEXEC
	}
	f, err := r.openLeaf(p, flags)
	if err != nil {
		return nil, err
	}
	return &File{f: f}, nil
}

const dirReadBufBytes = 32 << 10

func (r *RootHandle) ReadDirFunc(p Path, policy ReservedPolicy, fn func(DirEntry) bool) error {
	d, err := r.openLeaf(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer d.Close()
	buf := make([]byte, dirReadBufBytes)
	for {
		n, e := withFd(d, func(fd int) (int, error) { return unix.Getdents(fd, buf) })
		if e != nil {
			return mapErrno("read directory", e)
		}
		if n == 0 {
			return nil
		}
		for off := 0; off < n; {
			entry, reclen, e := parseDirent(buf[off:n])
			if e != nil {
				return e
			}
			off += reclen
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			if policy == HideReserved && IsReservedName(entry.Name) {
				continue
			}
			if !fn(entry) {
				return nil
			}
		}
	}
}
func (r *RootHandle) ReadDir(p Path, policy ReservedPolicy) ([]DirEntry, error) {
	const maxEntries = 100000
	out := make([]DirEntry, 0, 64)
	over := false
	err := r.ReadDirFunc(p, policy, func(e DirEntry) bool {
		if len(out) >= maxEntries {
			over = true
			return false
		}
		out = append(out, e)
		return true
	})
	if err != nil {
		return nil, err
	}
	if over {
		return nil, fmt.Errorf("local: directory entries exceed %d", maxEntries)
	}
	return out, nil
}
func parseDirent(rec []byte) (DirEntry, int, error) {
	if len(rec) < 19 {
		return DirEntry{}, 0, fmt.Errorf("read directory: short dirent header")
	}
	reclen := int(binary.NativeEndian.Uint16(rec[16:]))
	if reclen <= 19 || reclen > len(rec) {
		return DirEntry{}, 0, fmt.Errorf("read directory: invalid dirent length %d", reclen)
	}
	name := rec[19:reclen]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return DirEntry{Name: string(name), Kind: kindOfDirent(rec[18]), Ino: binary.NativeEndian.Uint64(rec)}, reclen, nil
}

func (r *RootHandle) Space(p Path) (FsSpace, error) {
	d, err := r.resolveDirOrParent(p)
	if err != nil {
		return FsSpace{}, err
	}
	defer d.Close()
	return spaceOf(d)
}
func spaceOf(f *os.File) (FsSpace, error) {
	var s unix.Statfs_t
	if err := withFdErr(f, func(fd int) error { return unix.Fstatfs(fd, &s) }); err != nil {
		return FsSpace{}, mapErrno("statfs", err)
	}
	bs := uint64(s.Bsize)
	return FsSpace{Total: s.Blocks * bs, Free: s.Bfree * bs, Available: s.Bavail * bs}, nil
}
func (r *RootHandle) DirDev(p Path) (uint64, error) {
	d, err := r.resolveDirOrParent(p)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	st, err := statOf(d)
	if err != nil {
		return 0, err
	}
	return st.Dev, nil
}

func CopyRange(src, dst *File, srcOff, dstOff, n uint64) (uint64, error) {
	if src == nil || dst == nil {
		return 0, ErrDenied
	}
	const bufBytes = 256 << 10
	buf := make([]byte, bufBytes)
	var copied uint64
	for copied < n {
		want := n - copied
		if want > uint64(len(buf)) {
			want = uint64(len(buf))
		}
		got, err := src.ReadAt(buf[:want], int64(srcOff+copied))
		if got > 0 {
			if _, werr := dst.WriteAt(buf[:got], int64(dstOff+copied)); werr != nil {
				return copied, werr
			}
			copied += uint64(got)
		}
		if err != nil {
			if err == io.EOF {
				return copied, nil
			}
			return copied, err
		}
		if got == 0 {
			return copied, nil
		}
	}
	return copied, nil
}
