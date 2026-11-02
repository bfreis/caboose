package broker

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// resizer is a fake Resizer: it records each resize asked of it, and
// fails to start one while err is set.
type resizer struct {
	mu    sync.Mutex
	calls []Resize
	err   error
}

func (r *resizer) ResizeVolume(rz Resize) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, rz)
	return nil
}

func (r *resizer) made() []Resize {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Resize(nil), r.calls...)
}

// volumeFixture is a fixture whose vm profile has volumes cache (64G) and
// data (100G), within the default volume_limit.
func volumeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.src.set(func(c *config.Config) {
		c.Volumes = []config.Volume{{Name: "cache", Path: "/c", Size: 64 << 30}, {Name: "data", Path: "/d", Size: 100 << 30}}
		c.VMVolumeLimit = config.DefaultVolumeLimit
	})
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	return f
}

// resizeReq is a resize of name to size, of a volume the running VM has
// live, at 64G now.
func resizeReq(name string, size int64) policy.Request {
	return policy.Request{Kind: policy.KindVolumeResize, Volume: name, VolumeSize: size, VolumeFrom: 64 << 30, VolumeLive: true, VolumeFree: 500 << 30}
}

// A resize the policy allows asks; a person allows it once, which starts
// the resize and makes no grant; what came of it is recorded on the
// request, with a new seq, for the sandbox to hear of.
func TestVolumeResizeAllowedOnce(t *testing.T) {
	f := volumeFixture(t)
	o, err := f.b.File(Ask{Request: resizeReq("cache", 128<<30), Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	id := mustPending(t, o)
	if o.Rule != policy.RuleAskVolumeResize {
		t.Errorf("rule %q", o.Rule)
	}
	ri, offers, withheld, err := f.b.RequestOffers(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ri.Effect, "from 64G to 128G") || !strings.Contains(ri.Effect, "500G") || ri.Key != "volume_resize:cache" {
		t.Errorf("request %+v", ri)
	}
	if len(offers) != 1 || offers[0].Scope != ScopeOnce || len(withheld) != 2 {
		t.Fatalf("offers %+v withheld %+v", offers, withheld)
	}
	for _, w := range withheld {
		if !strings.Contains(w.Why, "allowed once, each by a person") {
			t.Errorf("withheld %+v", w)
		}
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 2, By: ByCLI}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("hours: %v", err)
	}
	if got := f.resize.made(); len(got) != 0 {
		t.Fatalf("a refused answer resized: %+v", got)
	}
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if res.Grant != "" || f.grantCount() != 0 {
		t.Fatalf("a grant was made: %+v, %d", res, f.grantCount())
	}
	// The resize carries what the request was filed against: the disk's
	// size, config.toml's and the vm profile.
	want := Resize{ID: id, Volume: "cache", Size: 128 << 30, From: 64 << 30, Configured: 64 << 30, Profile: "vm.default"}
	if got := f.resize.made(); len(got) != 1 || got[0] != want {
		t.Fatalf("resizes %+v", got)
	}
	_, cur, err := f.b.List(Query{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.RecordOutcome(id, "volume cache is 128G now"); err != nil {
		t.Fatal(err)
	}
	reqs, _, err := f.b.List(Query{Since: cur})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].ID != id || !strings.Contains(reqs[0].Reason, "then: volume cache is 128G now") || reqs[0].Seq <= cur {
		t.Fatalf("after the outcome: %+v", reqs)
	}
	if !strings.Contains(string(f.read(decisionsFile)), `"event":"outcome"`) {
		t.Error("the outcome is not in decisions.log")
	}
	// A broker that no longer serves the environment records nothing.
	f.owner.lost.Store(true)
	if err := f.b.RecordOutcome(id, "late"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("not the owner: %v", err)
	}
	f.owner.lost.Store(false)
	if ri, _ := f.b.Request(id); strings.Contains(ri.Reason, "late") {
		t.Errorf("recorded by a broker that is not the owner: %+v", ri)
	}
	// The same resize again asks again: nothing was granted.
	o, err = f.b.File(Ask{Request: resizeReq("cache", 128<<30), Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if mustPending(t, o) == id {
		t.Fatal("merged into the answered request")
	}
}

// A denied resize starts nothing; an outcome is recorded only on an
// allowed resize.
func TestVolumeResizeDenied(t *testing.T) {
	f := volumeFixture(t)
	o, _ := f.b.File(Ask{Request: resizeReq("cache", 32<<30)})
	id := mustPending(t, o)
	f.answer(id, Reply{Verdict: ReplyDeny})
	if got := f.resize.made(); len(got) != 0 {
		t.Fatalf("a denied resize ran: %+v", got)
	}
	if err := f.b.RecordOutcome(id, "x"); err != nil {
		t.Fatal(err)
	}
	ri, _ := f.b.Request(id)
	if ri.State != StateDenied || strings.Contains(ri.Reason, "then:") {
		t.Fatalf("%+v", ri)
	}
}

// The policy's refusals file nothing and resize nothing.
func TestVolumeResizeRefused(t *testing.T) {
	f := volumeFixture(t)
	notLive := resizeReq("cache", 128<<30)
	notLive.VolumeLive = false
	for name, tc := range map[string]struct {
		r    policy.Request
		rule string
	}{
		"unknown":  {resizeReq("nope", 128<<30), "deny:volume-unknown"},
		"too big":  {resizeReq("cache", 2048<<30), "deny:volume-size"},
		"not live": {notLive, "deny:volume-not-live"},
		"same":     {resizeReq("cache", 64<<30), "deny:volume-same"},
		"limit":    {resizeReq("cache", 200<<30), policy.RuleVolumeLimit},
	} {
		o, err := f.b.File(Ask{Request: tc.r})
		if err != nil || o.Verdict != Denied || o.Rule != tc.rule {
			t.Errorf("%s: %+v %v", name, o, err)
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Errorf("%d requests filed", n)
	}
}

// What config.toml says when a person answers decides: a volume_limit
// lowered since the request was filed withholds the offer and refuses the
// answer, starting nothing.
func TestVolumeResizeRecheckedAtAnswer(t *testing.T) {
	f := volumeFixture(t)
	o, _ := f.b.File(Ask{Request: resizeReq("cache", 128<<30)})
	id := mustPending(t, o)
	f.src.set(func(c *config.Config) { c.VMVolumeLimit = 200 << 30 })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	_, offers, withheld, err := f.b.RequestOffers(id)
	if err != nil || len(offers) != 0 || len(withheld) != 3 || !strings.Contains(withheld[0].Why, "volume_limit") {
		t.Fatalf("offers %+v withheld %+v %v", offers, withheld, err)
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByCLI}); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "volume_limit") {
		t.Fatalf("answer: %v", err)
	}
	if got := f.resize.made(); len(got) != 0 {
		t.Fatalf("resized: %+v", got)
	}
}

// The person approves a resize from the size the request showed: once
// config.toml gives the volume another size, or another vm profile is in
// use, the offer is withheld and an answer refused, starting nothing --
// even where config.toml's size now refuses nothing (a resize to 128G of
// a volume grown to 200G meanwhile would be a shrink).
func TestVolumeResizeVolumeChanged(t *testing.T) {
	for name, change := range map[string]func(c *config.Config){
		"resized": func(c *config.Config) { c.Volumes[0].Size = 200 << 30 },
		"profile": func(c *config.Config) { c.Profile = "vm.other" },
	} {
		f := volumeFixture(t)
		o, _ := f.b.File(Ask{Request: resizeReq("cache", 128<<30)})
		id := mustPending(t, o)
		f.src.set(change)
		if err := f.b.Reload(); err != nil {
			t.Fatal(err)
		}
		_, offers, withheld, err := f.b.RequestOffers(id)
		if err != nil || len(offers) != 0 || len(withheld) != 3 || withheld[0].Why != VolumeChanged {
			t.Fatalf("%s: offers %+v withheld %+v %v", name, offers, withheld, err)
		}
		if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByCLI}); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), VolumeChanged) {
			t.Fatalf("%s: answer: %v", name, err)
		}
		if got := f.resize.made(); len(got) != 0 {
			t.Fatalf("%s: resized: %+v", name, got)
		}
	}
}

