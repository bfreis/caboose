package daemon

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// What needs the user is computed here, in the background, and cached:
// status serves the cache and never calls a source, so it never waits on
// the engine or a disk.
const (
	// DefaultAttentionEvery is how often the cheap kinds are computed.
	DefaultAttentionEvery = 2 * time.Second
	// AttentionSlowEvery is the least time between two computations of
	// an environment's slow kinds (image_stale), or of the update kind.
	AttentionSlowEvery = 10 * time.Minute
)

// attentionSlowTimeout bounds one slow computation; a variable for the
// tests.
var attentionSlowTimeout = time.Minute

// Finding is one thing a source says needs the user. The daemon adds
// when it began (Since) and fits the list to its bounds; the text is
// host-written, and any sandbox-influenced part of it is cut and made
// printable by the source.
type Finding struct {
	Env, Kind, Text, Route string
}

// Attention is where the daemon gets each kind of thing that needs the
// user. A kind whose source does not exist is left out, never faked.
// The daemon calls Cheap every poll and the others at most every
// AttentionSlowEvery, never from a client's request.
type Attention interface {
	// Cheap is the kinds that are cheap to find: requests, proposals,
	// link_failed, vnodes, and checkpoint.
	Cheap(ctx context.Context) []Finding
	// SlowEnvs are the environments Slow is asked about.
	SlowEnvs() []string
	// Slow is env's kinds that may ask the engine (image_stale). An
	// error leaves env's last answer in place.
	Slow(ctx context.Context, env string) ([]Finding, error)
	// Update is the update kind, or none. An error leaves the last answer.
	Update(ctx context.Context) ([]Finding, error)
}

type slowEntry struct {
	at    time.Time
	found []Finding
}

type attentionKey struct{ env, kind string }

// attentionState is the cache.
type attentionState struct {
	src Attention
	now func() time.Time
	log *log.Logger

	mu    sync.Mutex
	cheap []Finding
	slow  map[string]slowEntry
	upd   slowEntry
	since map[attentionKey]time.Time
	list  []daemonproto.AttentionItem
}

func newAttentionState(src Attention, now func() time.Time, lg *log.Logger) *attentionState {
	if now == nil {
		now = time.Now
	}
	return &attentionState{src: src, now: now, log: lg, slow: map[string]slowEntry{}, since: map[attentionKey]time.Time{}}
}

// items is the cached list, newest first; the caller may keep it.
func (a *attentionState) items() []daemonproto.AttentionItem {
	if a == nil {
		return []daemonproto.AttentionItem{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]daemonproto.AttentionItem{}, a.list...)
}

// pollCheap computes the cheap kinds.
func (a *attentionState) pollCheap(ctx context.Context) {
	found := a.src.Cheap(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cheap = found
	a.rebuild()
}

// askBounded calls fn with a context that ends after attentionSlowTimeout
// and returns when fn does or that context ends, whichever is first: a
// source that ignores its context is left behind, and holds nothing up.
func askBounded(ctx context.Context, fn func(context.Context) ([]Finding, error)) ([]Finding, error) {
	cctx, cancel := context.WithTimeout(ctx, attentionSlowTimeout)
	defer cancel()
	type res struct {
		f   []Finding
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := fn(cctx); ch <- res{f, err} }()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-cctx.Done():
		return nil, cctx.Err()
	}
}

// pollImages asks about each environment whose slow kinds are due, each
// at most every AttentionSlowEvery.
func (a *attentionState) pollImages(ctx context.Context) {
	now := a.now()
	envs := a.src.SlowEnvs()
	a.mu.Lock()
	var due []string
	for _, env := range envs {
		if e, ok := a.slow[env]; !ok || now.Sub(e.at) >= AttentionSlowEvery {
			due = append(due, env)
		}
	}
	a.mu.Unlock()
	for _, env := range due {
		f, err := askBounded(ctx, func(c context.Context) ([]Finding, error) { return a.src.Slow(c, env) })
		a.mu.Lock()
		e := a.slow[env] // a failure keeps the last answer, and waits its turn
		e.at = now
		if err != nil {
			a.logf("attention for %s: %v", env, err)
		} else {
			e.found = f
		}
		a.slow[env] = e
		a.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for env := range a.slow {
		if !slices.Contains(envs, env) {
			delete(a.slow, env)
		}
	}
	a.rebuild()
}

// pollUpdate asks about the update when due; it never waits on the images.
func (a *attentionState) pollUpdate(ctx context.Context) {
	now := a.now()
	a.mu.Lock()
	due := a.upd.at.IsZero() || now.Sub(a.upd.at) >= AttentionSlowEvery
	a.mu.Unlock()
	if !due {
		return
	}
	f, err := askBounded(ctx, a.src.Update)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.upd.at = now
	if err != nil {
		a.logf("attention for updates: %v", err)
	} else {
		a.upd.found = f
	}
	a.rebuild()
}

func (a *attentionState) logf(format string, args ...any) {
	if a.log != nil {
		a.log.Printf(format, args...)
	}
}

// rebuild makes the list from the caches, under mu: an item keeps its
// Since while its environment and kind stay in it, and one that left is
// forgotten, so that it is new if it comes back.
func (a *attentionState) rebuild() {
	now := a.now()
	all := append([]Finding{}, a.cheap...)
	for _, e := range a.slow {
		all = append(all, e.found...)
	}
	all = append(all, a.upd.found...)
	seen := map[attentionKey]bool{}
	var list []daemonproto.AttentionItem
	for _, f := range all {
		k := attentionKey{f.Env, f.Kind}
		if seen[k] {
			continue
		}
		it := daemonproto.AttentionItem{Env: f.Env, Kind: f.Kind, Text: daemonproto.Text(f.Text, daemonproto.MaxAttentionText), Route: f.Route}
		if err := it.Validate(); err != nil {
			a.logf("attention: dropped a %s item for %q: %v", f.Kind, f.Env, err)
			continue
		}
		seen[k] = true
		at, ok := a.since[k]
		if !ok {
			at = now
		}
		a.since[k] = at
		it.Since = at
		list = append(list, it)
	}
	for k := range a.since {
		if !seen[k] {
			delete(a.since, k)
		}
	}
	slices.SortStableFunc(list, func(x, y daemonproto.AttentionItem) int {
		if c := y.Since.Compare(x.Since); c != 0 {
			return c
		}
		if c := strings.Compare(x.Env, y.Env); c != 0 {
			return c
		}
		return strings.Compare(x.Kind, y.Kind)
	})
	if len(list) > daemonproto.MaxAttention {
		list = list[:daemonproto.MaxAttention]
	}
	a.list = list
}

// run polls until ctx ends: the cheap kinds every every, the slow ones
// when due (checked every poll), in a goroutine of their own so that the
// engine never holds the cheap ones, or the update, up.
func (a *attentionState) run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultAttentionEvery
	}
	loop := func(poll func(context.Context)) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			poll(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
	var wg sync.WaitGroup
	for _, poll := range []func(context.Context){a.pollCheap, a.pollImages, a.pollUpdate} {
		wg.Add(1)
		go func() { defer wg.Done(); loop(poll) }()
	}
	wg.Wait()
}
