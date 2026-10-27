package broker

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// hostileText is text a sandbox might send: control and format
// characters, invalid UTF-8, long runs.
func hostileText(r *rand.Rand, max int) string {
	pieces := []string{"a", "Z", " ", "\n", "\t", "\x00", "\x1b[2J", "\u202e", "\u2066", "\u2028", "\ufeff", "\xff\xfe", "é", "🙂", "#1", "allowed", "\r", "\\u0041", "\"", "}"}
	var b strings.Builder
	n := r.IntN(max + 1)
	for b.Len() < n {
		if r.IntN(10) == 0 {
			b.WriteString(strings.Repeat("x", r.IntN(600)))
		}
		b.WriteString(pieces[r.IntN(len(pieces))])
	}
	return b.String()
}

func randomRequest(r *rand.Rand) policy.Request {
	hosts := []string{"db.corp.example", "api.example.com", "localhost", "evil.example", "x"}
	addrs := []string{"10.0.0.5", "10.0.0.6", "93.184.216.34", "127.0.0.1", "169.254.1.1", "::1", "fd00::1"}
	switch r.IntN(7) {
	case 0:
		names := []string{"build", "free", "nope", ""}
		req := aliasReq(names[r.IntN(len(names))])
		if r.IntN(3) == 0 {
			req.Alias = hostileText(r, 40)
		}
		if r.IntN(4) == 0 {
			req.Args = map[string]string{hostileText(r, 10): hostileText(r, 10)}
		}
		if r.IntN(5) == 0 {
			req.Command = "/work/" + hostileText(r, 10)
		}
		return req
	case 1, 2:
		req := egressReq(hosts[r.IntN(len(hosts))], addrs[r.IntN(len(addrs))])
		if r.IntN(3) == 0 {
			req.Addrs = append(req.Addrs, netip.MustParseAddr(addrs[r.IntN(len(addrs))]))
		}
		if r.IntN(4) == 0 {
			req.Port = []int{22, 80, 8080, 0, -1, 70000}[r.IntN(6)]
		}
		if r.IntN(5) == 0 {
			req.Host = hostileText(r, 50)
		}
		return req
	case 3:
		urls := []string{"https://example.com/a", "https://other.example/", "http://localhost:3000/x"}
		req := urlReq(urls[r.IntN(len(urls))])
		if r.IntN(4) == 0 {
			req.URL = hostileText(r, 100)
		}
		return req
	case 4:
		return policy.Request{Kind: policy.KindHostExec}
	case 5:
		return policy.Request{Kind: policy.KindPort, Port: []int{22, 80, 1024, 3000, 4321, 7171, 0, -1, 70000}[r.IntN(9)], HostChecked: r.IntN(2) == 0, HostListens: r.IntN(4) == 0}
	}
	return policy.Request{Kind: policy.Kind(hostileText(r, 20)), Alias: hostileText(r, 20), Host: hostileText(r, 20)}
}

func randomAsk(r *rand.Rand) Ask {
	a := Ask{Request: randomRequest(r)}
	if r.IntN(2) == 0 {
		a.Context = hostileText(r, MaxContext+200)
	}
	if r.IntN(3) == 0 {
		for range r.IntN(MaxOptions + 2) {
			a.Options = append(a.Options, hostileText(r, MaxOption+20))
		}
	}
	if r.IntN(3) == 0 {
		a.Recommendation = hostileText(r, MaxRecommendation+20)
	}
	if r.IntN(3) == 0 {
		a.Meanwhile = hostileText(r, MaxMeanwhile+20)
	}
	if r.IntN(2) == 0 {
		a.Session = hostileText(r, MaxSession+10)
	}
	return a
}

