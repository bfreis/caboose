package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeAttention is a source whose answers a test sets, and which counts
// its calls; while forbid is set any call fails the test.
type fakeAttention struct {
	t *testing.T

	mu                    sync.Mutex
	cheap                 []Finding
	envs                  []string
	slow                  map[string][]Finding
	slowErr               error
	cheapBlock, cheapDone bool
	slowBlock             map[string]bool // envs whose Slow blocks until its context ends
	update                []Finding
	forbid                bool
	nCheap, nSlow, nUpdat int
}

func (f *fakeAttention) enter() {
	f.t.Helper()
	if f.forbid {
		f.t.Error("a source was called while forbidden")
	}
}

func (f *fakeAttention) Cheap(ctx context.Context) []Finding {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter()
	f.nCheap++
	if f.cheapBlock {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		f.cheapDone = true
		return nil
	}
	return append([]Finding(nil), f.cheap...)
}

func (f *fakeAttention) SlowEnvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.envs...)
}

func (f *fakeAttention) Slow(ctx context.Context, env string) ([]Finding, error) {
	f.mu.Lock()
	if f.slowBlock[env] {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer f.mu.Unlock()
	f.enter()
	f.nSlow++
	return append([]Finding(nil), f.slow[env]...), f.slowErr
}

func (f *fakeAttention) Update(context.Context) ([]Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter()
	f.nUpdat++
	return append([]Finding(nil), f.update...), nil
}

func (f *fakeAttention) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeAttention) counts() (c, s, u int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nCheap, f.nSlow, f.nUpdat
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)} }

func kinds(items []daemonproto.AttentionItem) string {
	var s []string
	for _, i := range items {
		s = append(s, i.Env+"/"+i.Kind)
	}
	return strings.Join(s, ",")
}

func TestAttentionKindsAppearAndLeave(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t, envs: []string{"a"}, slow: map[string][]Finding{}}
	st := newAttentionState(src, clk.now, nil)
	ctx := context.Background()
	if got := st.items(); got == nil || len(got) != 0 {
		t.Fatalf("empty cache: %#v", got)
	}
	every := []Finding{
		{"a", daemonproto.AttentionRequests, "2 pending requests", "requests/a"},
		{"a", daemonproto.AttentionProposals, "1 pending proposal", daemonproto.RouteProposals},
		{"a", daemonproto.AttentionLinkFailed, "the link failed", "env/a"},
		{"", daemonproto.AttentionVnodes, "many vnodes", ""},
		{"a", daemonproto.AttentionCheckpoint, "last error", "env/a"},
	}
	src.set(func() {
		src.cheap = every
		src.slow["a"] = []Finding{{"a", daemonproto.AttentionImageStale, "image out of date", "env/a"}}
		src.update = []Finding{{"", daemonproto.AttentionUpdate, "v2 is out", ""}}
	})
	st.pollCheap(ctx)
	st.pollSlow(ctx)
	items := st.items()
	if len(items) != 7 {
		t.Fatalf("got %d: %s", len(items), kinds(items))
	}
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.Kind] = true
		if err := it.Validate(); err != nil {
			t.Errorf("%+v: %v", it, err)
		}
	}
	for _, k := range daemonproto.AttentionKinds() {
		if !seen[k] {
			t.Errorf("kind %s missing", k)
		}
	}
	// Each leaves when its source resolves.
	src.set(func() { src.cheap = every[:1]; src.slow["a"] = nil; src.update = nil })
	clk.add(AttentionSlowEvery)
	st.pollCheap(ctx)
	st.pollSlow(ctx)
	if got := kinds(st.items()); got != "a/requests" {
		t.Errorf("after resolving: %s", got)
	}
	src.set(func() { src.cheap = nil })
	st.pollCheap(ctx)
	if len(st.items()) != 0 {
		t.Errorf("all resolved: %s", kinds(st.items()))
	}
}

func TestAttentionSinceKept(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t}
	st := newAttentionState(src, clk.now, nil)
	ctx := context.Background()
	first := clk.now()
	src.set(func() { src.cheap = []Finding{{"a", daemonproto.AttentionRequests, "1 pending request", ""}} })
	st.pollCheap(ctx)
	clk.add(time.Minute)
	src.set(func() { src.cheap = []Finding{{"a", daemonproto.AttentionRequests, "3 pending requests", ""}} })
	st.pollCheap(ctx)
	it := st.items()[0]
	if !it.Since.Equal(first) || it.Text != "3 pending requests" {
		t.Errorf("since or text moved: %+v", it)
	}
	// It leaves and comes back: new.
	src.set(func() { src.cheap = nil })
	st.pollCheap(ctx)
	clk.add(time.Minute)
	src.set(func() { src.cheap = []Finding{{"a", daemonproto.AttentionRequests, "1 pending request", ""}} })
	st.pollCheap(ctx)
	if got := st.items()[0].Since; !got.Equal(clk.now()) {
		t.Errorf("a returning item kept its old since: %v", got)
	}
}

