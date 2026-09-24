//go:build linux

// Package watch provides best-effort filesystem change detection built from a
// capped set of watched directories, a debounce stage, and a periodic rescan.
//
// The watcher is a sensor, not a source of truth. Consumers must re-read paths
// before trusting cached state. A capped, degraded, or dead watcher therefore
// costs promptness, never correctness.
package watch

import (
	"fmt"
	"time"
)

// Backend identifies the change-detection transport. This build implements
// inotify only; unknown values are rejected rather than silently substituted.
type Backend uint8

// BackendInotify is the only transport implemented by this package.
const BackendInotify Backend = iota

func (b Backend) String() string {
	if b == BackendInotify {
		return "inotify"
	}
	return fmt.Sprintf("backend(%d)", b)
}

// ParseBackend validates a configured transport at the trust boundary.
func ParseBackend(s string) (Backend, error) {
	if s == "inotify" {
		return BackendInotify, nil
	}
	return 0, fmt.Errorf("watch backend %q is not implemented; the only one is %q", s, "inotify")
}

// Clock supplies the timestamp used to start debounce windows. Implementations
// may also provide monotonic behavior through time.Time values from time.Now.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Config carries every setting the watcher takes.
type Config struct {
	Backend Backend

	// HotSetMax limits how many directories may hold a live kernel watch.
	HotSetMax int

	// FullThreshold bounds how many directories may be dirty before the watcher
	// invalidates every owner rather than enumerating individual directories.
	FullThreshold int

	// Debounce sets how long a directory stays dirty before being reported.
	Debounce time.Duration

	// RescanInterval controls how frequently directories on event-losing
	// filesystems are marked dirty again.
	RescanInterval time.Duration

	// OnCoverageLost runs when the watcher can no longer guarantee that every
	// registered directory's changes reach consumers.
	OnCoverageLost func()
}

// DefaultConfig returns safe production bounds.
func DefaultConfig() Config {
	return Config{
		Backend:        BackendInotify,
		HotSetMax:      4096,
		FullThreshold:  50_000,
		Debounce:       200 * time.Millisecond,
		RescanInterval: 60 * time.Second,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.HotSetMax <= 0 {
		c.HotSetMax = d.HotSetMax
	}
	if c.FullThreshold <= 0 {
		c.FullThreshold = d.FullThreshold
	}
	if c.Debounce <= 0 {
		c.Debounce = d.Debounce
	}
	if c.RescanInterval <= 0 {
		c.RescanInterval = d.RescanInterval
	}
	return c
}
