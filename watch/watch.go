//go:build linux

package watch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stowcloud/storage"
	"golang.org/x/sys/unix"
)

const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_MOVE_SELF |
	unix.IN_DELETE_SELF | unix.IN_ATTRIB

const eventBufBytes = 16 << 10
const flushInterval = 50 * time.Millisecond
const inotifyEventHeader = 16

const (
	offWd      = 0
	offMask    = 4
	offNameLen = 12
)

type ownerRoot struct {
	host   string
	anchor *os.File
	rescan bool
}

// Watcher detects changes using one kernel descriptor, reader, debounce loop,
// and rescan loop. It is safe for concurrent API calls.
type Watcher struct {
	cfg  Config
	clk  Clock
	sink chan<- Event

	inotify *os.File

	mu       sync.Mutex
	regMu    sync.Mutex
	hot      *hotSet
	wdToKeys map[int]map[key]struct{}
	keyToWd  map[key]int
	owners   map[OwnerID]ownerRoot
	pending  map[key]time.Time

	fullThreshold atomic.Int64
	overflow      atomic.Bool
	degraded      atomic.Int64

	onCoverageLost func()
	stop           chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

// Start launches a watcher. The sink is owned by the caller and should be
// buffered when it needs to absorb bursts. Close stops all internal loops.
func Start(ctx context.Context, cfg Config, clk Clock, sink chan<- Event) (*Watcher, error) {
	cfg = cfg.withDefaults()
	if cfg.Backend != BackendInotify {
		return nil, fmt.Errorf("watch backend %q is not implemented", cfg.Backend.String())
	}
	if clk == nil {
		clk = systemClock{}
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("start the watcher: %w", err)
	}
	w := &Watcher{
		cfg:      cfg,
		clk:      clk,
		sink:     sink,
		inotify:  os.NewFile(uintptr(fd), "inotify"),
		hot:      newHotSet(cfg.HotSetMax),
		wdToKeys: make(map[int]map[key]struct{}),
		keyToWd:  make(map[key]int),
		owners:   make(map[OwnerID]ownerRoot),
		pending:  make(map[key]time.Time),
		stop:     make(chan struct{}),
	}
	w.fullThreshold.Store(int64(cfg.FullThreshold))
	w.wg.Add(3)
	go w.runLoop(w.readLoop)
	go w.runLoop(w.flushLoop)
	go w.runLoop(w.rescanLoop)
	go func() {
		select {
		case <-ctx.Done():
			_ = w.Close()
		case <-w.stop:
		}
	}()
	return w, nil
}

func (w *Watcher) runLoop(fn func()) {
	defer w.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("watcher loop panicked", slog.Any("panic", recovered))
		}
	}()
	fn()
}

// AddOwner records an owner's host root. Existing subscriptions are migrated
// // to the new root before the method returns; rescan marks filesystems whose
// modifications inotify cannot observe, such as network and FUSE mounts.
func (w *Watcher) AddOwner(id OwnerID, hostRoot string, rescan bool) {
	anchor, err := openOwnerRoot(hostRoot)
	w.mu.Lock()
	old, hadOld := w.owners[id]
	if hadOld && old.host == hostRoot && old.anchor != nil && anchor != nil {
		old.rescan = rescan
		w.owners[id] = old
		w.mu.Unlock()
		_ = anchor.Close()
		return
	}
	oldKeys := make([]key, 0)
	seenKeys := make(map[key]struct{})
	for k := range w.hot.sticky {
		if k.owner == id {
			oldKeys = append(oldKeys, k)
			seenKeys[k] = struct{}{}
		}
	}
	for k := range w.keyToWd {
		if k.owner == id {
			if _, seen := seenKeys[k]; !seen {
				oldKeys = append(oldKeys, k)
			}
		}
	}
	oldWds := make(map[int]struct{})
	for _, k := range oldKeys {
		wd, ok := w.keyToWd[k]
		if ok {
			delete(w.keyToWd, k)
			if keys := w.wdToKeys[wd]; keys != nil {
				delete(keys, k)
				if len(keys) == 0 {
					delete(w.wdToKeys, wd)
					oldWds[wd] = struct{}{}
				}
			}
		}
		w.hot.markUnregistered(k)
		delete(w.pending, k)
	}
	w.owners[id] = ownerRoot{host: hostRoot, anchor: anchor, rescan: rescan}
	w.mu.Unlock()
	for wd := range oldWds {
		if err := w.rmWatch(wd); err != nil {
			slog.Debug("removing a host migration watch failed", slog.Any("error", err))
		}
	}
	if hadOld && old.anchor != nil {
		_ = old.anchor.Close()
	}
	for _, k := range oldKeys {
		if !w.register(k) {
			w.coverageLost(k.owner)
		}
	}
	if err != nil {
		w.coverageLost(id)
	}
}

