//go:build linux

package watch

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stowcloud/storage"
	"golang.org/x/sys/unix"
)

func TestWatcherObservesFilesystemChanges(t *testing.T) {
	root := t.TempDir()
	sink := make(chan Event, 16)
	w, err := Start(context.Background(), Config{Debounce: 10 * time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("owner", root, false)
	path := mustPath(t, "docs")
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.Touch("owner", path)
	if err := os.WriteFile(filepath.Join(root, "docs", "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if event := waitEvent(t, sink, func(event Event) bool {
		return event.Owner == "owner" && event.Path.String() == "docs" && !event.All
	}); event.Path.String() != "docs" {
		t.Fatalf("got event %#v", event)
	}
}

func TestWatcherPinsAncestorChain(t *testing.T) {
	root := t.TempDir()
	sink := make(chan Event, 16)
	w, err := Start(context.Background(), Config{Debounce: 10 * time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("owner", root, false)
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.Subscribe("owner", mustPath(t, "a/b/c"))
	if got := w.Stats().Registered; got != 4 {
		t.Fatalf("registered %d directories, want 4", got)
	}
	if got := w.Stats().Pinned; got != 4 {
		t.Fatalf("pinned %d directories, want 4", got)
	}
	w.Unsubscribe("owner", mustPath(t, "a/b/c"))
	if got := w.Stats().Pinned; got != 0 {
		t.Fatalf("pinned %d directories after unsubscribe, want 0", got)
	}
}

func TestWatcherQueueOverflowInvalidatesEveryOwner(t *testing.T) {
	sink := make(chan Event, 8)
	w, err := Start(context.Background(), Config{Debounce: time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("one", t.TempDir(), false)
	w.AddOwner("two", t.TempDir(), false)
	w.consume(overflowRecord())
	if got := waitEvent(t, sink, func(event Event) bool { return event.All }); got.Owner == "" {
		t.Fatal("overflow event had no owner")
	}
	if got := waitEvent(t, sink, func(event Event) bool { return event.All }); got.Owner == "" {
		t.Fatal("second owner was not invalidated")
	}
}

func TestWatcherDebouncesBurst(t *testing.T) {
	root := t.TempDir()
	sink := make(chan Event, 32)
	w, err := Start(context.Background(), Config{Debounce: 100 * time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("owner", root, false)
	path := mustPath(t, "busy")
	if err := os.Mkdir(filepath.Join(root, "busy"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.Touch("owner", path)
	for i := range 10 {
		name := filepath.Join(root, "busy", string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitEvent(t, sink, func(event Event) bool { return event.Path.String() == "busy" })
	count := 0
	settle := time.After(300 * time.Millisecond)
	for {
		select {
		case event := <-sink:
			if !event.All && event.Path.String() == "busy" {
				count++
			}
		case <-settle:
			if count > 3 {
				t.Fatalf("burst produced %d extra events", count)
			}
			return
		}
	}
}

func TestWatcherSharedInodeFansOutAndKeepsWatch(t *testing.T) {
	root := t.TempDir()
	sink := make(chan Event, 16)
	w, err := Start(context.Background(), Config{Debounce: 10 * time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("one", root, false)
	w.AddOwner("two", root, false)
	w.Touch("one", storage.Path{})
	w.Touch("two", storage.Path{})
	if err := os.WriteFile(filepath.Join(root, "note"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := map[OwnerID]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case ev := <-sink:
			if !ev.All && ev.Path.String() == "" {
				seen[ev.Owner] = true
			}
		case <-deadline:
			t.Fatalf("events = %#v", seen)
		}
	}
}

func TestWatcherRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	sink := make(chan Event, 8)
	w, err := Start(context.Background(), Config{}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("owner", root, false)
	w.Touch("owner", mustPath(t, "link"))
	if got := w.Stats().Degraded; got == 0 {
		t.Fatal("symlinked watch was not degraded")
	}
}

func TestWatcherHostChangeMigratesSubscriptions(t *testing.T) {
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	sink := make(chan Event, 16)
	w, err := Start(context.Background(), Config{Debounce: 10 * time.Millisecond}, nil, sink)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer w.Close()
	w.AddOwner("owner", oldRoot, false)
	w.Touch("owner", storage.Path{})
	w.AddOwner("owner", newRoot, false)
	if err := os.WriteFile(filepath.Join(oldRoot, "old"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sink:
		if ev.Owner == "owner" {
			t.Fatalf("old host still emitted %#v", ev)
		}
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(newRoot, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sink, func(ev Event) bool { return ev.Owner == "owner" })
}

func mustPath(t *testing.T, raw string) storage.Path {
	t.Helper()
	path, err := storage.ParsePath(raw)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func waitEvent(t *testing.T, sink <-chan Event, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-sink:
			if match(event) {
				return event
			}
		case <-deadline:
			t.Fatal("no matching watcher event")
		}
	}
}

func overflowRecord() []byte {
	record := make([]byte, inotifyEventHeader)
	binary.NativeEndian.PutUint32(record[offWd:], ^uint32(0))
	binary.NativeEndian.PutUint32(record[offMask:], unix.IN_Q_OVERFLOW)
	return record
}