// Whatever the sandbox does through Decide, File, List and Wait, no grant
// is added or extended, config.toml is never written, nothing is opened,
// and no request is decided: only a once grant may be consumed.
func TestPropertySandboxNeverWritesPolicy(t *testing.T) {
	r := rand.New(rand.NewPCG(20261018, 2))
	root := t.TempDir()
	for seq := range 1000 {
		home, err := os.MkdirTemp(root, "home")
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{t: t, home: home, dir: config.EnvDirFor(home, testEnv), owner: &owner{}, clock: newClock(),
			src: &source{cfg: baseConfig()}, open: &opener{}, dialog: &dialog{}}
		f.w = &writer{src: f.src}
		if r.IntN(2) == 0 {
			f.src.cfg.Trust = config.TrustHostile
		}
		f.b = f.openBroker()
		f.b.maxPerMinute = 1000

		// The host's own grants and decisions, before the sandbox acts.
		if id := f.decide(aliasReq("build")).Request; id != 0 {
			f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1 + r.IntN(5)})
		}
		if id := f.decide(egressReq("db.corp.example", "10.0.0.5")).Request; id != 0 {
			f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
		}
		if id := f.decide(urlReq("https://other.example/")).Request; id != 0 {
			f.answer(id, Reply{Verdict: ReplyDeny})
		}
		f.b.mu.Lock()
		grantsBefore := slices.Clone(f.b.st.grants.Grants)
		nextGrant := f.b.st.grants.NextID
		decidedBefore := map[uint64]string{}
		for _, q := range f.b.st.reqs.Requests {
			decidedBefore[q.ID] = q.State
		}
		f.b.mu.Unlock()
		diffs, writes := f.w.calls()
		opened := len(f.open.opened())
		link := f.b.Link()

		var ops []string
		for range 5 + r.IntN(15) {
			f.clock.Add(time.Duration(r.IntN(600)) * time.Second)
			switch r.IntN(4) {
			case 0:
				req := randomRequest(r)
				ops = append(ops, fmt.Sprintf("Decide %q", req.Kind))
				o, err := link.Decide(req)
				if err != nil {
					t.Fatal(err)
				}
				if req.Kind == policy.KindPort && o.Request != 0 {
					t.Fatalf("a Decide of a port filed request #%d", o.Request)
				}
			case 1:
				a := randomAsk(r)
				ops = append(ops, fmt.Sprintf("File %q", a.Request.Kind))
				if _, err := link.File(a); err != nil {
					t.Fatal(err)
				}
			case 2:
				ops = append(ops, "List")
				if _, _, err := link.List(Query{Session: hostileText(r, 20), All: r.IntN(2) == 0, Since: r.Uint64N(20), Limit: r.IntN(300) - 50}); err != nil {
					t.Fatal(err)
				}
			case 3:
				ops = append(ops, "Wait")
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := link.Wait(ctx, r.Uint64()); err != nil {
					t.Fatal(err)
				}
			}
		}

		f.b.mu.Lock()
		after := slices.Clone(f.b.st.grants.Grants)
		nextAfter := f.b.st.grants.NextID
		reqs := slices.Clone(f.b.st.reqs.Requests)
		f.b.mu.Unlock()
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("sequence %d (%s): %s", seq, strings.Join(ops, ", "), fmt.Sprintf(format, args...))
		}
		if nextAfter != nextGrant {
			fail("grant numbers moved: %d to %d", nextGrant, nextAfter)
		}
		for _, g := range after {
			i := slices.IndexFunc(grantsBefore, func(b grantRec) bool { return b.ID == g.ID })
			if i < 0 {
				fail("grant %+v added", g)
			}
			b := grantsBefore[i]
			if !b.Expires.Equal(g.Expires) || b.Key != g.Key || b.Kind != g.Kind || b.Once != g.Once || !slices.Equal(b.Addrs, g.Addrs) {
				fail("grant %+v changed to %+v", b, g)
			}
		}
		for _, b := range grantsBefore {
			if !slices.ContainsFunc(after, func(g grantRec) bool { return g.ID == b.ID }) && !b.Once {
				fail("timed grant %+v removed", b)
			}
		}
		if d, w := f.w.calls(); d != diffs || w != writes {
			fail("config writer called: %d diffs, %d writes", d-diffs, w-writes)
		}
		if len(f.open.opened()) != opened {
			fail("a URL was opened")
		}
		for _, q := range reqs {
			if st, ok := decidedBefore[q.ID]; ok {
				if st != q.State {
					fail("request #%d went from %s to %s", q.ID, st, q.State)
				}
			} else if q.State != StatePending || q.DecidedBy != "" {
				fail("request #%d is %s by %q", q.ID, q.State, q.DecidedBy)
			}
		}
		f.b.Close()
	}
}

// Two Brokers on one environment, both writing, never lose a grant or
// reuse a number: every read-modify-write is under broker.lock.
func TestTwoWritersUnderLock(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.Trust = config.TrustHostile })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	second := f.openBroker()
	brokers := []*Broker{f.b, second}
	for _, b := range brokers {
		b.maxPerMinute, b.maxPending = 1000, 1000
	}
	const n = 60
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = map[uint64]bool{}
		gids = map[string]bool{}
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := brokers[i%2].Decide(egressReq(fmt.Sprintf("h%d.example.com", i), "93.184.216.34"))
			if err != nil || o.Verdict != Pending {
				t.Errorf("%d: %+v %v", i, o, err)
				return
			}
			res, err := brokers[(i+1)%2].Answer(o.Request, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1, By: ByCLI})
			if err != nil {
				t.Errorf("answer #%d: %v", o.Request, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ids[o.Request] || gids[res.Grant] {
				t.Errorf("number reused: #%d %s", o.Request, res.Grant)
			}
			ids[o.Request], gids[res.Grant] = true, true
		}()
	}
	wg.Wait()
	f.reopen()
	g, err := f.b.Grants()
	if err != nil || len(g) != n {
		t.Fatalf("%d grants of %d: %v", len(g), n, err)
	}
	list, _, _ := f.b.List(Query{All: true})
	if len(list) != n {
		t.Fatalf("%d requests of %d", len(list), n)
	}
	for _, ri := range list {
		if ri.ID < 1 || ri.ID > n || ri.State != StateAllowed {
			t.Fatalf("%+v", ri)
		}
	}
	if f.b.st.grants.NextID != n+1 || f.b.st.reqs.NextID != n+1 {
		t.Fatalf("next ids %d %d", f.b.st.grants.NextID, f.b.st.reqs.NextID)
	}
}
