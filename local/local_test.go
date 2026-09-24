//go:build linux

package local

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestConfinedReadRejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(host, "link")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := ParsePath("link")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.OpenRead(p, IntentRead)
	if !errors.Is(err, ErrSymlinkDenied) {
		t.Fatalf("OpenRead = %v, want symlink denial", err)
	}
}

func TestConfinedReadRejectsTraversalPath(t *testing.T) {
	if _, err := ParsePath("../secret"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("ParsePath traversal = %v", err)
	}
}

func TestDurableWriteStagingIsReservedAndHidden(t *testing.T) {
	host := t.TempDir()
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, _ := ParsePath("file")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, werr := r.WriteDurable(p, DurableOpts{Mode: 0o600}, func(f *File) error {
			if _, err := f.WriteAt([]byte("partial"), 0); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
		done <- werr
	}()
	<-entered
	entries, err := r.ReadDir(RootPath(), HideReserved)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if IsStagingName(entry.Name) {
			t.Fatalf("staging entry leaked into hidden listing: %q", entry.Name)
		}
	}
	entries, err = r.ReadDir(RootPath(), IncludeReserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !IsStagingName(entries[0].Name) {
		t.Fatalf("reserved listing = %+v, want one staging entry", entries)
	}
	st, err := os.Stat(filepath.Join(host, entries[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0 {
		t.Fatalf("staging mode = %o, want 0000 while incomplete", got)
	}
	stage, err := ParsePath(entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenRead(stage, IntentRead); !errors.Is(err, ErrDenied) {
		t.Fatalf("opening incomplete staging file = %v, want ErrDenied", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDurableWritePublishesAndRestoresMode(t *testing.T) {
	host := t.TempDir()
	target := filepath.Join(host, "file")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := ParsePath("file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.WriteDurable(p, DurableOpts{Mode: 0o664}, func(f *File) error { _, e := f.WriteAt([]byte("new"), 0); return e }); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Fatalf("body = %q", body)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
}

func TestDurableWriteAppliesNewFileOwnerAndMode(t *testing.T) {
	host := t.TempDir()
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, _ := ParsePath("owned")
	owner := Owner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	if _, err := r.WriteDurable(p, DurableOpts{Mode: 0o640, Owner: &owner}, func(f *File) error {
		_, err := f.WriteAt([]byte("content"), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st, err := r.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode&0o7777 != 0o640 {
		t.Fatalf("mode = %o, want 0640", st.Mode&0o7777)
	}
	if st.UID != owner.UID || st.GID != owner.GID {
		t.Fatalf("owner = %d:%d, want %d:%d", st.UID, st.GID, owner.UID, owner.GID)
	}
}

func TestDurableWriteFailureLeavesPriorContent(t *testing.T) {
	host := t.TempDir()
	target := filepath.Join(host, "file")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, _ := ParsePath("file")
	boom := errors.New("write failed")
	if _, err := r.WriteDurable(p, DurableOpts{Mode: 0o664}, func(f *File) error { _, _ = f.WriteAt([]byte("new"), 0); return boom }); !errors.Is(err, boom) {
		t.Fatalf("write = %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old" {
		t.Fatalf("body = %q", body)
	}
	entries, err := r.ReadDir(RootPath(), IncludeReserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "file" {
		t.Fatalf("entries after failed write = %+v, want only the original file", entries)
	}
}

func TestBoundedDirectoryReadAndMaterializedHandle(t *testing.T) {
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(host, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, _ := ParsePath("f")
	f, err := r.OpenRead(p, IntentRead)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 5)
	n, err := f.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(b[:n]) != "hello" {
		t.Fatalf("read = %q", b[:n])
	}
	entries, err := r.ReadDir(RootPath(), HideReserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "f" {
		t.Fatalf("entries = %+v", entries)
	}
}