// RemoveOwner removes an owner and all of its subscriptions.
func (w *Watcher) RemoveOwner(id OwnerID) {
	w.mu.Lock()
	owner, ok := w.owners[id]
	keys := make([]key, 0)
	for k := range w.keyToWd {
		if k.owner == id {
			keys = append(keys, k)
		}
	}
	for k := range w.hot.sticky {
		if k.owner == id {
			delete(w.hot.sticky, k)
			delete(w.pending, k)
		}
	}
	delete(w.owners, id)
	w.mu.Unlock()
	for _, k := range keys {
		w.unregister(k)
	}
	if ok && owner.anchor != nil {
		_ = owner.anchor.Close()
	}
}

func openOwnerRoot(host string) (*os.File, error) {
	info, err := os.Lstat(host)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("watch root is not a real directory")
	}
	fd, err := unix.Open(host, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), host), nil
}

func (w *Watcher) loseCoverage() {
	if w.onCoverageLost != nil {
		w.onCoverageLost()
	}
}

// Subscribe pins a directory and every ancestor. The watches are registered
// before this method returns, so a caller can read without a coverage gap.
func (w *Watcher) Subscribe(id OwnerID, p storage.Path) {
	for _, k := range ancestorKeys(id, p) {
		w.mu.Lock()
		needsRegister, accepted := w.hot.addSticky(k)
		w.mu.Unlock()
		if !accepted {
			w.degraded.Add(1)
			w.coverageLost(id)
			continue
		}
		if needsRegister && !w.register(k) {
			w.coverageLost(id)
		}
	}
}

// Unsubscribe drops one subscriber's pins for a directory and its ancestors.
func (w *Watcher) Unsubscribe(id OwnerID, p storage.Path) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range ancestorKeys(id, p) {
		w.hot.removeSticky(k)
	}
}

// Touch notes a read of a directory, making it eligible for the recent hot set.
func (w *Watcher) Touch(id OwnerID, p storage.Path) {
	k := key{owner: id, path: p.String()}
	w.mu.Lock()
	isNew := w.hot.touch(k)
	w.mu.Unlock()
	if isNew && !w.register(k) {
		w.coverageLost(id)
	}
}

// Stats reports current watch coverage.
func (w *Watcher) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Stats{
		Registered: w.hot.registeredCount(),
		Pinned:     w.hot.stickyCount(),
		Degraded:   w.degraded.Load(),
		Owners:     len(w.owners),
	}
}

// SetBounds applies live hot-set and overflow bounds.
func (w *Watcher) SetBounds(hotSetMax, fullThreshold int) {
	if fullThreshold <= 0 {
		fullThreshold = DefaultConfig().FullThreshold
	}
	w.fullThreshold.Store(int64(fullThreshold))
	w.mu.Lock()
	evicted := w.hot.setCap(hotSetMax)
	w.mu.Unlock()
	for _, k := range evicted {
		w.unregister(k)
	}
}

