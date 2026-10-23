package broker

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

func TestEditEgressDiffWithoutWriting(t *testing.T) {
	f := newFixture(t)
	res, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"git.corp.example"}, By: ByWeb})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written || res.Diff == "" || res.DiffHash != DiffHash(res.Diff) {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Diff, "vm.default.egress_allow") || !strings.Contains(res.Diff, "git.corp.example") {
		t.Errorf("diff %q", res.Diff)
	}
	if !strings.Contains(res.Effect, "allows connections to git.corp.example from environment "+testEnv) {
		t.Errorf("effect %q", res.Effect)
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Errorf("%d writes without a confirmation", writes)
	}
}

func TestEditEgressWritesWithHash(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) {
		c.EgressAllow = []string{"a.example"}
		c.EgressDeny = []string{"*.ads.example"}
	})
	e := EgressEdit{AddAllow: []string{"10.20.0.0/16"}, RemoveAllow: []string{"a.example"}, RemoveDeny: []string{"*.ads.example"}, AddDeny: []string{"bad.example"}, By: ByCLI}
	shown, err := f.b.EditEgress(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Confirm = shown.DiffHash
	res, err := f.b.EditEgress(e)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Written || res.Diff != shown.Diff {
		t.Fatalf("result %+v, shown %+v", res, shown)
	}
	got, _ := f.src.get()
	if !slices.Equal(got.Config.EgressAllow, []string{"10.20.0.0/16"}) || !slices.Equal(got.Config.EgressDeny, []string{"bad.example"}) {
		t.Errorf("config now allow %q, deny %q", got.Config.EgressAllow, got.Config.EgressDeny)
	}
	for _, want := range []string{"changes nothing for public hosts", "addresses in 10.20.0.0/16", "denies connections to bad.example", "stops denying connections to every name under ads.example"} {
		if !strings.Contains(res.Effect, want) {
			t.Errorf("effect %q lacks %q", res.Effect, want)
		}
	}
	if !strings.Contains(string(f.read(decisionsFile)), `"event":"egress-edit"`) {
		t.Error("decisions.log has no egress-edit line")
	}
}

func TestEditEgressStaleAfterConcurrentEdit(t *testing.T) {
	f := newFixture(t)
	mine, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"one.example"}, By: ByWeb})
	if err != nil {
		t.Fatal(err)
	}
	// Someone else's edit lands while the first is being looked at.
	other, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"two.example"}, By: ByCLI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"two.example"}, By: ByCLI, Confirm: other.DiffHash}); err != nil {
		t.Fatal(err)
	}
	_, err = f.b.EditEgress(EgressEdit{AddAllow: []string{"one.example"}, By: ByWeb, Confirm: mine.DiffHash})
	if !errors.Is(err, ErrStaleDiff) {
		t.Fatalf("got %v, want stale_diff", err)
	}
	if _, writes := f.w.calls(); writes != 1 {
		t.Errorf("%d writes, want 1", writes)
	}
	// The file changing under the writer is stale too.
	again, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"one.example"}, By: ByWeb})
	if err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.drift = "+x\n"
	f.w.mu.Unlock()
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"one.example"}, By: ByWeb, Confirm: again.DiffHash}); !errors.Is(err, ErrStaleDiff) {
		t.Fatalf("got %v, want stale_diff", err)
	}
}

func TestEditEgressRefusedOutsideVM(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.Isolation, c.Profile = config.KindContainer, "container.default" })
	_, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"git.corp.example"}, By: ByWeb})
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "only under vm") {
		t.Fatalf("got %v", err)
	}
}

func TestEditEgressRefusesBadEdits(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.EgressAllow = []string{"a.example"} })
	many := make([]string, MaxEgressEditEntries+1)
	for i := range many {
		many[i] = strings.Repeat("a", i+1) + ".example"
	}
	for name, e := range map[string]EgressEdit{
		"syntax":                   {AddAllow: []string{"not a host"}},
		"comma":                    {AddAllow: []string{"a.example,b.example"}},
		"empty entry":              {AddDeny: []string{""}},
		"none":                     {AddAllow: []string{"none"}},
		"bad cidr":                 {AddDeny: []string{"10.0.0.0/99"}},
		"control":                  {AddAllow: []string{"a\x1b.example"}},
		"absent removal":           {RemoveAllow: []string{"b.example"}},
		"absent deny":              {RemoveDeny: []string{"a.example"}},
		"absent beside a real add": {AddAllow: []string{"c.example"}, RemoveAllow: []string{"b.example"}},
		"nothing":                  {},
		"too many":                 {AddAllow: many},
		"twice":                    {AddAllow: []string{"b.example", "b.example"}},
		"add and remove":           {AddAllow: []string{"a.example"}, RemoveAllow: []string{"a.example"}},
		"no change":                {AddAllow: []string{"a.example"}},
	} {
		e.By = ByWeb
		if _, err := f.b.EditEgress(e); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want bad_request", name, err)
		}
	}
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"b.example"}, By: "sandbox"}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("by the sandbox: %v", err)
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Errorf("%d writes", writes)
	}
}

