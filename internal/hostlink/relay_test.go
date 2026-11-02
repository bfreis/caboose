package hostlink

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

func testRelay(states map[string]fileState) *relay {
	r := newRelay([]Root{{Host: "/Users/me/dev", Container: "/work"}, {Host: "/Users/me/other", Container: "/work/other"}})
	r.stat = func(p string) fileState { return states[p] }
	return r
}

// batch is split's paths: the ones the agent touches.
func (r *relay) batch(hostPaths []string) []string {
	paths, _ := r.split(hostPaths)
	return paths
}

func TestRelayMapsAndFilters(t *testing.T) {
	file := fileState{exists: true, mode: 0o644, size: 1, mtime: time.Unix(1, 0)}
	r := testRelay(map[string]fileState{
		"/Users/me/dev/a/x.go":    file,
		"/Users/me/other/y.go":    file,
		"/Users/me/dev/.git/HEAD": file,
	})
	got := r.batch([]string{
		"/Users/me/dev/a/x.go",
		"/Users/me/other/y.go",
		"/Users/me/dev/.git/HEAD",  // git's own churn
		"/Users/me/elsewhere/z.go", // under no root
		"/Users/me/devx/z.go",      // a sibling, not under the root
		"/Users/me/dev/gone.go",    // deleted: still sent
	})
	want := []string{"/work/a/x.go", "/work/gone.go", "/work/other/y.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("batch = %v, want %v", got, want)
	}
}

// The agent's touch comes back as a change to a path whose state has not
// changed: it is not sent again, or the relay would loop. A real change
// after it is.
func TestRelayDropsEchoes(t *testing.T) {
	states := map[string]fileState{"/Users/me/dev/x.go": {exists: true, mode: 0o644, size: 1, mtime: time.Unix(1, 0)}}
	r := testRelay(states)
	if got := r.batch([]string{"/Users/me/dev/x.go"}); len(got) != 1 {
		t.Fatalf("first change: %v", got)
	}
	if got := r.batch([]string{"/Users/me/dev/x.go"}); len(got) != 0 {
		t.Fatalf("the echo of a touch was sent again: %v", got)
	}
	states["/Users/me/dev/x.go"] = fileState{exists: true, mode: 0o644, size: 2, mtime: time.Unix(2, 0)}
	if got := r.batch([]string{"/Users/me/dev/x.go"}); len(got) != 1 {
		t.Fatalf("a real change after an echo was dropped: %v", got)
	}
	delete(states, "/Users/me/dev/x.go")
	if got := r.batch([]string{"/Users/me/dev/x.go"}); len(got) != 1 {
		t.Fatalf("a deletion was dropped: %v", got)
	}
	if got := r.batch([]string{"/Users/me/dev/x.go"}); len(got) != 0 {
		t.Fatalf("a deletion was sent twice: %v", got)
	}
}

// A deleted file's directory, and a directory sent for a burst, are what
// the agent touches: their echoes are not sent either.
func TestRelayDropsDirectoryEchoes(t *testing.T) {
	dir := fileState{exists: true, mode: fs.ModeDir | 0o755, mtime: time.Unix(1, 0)}
	states := map[string]fileState{"/Users/me/dev/d": dir, "/Users/me/dev/big": dir}
	r := testRelay(states)
	if got := r.batch([]string{"/Users/me/dev/d/gone.go"}); !slices.Equal(got, []string{"/work/d/gone.go"}) {
		t.Fatalf("deletion: %v", got)
	}
	if got := r.batch([]string{"/Users/me/dev/d"}); len(got) != 0 {
		t.Fatalf("the touched directory of a deletion was sent back: %v", got)
	}
	var many []string
	for i := range relayPerDir + 1 {
		many = append(many, fmt.Sprintf("/Users/me/dev/big/f%d", i))
	}
	if got := r.batch(many); !slices.Equal(got, []string{"/work/big"}) {
		t.Fatalf("burst: %v", got)
	}
	if got := r.batch([]string{"/Users/me/dev/big"}); len(got) != 0 {
		t.Fatalf("the touched directory of a burst was sent back: %v", got)
	}
}