// Close halts all loops and gives up the kernel descriptor. It is idempotent.
func (w *Watcher) Close() error {
	var err error
	w.stopOnce.Do(func() {
		close(w.stop)
		err = w.inotify.Close()
		w.wg.Wait()
	})
	return err
}

func ancestorKeys(id OwnerID, p storage.Path) []key {
	components := p.Components()
	out := make([]key, 0, len(components)+1)
	out = append(out, key{owner: id})
	path := ""
	for _, component := range components {
		if path == "" {
			path = component
		} else {
			path += "/" + component
		}
		out = append(out, key{owner: id, path: path})
	}
	return out
}
func (w *Watcher) register(k key) bool {
	w.regMu.Lock()
	defer w.regMu.Unlock()
	w.mu.Lock()
	_, ownerOK := w.owners[k.owner]
	w.mu.Unlock()
	if !ownerOK {
		w.degraded.Add(1)
		return false
	}
	w.mu.Lock()
	evicted := w.hot.evictFor(k)
	w.mu.Unlock()
	for _, old := range evicted {
		w.unregister(old)
	}
	wd, err := w.addWatch(k)
	if err != nil {
		w.degraded.Add(1)
		return false
	}
	w.mu.Lock()
	if w.wdToKeys[wd] == nil {
		w.wdToKeys[wd] = make(map[key]struct{})
	}
	w.wdToKeys[wd][k] = struct{}{}
	w.keyToWd[k] = wd
	w.hot.markRegistered(k)
	w.mu.Unlock()
	return true
}

func (w *Watcher) coverageLost(id OwnerID) { w.loseCoverage(); w.emit(Event{Owner: id, All: true}) }

func (w *Watcher) unregister(k key) {
	w.mu.Lock()
	wd, ok := w.keyToWd[k]
	if ok {
		delete(w.keyToWd, k)
		keys := w.wdToKeys[wd]
		delete(keys, k)
		if len(keys) == 0 {
			delete(w.wdToKeys, wd)
		}
	}
	w.hot.markUnregistered(k)
	stillShared := ok && len(w.wdToKeys[wd]) > 0
	w.mu.Unlock()
	if !ok || stillShared {
		return
	}
	_ = w.rmWatch(wd)
}

func (w *Watcher) addWatch(k key) (int, error) {
	w.mu.Lock()
	o, ok := w.owners[k.owner]
	w.mu.Unlock()
	if !ok || o.anchor == nil {
		return 0, os.ErrNotExist
	}
	rel := k.path
	if rel == "" {
		rel = "."
	}
	how := &unix.OpenHow{Flags: uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS}
	fd, err := unix.Openat2(int(o.anchor.Fd()), rel, how)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	rc, err := w.inotify.SyscallConn()
	if err != nil {
		return 0, err
	}
	var wd int
	var inner error
	if err := rc.Control(func(infd uintptr) {
		wd, inner = unix.InotifyAddWatch(int(infd), fmt.Sprintf("/proc/self/fd/%d", fd), watchMask)
	}); err != nil {
		return 0, err
	}
	return wd, inner
}

func (w *Watcher) rmWatch(wd int) error {
	if wd < 0 || uint64(wd) > uint64(^uint32(0)) {
		return fmt.Errorf("watch descriptor %d is outside uint32", wd)
	}
	rc, err := w.inotify.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := rc.Control(func(fd uintptr) { _, inner = unix.InotifyRmWatch(int(fd), uint32(wd)) }); err != nil {
		return err
	}
	return inner
}

func (w *Watcher) readLoop() {
	buf := make([]byte, eventBufBytes)
	for {
		n, err := w.inotify.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, io.EOF) {
				return
			}
			select {
			case <-w.stop:
				return
			default:
			}
			w.loseCoverage()
			slog.Warn("the watch descriptor stopped reading", slog.Any("error", err))
			return
		}
		w.consume(buf[:n])
	}
}

