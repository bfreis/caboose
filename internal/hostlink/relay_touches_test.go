package hostlink

import (
	"io/fs"
	"slices"
	"testing"
	"time"
)

// How many touches one host save costs: each batch is one Next from the
// watcher (batches are at least relayGather apart), and every path a batch
// returns is one touch in the sandbox.

func fileAt(size int64, ns int64) fileState {
	return fileState{exists: true, mode: 0o644, size: size, mtime: time.Unix(1000, ns)}
}

// sends runs batches in order, each with states set first, and returns
// what each one sent.
func sends(t *testing.T, r *relay, states map[string]fileState, steps []struct {
	set   map[string]fileState
	paths []string
}) [][]string {
	t.Helper()
	var out [][]string
	for _, s := range steps {
		for p, st := range s.set {
			if st.exists {
				states[p] = st
			} else {
				delete(states, p)
			}
		}
		out = append(out, r.batch(s.paths))
	}
	return out
}

type step = struct {
	set   map[string]fileState
	paths []string
}

const tf = "/Users/me/dev/p/f.txt"

// printf > f: open(O_TRUNC), then write. If FSEvents reports the truncate
// on its own (NoDefer delivers the first event of a quiet spell at once)
// and the relay stats f before the write lands, the truncate and the
// write are two states in two batches: two touches.
func TestRelayTwiceTruncateThenWriteInTwoBatches(t *testing.T) {
	states := map[string]fileState{tf: fileAt(4, 0)}
	r := testRelay(states)
	r.batch([]string{tf}) // f's state before the save, as sent earlier
	got := sends(t, r, states, []step{
		{map[string]fileState{tf: fileAt(0, 1)}, []string{tf}}, // truncated
		{map[string]fileState{tf: fileAt(9, 2)}, []string{tf}}, // written
		{nil, []string{tf}}, // the echo of both touches
	})
	t.Logf("sent per batch: %v", got)
	if n := len(got[0]) + len(got[1]) + len(got[2]); n != 2 {
		t.Fatalf("truncate and write in two batches: %d touches, want 2 (%v)", n, got)
	}
}

// The same save, but the stat lands after the write (the usual case: a
// shell's printf writes microseconds after the truncate, FSEvents takes
// milliseconds): the write's own event finds the state already sent.
func TestRelayOnceTruncateThenWriteStattedLate(t *testing.T) {
	states := map[string]fileState{tf: fileAt(4, 0)}
	r := testRelay(states)
	r.batch([]string{tf})
	got := sends(t, r, states, []step{
		{map[string]fileState{tf: fileAt(9, 2)}, []string{tf}}, // truncate's event, statted after the write
		{nil, []string{tf}}, // the write's event
		{nil, []string{tf}}, // the echo
	})
	t.Logf("sent per batch: %v", got)
	if n := len(got[0]) + len(got[1]) + len(got[2]); n != 1 {
		t.Fatalf("%d touches, want 1 (%v)", n, got)
	}
}

// >> f: one write, one state. Whatever FSEvents reports for it (the write,
// the close, a repeat in the next callback) stats the same.
func TestRelayOnceAppend(t *testing.T) {
	states := map[string]fileState{tf: fileAt(4, 0)}
	r := testRelay(states)
	r.batch([]string{tf})
	got := sends(t, r, states, []step{
		{map[string]fileState{tf: fileAt(9, 1)}, []string{tf, tf}},
		{nil, []string{tf}},
		{nil, []string{tf}},
	})
	t.Logf("sent per batch: %v", got)
	if n := len(got[0]) + len(got[1]) + len(got[2]); n != 1 {
		t.Fatalf("%d touches, want 1 (%v)", n, got)
	}
}

// An editor's safe save: write f.tmp, rename it over f. f.tmp is touched
// if its creation came in a batch of its own; the rename sends f (touched)
// and f.tmp (gone: its directory is touched). Two or three touches for one
// save, by design: f's and the directory's.
func TestRelaySafeSave(t *testing.T) {
	const tmp = "/Users/me/dev/p/f.txt.tmp"
	dir := fileState{exists: true, mode: fs.ModeDir | 0o755, mtime: time.Unix(1000, 0)}
	states := map[string]fileState{tf: fileAt(4, 0), "/Users/me/dev/p": dir}
	r := testRelay(states)
	r.batch([]string{tf})
	dir2 := dir
	dir2.mtime = time.Unix(1000, 5)
	got := sends(t, r, states, []step{
		{map[string]fileState{tmp: fileAt(9, 3)}, []string{tmp}},
		{map[string]fileState{tmp: {}, tf: fileAt(9, 3), "/Users/me/dev/p": dir2}, []string{tmp, tf}},
		{nil, []string{tf, "/Users/me/dev/p"}}, // echoes: f's touch, the directory's
	})
	t.Logf("sent per batch: %v", got)
	if !slices.Equal(got[0], []string{"/work/p/f.txt.tmp"}) ||
		!slices.Equal(got[1], []string{"/work/p/f.txt", "/work/p/f.txt.tmp"}) || len(got[2]) != 0 {
		t.Fatalf("safe save: %v", got)
	}
}

// The agent's touch reaches the host as a change; its stat is what was
// sent, so it is dropped, a directory's too.
func TestRelayEchoDropped(t *testing.T) {
	states := map[string]fileState{tf: fileAt(4, 0)}
	r := testRelay(states)
	if got := r.batch([]string{tf}); len(got) != 1 {
		t.Fatalf("first: %v", got)
	}
	for range 3 {
		if got := r.batch([]string{tf}); len(got) != 0 {
			t.Fatalf("echo sent: %v", got)
		}
	}
}

// A deletion whose directory FSEvents reports too, in the same batch:
// both are sent, and the agent touches the directory twice, once as a
// path and once as the parent of what is gone.
func TestRelayDeleteWithItsDirectoryReported(t *testing.T) {
	dir := fileState{exists: true, mode: fs.ModeDir | 0o755, mtime: time.Unix(1000, 0)}
	states := map[string]fileState{"/Users/me/dev/p": dir}
	r := testRelay(states)
	r.batch([]string{"/Users/me/dev/p"})
	dir2 := dir
	dir2.mtime = time.Unix(1000, 7)
	got := sends(t, r, states, []step{
		{map[string]fileState{"/Users/me/dev/p": dir2}, []string{"/Users/me/dev/p", tf}},
	})
	t.Logf("sent: %v", got)
	if !slices.Equal(got[0], []string{"/work/p", "/work/p/f.txt"}) {
		t.Fatalf("got %v", got)
	}
}