func TestAttentionSlowAtMostEveryTenMinutes(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t, envs: []string{"a", "b"}, slow: map[string][]Finding{
		"a": {{"a", daemonproto.AttentionImageStale, "stale", "env/a"}},
	}, update: []Finding{{"", daemonproto.AttentionUpdate, "v2", ""}}}
	st := newAttentionState(src, clk.now, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		st.pollSlow(ctx)
		clk.add(2 * time.Second)
	}
	if _, s, u := src.counts(); s != 2 || u != 1 {
		t.Fatalf("slow %d, update %d after 5 polls", s, u)
	}
	clk.add(AttentionSlowEvery - 11*time.Second) // 9m59s since the first
	st.pollSlow(ctx)
	if _, s, u := src.counts(); s != 2 || u != 1 {
		t.Fatalf("asked before 10 min: slow %d, update %d", s, u)
	}
	clk.add(time.Second)
	st.pollSlow(ctx)
	if _, s, u := src.counts(); s != 4 || u != 2 {
		t.Fatalf("not asked at 10 min: slow %d, update %d", s, u)
	}
	// A new environment is asked at once; a failure keeps the last answer
	// and waits its turn.
	src.set(func() { src.envs = []string{"a", "b", "c"}; src.slowErr = errors.New("engine down") })
	st.pollSlow(ctx)
	if _, s, _ := src.counts(); s != 5 {
		t.Fatalf("new env not asked: %d", s)
	}
	if got := kinds(st.items()); !strings.Contains(got, "a/image_stale") {
		t.Errorf("a failed ask dropped the last answer: %s", got)
	}
	st.pollSlow(ctx)
	if _, s, _ := src.counts(); s != 5 {
		t.Errorf("a failed ask was retried at once: %d", s)
	}
	// An environment that is gone leaves with its items.
	src.set(func() { src.envs = []string{"b"}; src.slowErr = nil })
	clk.add(AttentionSlowEvery)
	st.pollSlow(ctx)
	if got := kinds(st.items()); got != "/update" {
		t.Errorf("after env a left: %s", got)
	}
}

func TestAttentionCapAndOrder(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t}
	st := newAttentionState(src, clk.now, nil)
	ctx := context.Background()
	var found []Finding
	// 70 items over 7 polls, ten new at each.
	for round := 0; round < 7; round++ {
		for i := 0; i < 10; i++ {
			found = append(found, Finding{fmt.Sprintf("e%d-%d", round, i), daemonproto.AttentionRequests, "x", ""})
		}
		src.set(func() { src.cheap = found })
		st.pollCheap(ctx)
		clk.add(time.Second)
	}
	items := st.items()
	if len(items) != daemonproto.MaxAttention {
		t.Fatalf("%d items", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i].Since.After(items[i-1].Since) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if items[0].Env[:2] != "e6" {
		t.Errorf("newest is %s", items[0].Env)
	}
	if st.items()[0].Env != items[0].Env {
		t.Error("order not stable")
	}
	// The oldest were cut, not the newest: round 0 has 10 and 6 are cut.
	for _, it := range items {
		if strings.HasPrefix(it.Env, "e0-") && it.Env > "e0-3" {
			t.Errorf("an old item survived the cut: %s", it.Env)
		}
	}
}

func TestAttentionDropsBadFindings(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t}
	st := newAttentionState(src, clk.now, nil)
	src.set(func() {
		src.cheap = []Finding{
			{"a", "nope", "x", ""},
			{"a", daemonproto.AttentionRequests, "esc\x1b[2J", "env/a"},
			{"a", daemonproto.AttentionProposals, "x", "https://evil"},
			{"", daemonproto.AttentionRequests, "no env", ""},
			{"a", daemonproto.AttentionLinkFailed, strings.Repeat("y", 5000), ""},
		}
	})
	st.pollCheap(context.Background())
	items := st.items()
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Errorf("%+v: %v", it, err)
		}
	}
	// The escape is cleaned and the long text cut, so those two stay;
	// the unknown kind, the bad route and the missing env go.
	if got := kinds(items); got != "a/link_failed,a/requests" && got != "a/requests,a/link_failed" {
		t.Errorf("kept: %s", got)
	}
}