func TestRelayForgets(t *testing.T) {
	now := time.Unix(1000, 0)
	r := testRelay(map[string]fileState{})
	r.now = func() time.Time { return now }
	r.batch([]string{"/Users/me/dev/x.go"})
	now = now.Add(2 * relayForget)
	if got := r.batch([]string{"/Users/me/dev/y.go"}); len(got) != 1 {
		t.Fatalf("after relayForget: sent %v", got)
	}
	if _, kept := r.seen["/Users/me/dev/x.go"]; kept {
		t.Fatal("a path sent relayForget ago is still kept")
	}
}

func TestRelayCoalesces(t *testing.T) {
	file := fileState{exists: true, mode: 0o644, size: 1, mtime: time.Unix(1, 0)}
	states := map[string]fileState{}
	r := testRelay(states)
	var many, wantGone []string
	for i := range relayPerDir + 1 {
		p := fmt.Sprintf("/Users/me/dev/big/f%03d", i)
		many = append(many, p)
		// Every other one is gone.
		if i%2 == 0 {
			states[p] = file
		} else {
			wantGone = append(wantGone, fmt.Sprintf("/work/big/f%03d", i))
		}
	}
	got, gone := r.split(append(many, "/Users/me/dev/small/a"))
	if want := []string{"/work/big", "/work/small/a"}; !slices.Equal(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	// The directory stands for the gone ones too, but the agent has to
	// look each up for a guest that holds one to let go of it. A gone one
	// sent among the paths is not sent again.
	if !slices.Equal(gone, wantGone) {
		t.Fatalf("gone = %v, want the %d gone in the coalesced directory", gone, len(wantGone))
	}

	r = testRelay(map[string]fileState{})
	many = nil
	for i := range relayPerBatch + 1 {
		many = append(many, fmt.Sprintf("/Users/me/dev/d%05d/f", i))
	}
	got, gone = r.split(many)
	if want := []string{"/work", "/work/other"}; !slices.Equal(got, want) {
		t.Fatalf("an oversized batch = %v, want the roots", got)
	}
	if len(gone) != relayGonePerBatch || gone[0] != "/work/d00000/f" || gone[len(gone)-1] != fmt.Sprintf("/work/d%05d/f", relayGonePerBatch-1) {
		t.Fatalf("an oversized batch's gone: %d paths, want the first %d", len(gone), relayGonePerBatch)
	}
	// However many, they fit one message beside the paths of the batch.
	if msgs := changedMessages(nil, gone); len(msgs) != 1 {
		t.Fatalf("the gone of a batch take %d messages, want 1", len(msgs))
	}
}

func TestChangedMessagesFitAFrame(t *testing.T) {
	var paths []string
	for i := range 3000 {
		// Control characters take six bytes each once encoded.
		paths = append(paths, fmt.Sprintf("/work/%d/%s", i, strings.Repeat("\x01", 100)))
	}
	gone := []string{}
	for i := range 1000 {
		gone = append(gone, fmt.Sprintf("/work/gone/%d", i))
	}
	msgs := changedMessages(paths, gone)
	var all, allGone []string
	for _, m := range msgs {
		b, err := agentproto.Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > agentproto.MaxPayload || len(m.Paths)+len(m.Gone) > agentproto.MaxChanged {
			t.Fatalf("a message of %d bytes, %d paths and %d gone", len(b), len(m.Paths), len(m.Gone))
		}
		all = append(all, m.Paths...)
		allGone = append(allGone, m.Gone...)
	}
	if !slices.Equal(all, paths) || !slices.Equal(allGone, gone) {
		t.Fatal("paths were lost or reordered across messages")
	}
}