func (w *Watcher) consume(buf []byte) {
	for off := 0; off < len(buf); {
		if off+inotifyEventHeader > len(buf) {
			w.escalate("an inotify record header was truncated")
			return
		}
		wd := int(binary.NativeEndian.Uint32(buf[off+offWd:]))
		mask := binary.NativeEndian.Uint32(buf[off+offMask:])
		nameLen := binary.NativeEndian.Uint32(buf[off+offNameLen:])
		size := uint64(nameLen)
		if size > uint64(len(buf)) || size > uint64(^uint(0)>>1) || off+inotifyEventHeader+int(size) > len(buf) {
			w.escalate("an inotify record name ran past the buffer")
			return
		}
		off += inotifyEventHeader + int(size)
		switch {
		case mask&unix.IN_Q_OVERFLOW != 0:
			w.loseCoverage()
			w.overflow.Store(true)
		case mask&unix.IN_UNMOUNT != 0:
			w.forget(wd)
			w.loseCoverage()
		case mask&unix.IN_IGNORED != 0:
			w.forget(wd)
		default:
			w.markDirty(wd)
		}
	}
}

func (w *Watcher) escalate(reason string) {
	w.loseCoverage()
	slog.Warn("the inotify stream could not be parsed; invalidating whole owners", slog.String("reason", reason))
	w.overflow.Store(true)
}

func (w *Watcher) markDirty(wd int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.wdToKeys[wd] {
		if _, seen := w.pending[k]; !seen {
			w.pending[k] = w.clk.Now()
		}
	}
}

func (w *Watcher) forget(wd int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.wdToKeys[wd] {
		delete(w.keyToWd, k)
		w.hot.markUnregistered(k)
	}
	delete(w.wdToKeys, wd)
}

func (w *Watcher) flushLoop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		overflowed := w.overflow.Swap(false)
		w.mu.Lock()
		pendingLen := len(w.pending)
		w.mu.Unlock()
		if overflowed || int64(pendingLen) > w.fullThreshold.Load() {
			w.invalidateEverything(overflowed, pendingLen)
			continue
		}
		for _, k := range w.ready() {
			path, _ := storage.ParsePath(k.path)
			w.emit(Event{Owner: k.owner, Path: path})
		}
	}
}

func (w *Watcher) ready() []key {
	now := w.clk.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []key
	for k, first := range w.pending {
		if now.Sub(first) >= w.cfg.Debounce {
			out = append(out, k)
			delete(w.pending, k)
		}
	}
	return out
}

func (w *Watcher) invalidateEverything(kernelOverflow bool, pendingLen int) {
	w.loseCoverage()
	w.mu.Lock()
	clear(w.pending)
	ids := make([]OwnerID, 0, len(w.owners))
	for id := range w.owners {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	slog.Warn("the watch queue overflowed; invalidating whole owners instead of directories",
		slog.Bool("kernel overflow", kernelOverflow),
		slog.Int("pending", pendingLen),
		slog.Int64("threshold", w.fullThreshold.Load()))
	for _, id := range ids {
		w.emit(Event{Owner: id, All: true})
	}
}

func (w *Watcher) emit(ev Event) {
	select {
	case <-w.stop:
		return
	default:
	}
	select {
	case w.sink <- ev:
	case <-w.stop:
	default:
		w.loseCoverage()
		w.overflow.Store(true)
	}
}

func (w *Watcher) rescanLoop() {
	t := time.NewTicker(w.cfg.RescanInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.rescan()
		}
	}
}

func (w *Watcher) rescan() {
	w.mu.Lock()
	defer w.mu.Unlock()
	unreliable := make(map[OwnerID]struct{})
	for id, owner := range w.owners {
		if owner.rescan {
			unreliable[id] = struct{}{}
		}
	}
	now := w.clk.Now()
	for _, k := range w.hot.registeredKeys() {
		if _, ok := unreliable[k.owner]; !ok {
			continue
		}
		if _, seen := w.pending[k]; !seen {
			w.pending[k] = now
		}
	}
}