// status serves the cache and never calls a source.
func TestStatusNeverCallsASource(t *testing.T) {
	src := &fakeAttention{t: t, envs: []string{"a"}, slow: map[string][]Finding{
		"a": {{"a", daemonproto.AttentionImageStale, "image out of date", "env/a"}},
	}, cheap: []Finding{{"a", daemonproto.AttentionRequests, "1 pending request", "requests/a"}},
		update: []Finding{{"", daemonproto.AttentionUpdate, "v2 is out", ""}}}
	dir, _ := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Attention: src, AttentionEvery: time.Hour})
	// Wait for the first round.
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, s, u := src.counts()
		if c > 0 && s > 0 && u > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var st *daemonproto.StatusResult
	waitFor := time.Now().Add(20 * time.Second)
	for {
		c := dialAs(t, dir, daemonproto.ClientCLI, daemonproto.ProtocolVersion)
		var err error
		src.set(func() { src.forbid = true })
		st, err = c.Status(context.Background())
		src.set(func() { src.forbid = false })
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Attention) == 3 || time.Now().After(waitFor) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(st.Attention) != 3 {
		t.Fatalf("status attention: %s", kinds(st.Attention))
	}
	for i := 0; i < 5; i++ {
		src.set(func() { src.forbid = true })
		c := dialAs(t, dir, daemonproto.ClientCLI, daemonproto.ProtocolVersion)
		_, err := c.Status(context.Background())
		src.set(func() { src.forbid = false })
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusWithoutASource(t *testing.T) {
	dir, _ := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}})
	c := dialAs(t, dir, daemonproto.ClientCLI, daemonproto.ProtocolVersion)
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Attention) != 0 {
		t.Errorf("%+v", st.Attention)
	}
}

// A slow ask that ignores its context holds nothing up: the update and the
// other environments are still answered, and the ask ends at its timeout.
func TestAttentionHungEngineHoldsNothingUp(t *testing.T) {
	old := attentionSlowTimeout
	// Long enough that the asks that answer at once never lose to it on
	// a loaded machine; only the hung one waits it out.
	attentionSlowTimeout = time.Second
	t.Cleanup(func() { attentionSlowTimeout = old })
	src := &fakeAttention{t: t, envs: []string{"a", "b"}, slowBlock: map[string]bool{"a": true},
		slow:   map[string][]Finding{"b": {{"b", daemonproto.AttentionImageStale, "stale", "env/b"}}},
		update: []Finding{{"", daemonproto.AttentionUpdate, "v2", ""}}}
	st := newAttentionState(src, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(20 * time.Second)
	for kinds(st.items()) != "/update,b/image_stale" && kinds(st.items()) != "b/image_stale,/update" {
		if time.Now().After(deadline) {
			t.Fatalf("got %s", kinds(st.items()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the loops did not end with their context")
	}
}

// Stopping the daemon waits for the attention loops, which end with the
// daemon's context.
func TestAttentionLoopsEndWithTheDaemon(t *testing.T) {
	src := &fakeAttention{t: t, cheapBlock: true}
	h := newHarness(t)
	stop := runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Attention: src, AttentionEvery: time.Hour})
	waitFor(t, 20*time.Second, "the source is asked", func() bool { c, _, _ := src.counts(); return c > 0 })
	stop()
	src.mu.Lock()
	done := src.cheapDone
	src.mu.Unlock()
	if !done {
		t.Error("the daemon stopped before its attention loop ended")
	}
}

// A failed ask keeps the environment's earlier answer.
func TestAttentionSlowErrorKeepsLastAnswer(t *testing.T) {
	clk := newClock()
	src := &fakeAttention{t: t, envs: []string{"a"}, slow: map[string][]Finding{
		"a": {{"a", daemonproto.AttentionImageStale, "stale", "env/a"}}}}
	st := newAttentionState(src, clk.now, nil)
	st.pollImages(context.Background())
	if kinds(st.items()) != "a/image_stale" {
		t.Fatal(kinds(st.items()))
	}
	src.set(func() { src.slowErr = errors.New("down"); src.slow["a"] = nil })
	clk.add(AttentionSlowEvery)
	st.pollImages(context.Background())
	if kinds(st.items()) != "a/image_stale" {
		t.Errorf("an error dropped the answer: %s", kinds(st.items()))
	}
}

// pollSlow runs the two slow polls, which the daemon runs on loops of their
// own, one after the other: a test's one step for both.
func (a *attentionState) pollSlow(ctx context.Context) {
	a.pollImages(ctx)
	a.pollUpdate(ctx)
}
