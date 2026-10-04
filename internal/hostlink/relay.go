package hostlink

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/fswatch"
)

// The relay: under gVisor (and in a VM) nothing the host changes under the
// roots becomes an inotify event inside, so a dev server or a watch-mode
// test there never sees an edit made in an editor here. The host watches
// the roots, and sends the agent the container paths that changed; the
// agent does something to each that raises an event inside without
// changing it.
//
// The agent's touch reaches the host as a change too. What stops the loop
// is that the relay sends a path only when what it stats there differs from
// what it last sent it with: the touch leaves type, mode, size and mtime as
// they were.

const (
	// relayGather is how long the relay lets changes gather after sending
	// some, so a burst goes in few messages.
	relayGather = 50 * time.Millisecond
	// relayPerDir is the most paths sent for one directory in a batch;
	// past it the directory goes instead, for watchers there to look
	// again.
	relayPerDir = 64
	// relayPerBatch is the most paths sent in a batch; past it only the
	// roots go.
	relayPerBatch = 4096
	// relayMaxPath is the longest path sent: longer is not a real one.
	relayMaxPath = 4096
	// relayForget is how long a sent path's state is kept, and
	// relaySeenMax the most kept.
	relayForget  = time.Minute
	relaySeenMax = 1 << 16
)

// Root is a directory the container mounts: where it is here, and there.
type Root struct {
	Host, Container string
}

// Watcher is what the relay reads the host's changes from: host paths, a
// batch at a time, until closed. fswatch.Watcher is one.
type Watcher interface {
	Next() ([]string, bool)
	Close() error
}

// WatchRoots is fswatch.Watch, as a Watcher.
func WatchRoots(dirs []string) (Watcher, error) { return fswatch.Watch(dirs) }

// fileState is what the relay compares a path by.
type fileState struct {
	exists bool
	mode   fs.FileMode
	size   int64
	mtime  time.Time
}

type sent struct {
	state fileState
	at    time.Time
}

// relay decides what of the host's changes to send.
type relay struct {
	roots []Root
	stat  func(string) fileState
	now   func() time.Time
	seen  map[string]sent
}

func newRelay(roots []Root) *relay {
	return &relay{roots: roots, stat: lstat, now: time.Now, seen: map[string]sent{}}
}

func lstat(p string) fileState {
	fi, err := os.Lstat(p)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, mode: fi.Mode(), size: fi.Size(), mtime: fi.ModTime()}
}

// change is a path to send: where it is here, and in the container.
type change struct {
	host, container string
}

// batch turns host paths that changed into the container paths to send.
func (r *relay) batch(hostPaths []string) []string {
	now := r.now()
	r.forget(now)
	var out []change
	for _, hp := range hostPaths {
		hp = filepath.Clean(hp)
		cp, ok := r.containerPath(hp)
		if !ok || len(cp) > relayMaxPath || inGit(cp) {
			continue
		}
		st := r.stat(hp)
		if !r.differs(hp, st, now) {
			continue
		}
		if !st.exists {
			// The agent touches the directory of what is gone.
			r.differs(filepath.Dir(hp), r.stat(filepath.Dir(hp)), now)
		}
		out = append(out, change{hp, cp})
	}
	return r.coalesce(out, now)
}

// differs records st as what hp is sent with, and says whether that is
// news: not what it was last sent with.
func (r *relay) differs(hp string, st fileState, now time.Time) bool {
	if prev, ok := r.seen[hp]; ok && prev.state == st {
		return false
	}
	r.seen[hp] = sent{state: st, at: now}
	return true
}

// forget drops what was sent long enough ago that no echo of it is still
// coming, and everything when too much is kept.
func (r *relay) forget(now time.Time) {
	if len(r.seen) >= relaySeenMax {
		r.seen = map[string]sent{}
		return
	}
	for p, s := range r.seen {
		if now.Sub(s.at) > relayForget {
			delete(r.seen, p)
		}
	}
}

// containerPath maps a host path under a root to the container's.
func (r *relay) containerPath(hp string) (string, bool) {
	for _, root := range r.roots {
		rest, ok := strings.CutPrefix(hp, root.Host)
		if ok && (rest == "" || rest[0] == '/') {
			return root.Container + rest, true
		}
	}
	return "", false
}

// inGit reports whether p is in a .git directory: git's own churn, which
// watchers ignore.
func inGit(p string) bool {
	return strings.Contains(p+"/", "/.git/")
}

// coalesce puts a directory in place of its paths when a batch has too
// many in it, and the roots in place of everything when it is still too
// long. A directory sent is recorded as its paths are, so the agent's
// touch of it is not sent back.
func (r *relay) coalesce(changes []change, now time.Time) []string {
	byDir := map[string][]change{}
	for _, c := range changes {
		d := filepath.Dir(c.container)
		byDir[d] = append(byDir[d], c)
	}
	var out []change
	for d, cs := range byDir {
		if len(cs) > relayPerDir {
			out = append(out, change{filepath.Dir(cs[0].host), d})
		} else {
			out = append(out, cs...)
		}
	}
	if len(out) > relayPerBatch {
		out = out[:0]
		for _, root := range r.roots {
			out = append(out, change{root.Host, root.Container})
		}
	}
	var paths []string
	for _, c := range out {
		if st := r.stat(c.host); st.mode.IsDir() {
			r.seen[c.host] = sent{state: st, at: now}
		}
		paths = append(paths, c.container)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// relayChanges sends the host's changes under the roots until w is closed
// or the session ends.
func (h *Host) relayChanges(w Watcher) {
	r := newRelay(h.cfg.Relay)
	for {
		hostPaths, ok := w.Next()
		if !ok {
			return
		}
		for _, msg := range changedMessages(r.batch(hostPaths)) {
			if err := h.sess.Send(msg); err != nil {
				return
			}
		}
		select {
		case <-h.sess.Done():
			return
		case <-time.After(relayGather):
		}
	}
}

// changedMessages splits paths into messages that each fit a frame.
func changedMessages(paths []string) []agentproto.Message {
	var msgs []agentproto.Message
	var cur []string
	size := 0
	for _, p := range paths {
		b, _ := json.Marshal(p)
		n := len(b) + 1
		if len(cur) == agentproto.MaxChanged || size+n > agentproto.MaxPayload/2 {
			msgs = append(msgs, agentproto.Message{Type: agentproto.TypeChanged, Paths: cur})
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += n
	}
	if len(cur) > 0 {
		msgs = append(msgs, agentproto.Message{Type: agentproto.TypeChanged, Paths: cur})
	}
	return msgs
}
