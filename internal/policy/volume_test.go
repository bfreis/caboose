package policy

import (
	"strings"
	"testing"
	"time"
)

const gib = int64(1 << 30)

// volumePolicy is a vm profile with cache (64G) and data (100G), and
// volume_limit 256G.
func volumePolicy() Policy {
	return Policy{Trust: TrustHostile, VMProfile: "vm.default", VolumeLimit: 256 * gib,
		Volumes: map[string]int64{"cache": 64 * gib, "data": 100 * gib}}
}

func liveResize(name string, size int64) Request {
	return Request{Kind: KindVolumeResize, Volume: name, VolumeSize: size, VolumeFrom: 64 * gib, VolumeLive: true, VolumeFree: 300 * gib}
}

func TestVolumeResizeDecisions(t *testing.T) {
	now := time.Now()
	notLive := liveResize("cache", 128*gib)
	notLive.VolumeLive = false
	noVM := volumePolicy()
	noVM.VMProfile, noVM.Volumes = "", nil
	overLimit := volumePolicy()
	overLimit.Volumes = map[string]int64{"cache": 200 * gib, "data": 100 * gib}
	for name, tc := range map[string]struct {
		p       Policy
		r       Request
		verdict Verdict
		rule    string
		says    []string
	}{
		"grows, asks even under hostile": {volumePolicy(), liveResize("cache", 128*gib), VerdictAsk, RuleAskVolumeResize, nil},
		"shrinks, asks":                  {volumePolicy(), liveResize("cache", 32*gib), VerdictAsk, RuleAskVolumeResize, nil},
		"up to the limit exactly":        {volumePolicy(), liveResize("cache", 156*gib), VerdictAsk, RuleAskVolumeResize, nil},
		"one over the limit": {volumePolicy(), liveResize("cache", 157*gib), VerdictDeny, RuleVolumeLimit,
			[]string{"would total 257G, 1G over volume_limit (256G) in [vm.default] of config.toml on the host", "ask for at most 156G", "the user raises volume_limit"}},
		"no room left": {func() Policy { p := volumePolicy(); p.Volumes["data"] = 250 * gib; return p }(), liveResize("cache", 128*gib), VerdictDeny, RuleVolumeLimit,
			[]string{"the other volumes leave no room to grow it"}},
		// A shrink brings the total down: never the limit's to refuse,
		// even where config.toml is past it already.
		"a shrink past the limit already": {overLimit, Request{Kind: KindVolumeResize, Volume: "cache", VolumeSize: 180 * gib, VolumeFrom: 200 * gib, VolumeLive: true}, VerdictAsk, RuleAskVolumeResize, nil},
		"a grow past the limit already":   {overLimit, Request{Kind: KindVolumeResize, Volume: "cache", VolumeSize: 210 * gib, VolumeFrom: 200 * gib, VolumeLive: true}, VerdictDeny, RuleVolumeLimit, nil},
		// The disk (64G) not yet at config.toml's size (200G, past the
		// limit already): a resize to config.toml's own size leaves its
		// total, and is not the limit's to refuse.
		"config.toml's own size past the limit": {overLimit, liveResize("cache", 200*gib), VerdictAsk, RuleAskVolumeResize, nil},
		// volume_limit unset (0) is the default, 256G.
		"no volume_limit, within the default": {func() Policy { p := volumePolicy(); p.VolumeLimit = 0; return p }(), liveResize("cache", 156*gib), VerdictAsk, RuleAskVolumeResize, nil},
		"no volume_limit, past the default": {func() Policy { p := volumePolicy(); p.VolumeLimit = 0; return p }(), liveResize("cache", 157*gib), VerdictDeny, RuleVolumeLimit,
			[]string{"over volume_limit (256G)"}},
		"no such volume": {volumePolicy(), liveResize("nope", 128*gib), VerdictDeny, "deny:volume-unknown",
			[]string{`profile vm.default has no volume "nope" (its volumes: cache, data)`, "[volume] in a proposal"}},
		"no vm":          {noVM, liveResize("cache", 128*gib), VerdictDeny, "deny:volume-no-vm", nil},
		"too small":      {volumePolicy(), liveResize("cache", 2*gib), VerdictDeny, "deny:volume-size", []string{"outside 4G to 1024G"}},
		"too big":        {volumePolicy(), liveResize("cache", 1025*gib), VerdictDeny, "deny:volume-size", nil},
		"not live":       {volumePolicy(), notLive, VerdictDeny, "deny:volume-not-live", []string{"caboose volume resize cache 128G"}},
		"the same size":  {volumePolicy(), liveResize("cache", 64*gib), VerdictDeny, "deny:volume-same", []string{"64G already"}},
		"a name refused": {volumePolicy(), liveResize("\x1b[31m", 128*gib), VerdictDeny, "deny:volume-unknown", nil},
	} {
		d := Decide(tc.p, tc.r, now)
		if d.Verdict != tc.verdict || d.Rule != tc.rule {
			t.Errorf("%s: %v %s: %s", name, d.Verdict, d.Rule, d.Reason)
		}
		for _, s := range tc.says {
			if !strings.Contains(d.Reason, s) {
				t.Errorf("%s: %q does not say %q", name, d.Reason, s)
			}
		}
		if strings.ContainsRune(d.Reason, 0x1b) {
			t.Errorf("%s: unprintable reason %q", name, d.Reason)
		}
	}
}

// A grant of the volume's key never allows a resize: a person decides
// each.
func TestVolumeResizeIgnoresGrants(t *testing.T) {
	p := volumePolicy()
	r := liveResize("cache", 128*gib)
	p.Grants = []Grant{{ID: "g1", Kind: KindVolumeResize, Key: GrantKey(r), Expires: time.Now().Add(time.Hour)},
		{ID: "g2", Kind: KindVolumeResize, Key: GrantKey(r), Once: true, Exact: ExactKey(r), Expires: time.Now().Add(time.Hour)}}
	if d := Decide(p, r, time.Now()); d.Verdict != VerdictAsk {
		t.Fatalf("%+v", d)
	}
}

func TestVolumeResizeKeysAndEffects(t *testing.T) {
	r := liveResize("cache", 128*gib)
	if GrantKey(r) != "volume_resize:cache" || ExactKey(r) != "137438953472" {
		t.Errorf("keys %q %q", GrantKey(r), ExactKey(r))
	}
	if ExactKey(r) == ExactKey(liveResize("cache", 96*gib)) {
		t.Error("two sizes share an exact key: one answer would cover the other")
	}
	e := Effect(volumePolicy(), r)
	for _, s := range []string{"resize volume cache of vm.default from 64G to 128G", "grows its disk", "free on this Mac where its disk is: 300G"} {
		if !strings.Contains(e, s) {
			t.Errorf("grow effect %q lacks %q", e, s)
		}
	}
	shrink := liveResize("cache", 32*gib)
	shrink.VolumeFree = -1
	e = Effect(volumePolicy(), shrink)
	for _, s := range []string{"from 64G to 32G", "unmounted", "cloned first", "where its disk is: unknown"} {
		if !strings.Contains(e, s) {
			t.Errorf("shrink effect %q lacks %q", e, s)
		}
	}
	if s := AnswerEffect(r, ScopeOnce, 0, "default", "default", "/c"); !strings.Contains(s, "resizes volume cache to 128G now") {
		t.Errorf("once: %q", s)
	}
	for _, s := range []Scope{ScopeHours, ScopeAlways} {
		if e := AnswerEffect(r, s, 2, "default", "default", "/c"); e != "" {
			t.Errorf("scope %d offered: %q", s, e)
		}
	}
}