func TestEditEgressAllowsTheNextRequestAtOnce(t *testing.T) {
	f := newFixture(t)
	r := egressReq("git.corp.example", "10.1.2.3")
	if o := f.decide(r); o.Verdict != Pending {
		t.Fatalf("before: %+v", o)
	}
	shown, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"git.corp.example"}, By: ByWeb})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"git.corp.example"}, By: ByWeb, Confirm: shown.DiffHash}); err != nil {
		t.Fatal(err)
	}
	if o := f.decide(r); o.Verdict != Allowed {
		t.Fatalf("after: %+v", o)
	}
	// A deny entry wins at once too.
	shown, err = f.b.EditEgress(EgressEdit{AddDeny: []string{"git.corp.example"}, By: ByWeb})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.EditEgress(EgressEdit{AddDeny: []string{"git.corp.example"}, By: ByWeb, Confirm: shown.DiffHash}); err != nil {
		t.Fatal(err)
	}
	if o := f.decide(r); o.Verdict != Denied {
		t.Fatalf("after the deny: %+v", o)
	}
}

func TestEditEgressHostileEffect(t *testing.T) {
	f := newFixture(t)
	f.required = []string{"api.anthropic.com"}
	f.reopen()
	f.src.set(func(c *config.Config) { c.Trust = config.TrustHostile; c.EgressAllow = []string{"api.example"} })
	res, err := f.b.EditEgress(EgressEdit{RemoveAllow: []string{"api.example"}, AddAllow: []string{"*.corp.example"}, By: ByApp})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"connections to api.example from environment " + testEnv + " ask again", "every name under corp.example from environment " + testEnv + " without asking"} {
		if !strings.Contains(res.Effect, want) {
			t.Errorf("effect %q lacks %q", res.Effect, want)
		}
	}
	if strings.Contains(res.Effect, "public hosts") {
		t.Errorf("hostile effect speaks of fallible: %q", res.Effect)
	}
}

func TestEditEgressNotOwner(t *testing.T) {
	f := newFixture(t)
	f.owner.lost.Store(true)
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"a.example"}, By: ByWeb}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v", err)
	}
}

// hostileFixture is a broker under trust = hostile whose egress_allow
// reaches the required hosts by name and by pattern.
func hostileFixture(t *testing.T, required []string) *fixture {
	t.Helper()
	f := newFixture(t)
	f.required = required
	f.reopen()
	f.src.set(func(c *config.Config) {
		c.Trust = config.TrustHostile
		c.EgressAllow = []string{"api.anthropic.com", "*.claude.ai", "claude.ai", "git.corp.example"}
	})
	return f
}

func TestEditEgressHostileKeepsRequiredHosts(t *testing.T) {
	required := []string{"api.anthropic.com", "claude.ai", "downloads.claude.ai"}
	for _, tc := range []struct {
		name string
		edit EgressEdit
		cut  string
	}{
		{"remove a name", EgressEdit{RemoveAllow: []string{"api.anthropic.com"}}, "api.anthropic.com"},
		{"remove a pattern", EgressEdit{RemoveAllow: []string{"*.claude.ai"}}, "downloads.claude.ai"},
		{"deny a name", EgressEdit{AddDeny: []string{"claude.ai"}}, "claude.ai"},
		{"deny a pattern", EgressEdit{AddDeny: []string{"*.claude.ai"}}, "downloads.claude.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hostileFixture(t, required)
			tc.edit.By = ByWeb
			_, err := f.b.EditEgress(tc.edit)
			var be *Error
			if !errors.As(err, &be) || be.Kind != KindBadRequest || !strings.Contains(be.Msg, tc.cut) {
				t.Fatalf("got %v, want a refusal naming %s", err, tc.cut)
			}
			if _, writes := f.w.calls(); writes != 0 {
				t.Errorf("%d writes", writes)
			}
		})
	}
	// What cuts off no required host goes through.
	f := hostileFixture(t, required)
	if _, err := f.b.EditEgress(EgressEdit{RemoveAllow: []string{"git.corp.example"}, AddDeny: []string{"ads.example"}, By: ByWeb}); err != nil {
		t.Fatalf("an edit that keeps every required host: %v", err)
	}
}

func TestEditEgressHostileWithoutRequiredHostsFailsClosed(t *testing.T) {
	f := hostileFixture(t, nil)
	for _, e := range []EgressEdit{{RemoveAllow: []string{"git.corp.example"}}, {AddDeny: []string{"ads.example"}}} {
		e.By = ByCLI
		if _, err := f.b.EditEgress(e); err == nil {
			t.Errorf("%+v went through with no required hosts known", e)
		}
	}
	// Adding an allow entry cuts nothing off.
	if _, err := f.b.EditEgress(EgressEdit{AddAllow: []string{"new.example"}, By: ByCLI}); err != nil {
		t.Errorf("adding an allow entry: %v", err)
	}
}

func TestEditEgressFallibleIgnoresRequiredHosts(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.EgressAllow = []string{"api.anthropic.com"} })
	if _, err := f.b.EditEgress(EgressEdit{RemoveAllow: []string{"api.anthropic.com"}, By: ByWeb}); err != nil {
		t.Fatalf("fallible: %v", err)
	}
}

// A trust other than exactly fallible is decided as hostile.
func TestEditEgressUnknownTrustKeepsRequiredHosts(t *testing.T) {
	f := hostileFixture(t, []string{"api.anthropic.com"})
	f.src.set(func(c *config.Config) { c.Trust = "Fallible" })
	if _, err := f.b.EditEgress(EgressEdit{RemoveAllow: []string{"api.anthropic.com"}, By: ByWeb}); err == nil {
		t.Fatal("an unknown trust let a required host be cut off")
	}
}
