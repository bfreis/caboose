// Package fswatch watches directory trees on the host for changes, for the
// host link to relay into a sandbox that does not see them: gVisor (and a
// VM) turns none of the host's edits into inotify events inside.
//
// A Watcher reports paths only, never what happened to them: the relay
// stats each one itself. On macOS it is FSEvents, through purego (the
// launcher is built without cgo); on Linux, inotify with a watch per
// directory.
package fswatch

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrUnsupported is a platform with no watcher.
var ErrUnsupported = errors.New("watching files is not supported on " + runtime.GOOS)

// maxPending bounds the paths held between two Next calls. Past it the
// watcher gives up on paths and reports its roots, as it does when the
// system itself dropped events: whatever reads them has to look again.
const maxPending = 1 << 16

// Watcher watches trees for changes until Close.
type Watcher struct {
	roots []root

	mu       sync.Mutex
	pending  []string
	seen     map[string]bool
	overflow bool
	closed   bool
	wake     chan struct{}

	stop func()
}

// root is a watched tree: the path it was given as, and the physical path
// the system reports events under.
type root struct {
	given, phys string
}

// Watch starts watching the trees at roots, which must be directories.
// Paths are reported under a root as it was given here, whatever symlinks
// it resolves through.
func Watch(roots []string) (*Watcher, error) {
	w := &Watcher{seen: map[string]bool{}, wake: make(chan struct{}, 1)}
	for _, r := range roots {
		given := filepath.Clean(r)
		phys, err := filepath.EvalSymlinks(given)
		if err != nil {
			return nil, err
		}
		w.roots = append(w.roots, root{given: given, phys: phys})
	}
	if len(w.roots) == 0 {
		return nil, errors.New("fswatch: nothing to watch")
	}
	stop, err := start(w)
	if err != nil {
		return nil, err
	}
	w.stop = stop
	return w, nil
}

// Next waits for changes and returns the paths that changed since the last
// call, each once, in no particular order. A root among them means events
// were lost under it. ok is false once the watcher is closed.
func (w *Watcher) Next() (paths []string, ok bool) {
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return nil, false
		}
		if w.overflow {
			w.overflow = false
			w.pending, w.seen = nil, map[string]bool{}
			w.mu.Unlock()
			return w.Roots(), true
		}
		if len(w.pending) > 0 {
			paths = w.pending
			w.pending, w.seen = nil, map[string]bool{}
			w.mu.Unlock()
			return paths, true
		}
		w.mu.Unlock()
		<-w.wake
	}
}

// Roots are the watched trees, as given.
func (w *Watcher) Roots() []string {
	var rs []string
	for _, r := range w.roots {
		rs = append(rs, r.given)
	}
	return rs
}

// Close stops the watcher; a Next waiting returns.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	w.stop()
	w.signal()
	return nil
}

func (w *Watcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *Watcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// add records a physical path the system reported. One outside every root
// is dropped.
func (w *Watcher) add(phys string) {
	p, ok := w.given(phys)
	if !ok {
		return
	}
	w.mu.Lock()
	switch {
	case w.closed || w.overflow || w.seen[p]:
	case len(w.pending) >= maxPending:
		w.overflow = true
	default:
		w.seen[p] = true
		w.pending = append(w.pending, p)
	}
	w.mu.Unlock()
	w.signal()
}

// lost says the system dropped events: everything has to be looked at
// again.
func (w *Watcher) lost() {
	w.mu.Lock()
	w.overflow = true
	w.mu.Unlock()
	w.signal()
}

// given maps a physical path back under the root it was given as. On a Mac
// the names are compared as the file system does, ignoring case.
func (w *Watcher) given(phys string) (string, bool) {
	phys = filepath.Clean(phys)
	for _, r := range w.roots {
		if within(phys, r.phys) {
			return filepath.Join(r.given, phys[len(r.phys):]), true
		}
	}
	return "", false
}

// within reports whether p is dir or under it.
func within(p, dir string) bool {
	if len(p) < len(dir) || !samePath(p[:len(dir)], dir) {
		return false
	}
	return len(p) == len(dir) || p[len(dir)] == '/' || strings.HasSuffix(dir, "/")
}

func samePath(a, b string) bool {
	if runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
