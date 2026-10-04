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
	r := testRelay(map[string]fileState{})
	var many []string
	for i := range relayPerDir + 1 {
		many = append(many, fmt.Sprintf("/Users/me/dev/big/f%d", i))
	}
	got := r.batch(append(many, "/Users/me/dev/small/a"))
	if want := []string{"/work/big", "/work/small/a"}; !slices.Equal(got, want) {
		t.Fatalf("batch = %v, want %v", got, want)
	}

	r = testRelay(map[string]fileState{})
	many = nil
	for i := range relayPerBatch + 1 {
		many = append(many, fmt.Sprintf("/Users/me/dev/d%d/f", i))
	}
	got = r.batch(many)
	if want := []string{"/work", "/work/other"}; !slices.Equal(got, want) {
		t.Fatalf("an oversized batch = %v, want the roots", got)
	}
}

func TestChangedMessagesFitAFrame(t *testing.T) {
	var paths []string
	for i := range 3000 {
		// Control characters take six bytes each once encoded.
		paths = append(paths, fmt.Sprintf("/work/%d/%s", i, strings.Repeat("\x01", 100)))
	}
	msgs := changedMessages(paths)
	var all []string
	for _, m := range msgs {
		b, err := agentproto.Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > agentproto.MaxPayload || len(m.Paths) > agentproto.MaxChanged {
			t.Fatalf("a message of %d bytes, %d paths", len(b), len(m.Paths))
		}
		all = append(all, m.Paths...)
	}
	if !slices.Equal(all, paths) {
		t.Fatal("paths were lost or reordered across messages")
	}
}