// With no Resizer (a link of its own, no daemon), a resize cannot be
// allowed; one that does not start says why on the answer and the
// request.
func TestVolumeResizeNotStarted(t *testing.T) {
	f := volumeFixture(t)
	f.noResizer = true
	f.reopen()
	o, _ := f.b.File(Ask{Request: resizeReq("cache", 128<<30)})
	id := mustPending(t, o)
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByCLI}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no resizer: %v", err)
	}

	f.noResizer = false
	f.resize.err = errors.New("a restart job already runs")
	f.reopen()
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if res.ResizeError != "a restart job already runs" {
		t.Fatalf("%+v", res)
	}
	ri, _ := f.b.Request(id)
	if !strings.Contains(ri.Reason, "then: the resize did not start: a restart job already runs") {
		t.Fatalf("%+v", ri)
	}
}

// sandboxErr is a Resizer's error with a text of its own for the sandbox.
type sandboxErr struct{ full, sandbox string }

func (e sandboxErr) Error() string       { return e.full }
func (e sandboxErr) SandboxText() string { return e.sandbox }

// A resize that does not start with an error that has a sandbox's text:
// the request's outcome, which the sandbox reads, has that text; the
// answer, the person's, has the whole error.
func TestVolumeResizeNotStartedSandboxText(t *testing.T) {
	f := volumeFixture(t)
	f.resize.err = sandboxErr{full: "open /Users/x/.caboose/envs/default/jobs.log: is a directory", sandbox: "open (a path on the host): is a directory"}
	f.reopen()
	o, _ := f.b.File(Ask{Request: resizeReq("cache", 128<<30)})
	id := mustPending(t, o)
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if res.ResizeError != "open /Users/x/.caboose/envs/default/jobs.log: is a directory" {
		t.Fatalf("%+v", res)
	}
	ri, _ := f.b.Request(id)
	if !strings.Contains(ri.Reason, "then: the resize did not start: open (a path on the host): is a directory") || strings.Contains(ri.Reason, "/Users/x") {
		t.Fatalf("%+v", ri)
	}
}
