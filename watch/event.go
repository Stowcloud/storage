package watch

import "github.com/stowcloud/storage"

// OwnerID identifies one independently watched filesystem root. It is opaque to
// the watcher and belongs to the caller, which may map it to any product or
// backend identity.
type OwnerID string

// Event reports that something beneath an owner has changed.
type Event struct {
	Owner OwnerID

	// Path names the owner-relative directory whose entries changed. It is empty
	// whenever All is set.
	Path storage.Path

	// All indicates the watcher dropped events, so the entire owner must be
	// considered stale. Consumers respond by rebuilding their own state; replay
	// is impossible because the missed events are precisely what is unknown.
	All bool
}

// Stats holds change-detection figures.
type Stats struct {
	// Registered counts directories currently holding a live kernel watch.
	Registered int
	// Pinned counts distinct directories currently held by subscribers.
	Pinned int
	// Degraded tallies refused registrations, each leaving a subtree on lazy
	// revalidation. Any non-zero value denotes a named degradation.
	Degraded int64
	// Owners counts roots known to the watcher.
	Owners int
}
