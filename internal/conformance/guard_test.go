package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

func TestSetupMakesTheEnvelope(t *testing.T) {
	r := newRig(t, config.KindContainer)
	g := r.g
	if filepath.Dir(g.Base()) != r.tmp || !strings.HasPrefix(filepath.Base(g.Base()), BasePrefix) {
		t.Fatalf("base %s is not %s/%s*", g.Base(), r.tmp, BasePrefix)
	}
	if !regexp.MustCompile(`^cf-[0-9a-f]{8}$`).MatchString(g.Env()) || !config.ValidEnv(g.Env()) {
		t.Fatalf("env %q", g.Env())
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(g.Nonce()) {
		t.Fatalf("nonce %q", g.Nonce())
	}
	fi, err := os.Lstat(r.path(MarkerFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker: %v %v", fi, err)
	}
	var m Marker
	b, _ := os.ReadFile(r.path(MarkerFile))
	if err := json.Unmarshal(b, &m); err != nil || m.Nonce != g.Nonce() || m.Env != g.Env() || m.Base != g.Base() || m.SuiteVersion != SuiteVersion {
		t.Fatalf("marker %+v: %v", m, err)
	}
	for _, n := range g.RootNames() {
		if !strings.HasPrefix(n, "conf-"+g.Nonce8()+"-") {
			t.Fatalf("root name %q", n)
		}
		b, err := os.ReadFile(filepath.Join(g.RootPath(n), RootMarker))
		if err != nil || strings.TrimSpace(string(b)) != g.Nonce() {
			t.Fatalf("root %s marker: %q %v", n, b, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(g.RootPath(g.RootNames()[1]), WritablePart)); err != nil || !fi.IsDir() {
		t.Fatalf("writable part: %v", err)
	}
	for _, d := range []string{"home", g.DataRel(), "roots", "canary"} {
		if fi, err := os.Lstat(r.path(d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if g.DataDir() != r.path("home/envs/"+g.Env()+"/data") {
		t.Fatalf("the data dir is %s, not the environment's default", g.DataDir())
	}
	if _, err := os.Lstat(r.path("data")); err == nil {
		t.Fatal("B/data was made")
	}
	if err := g.Verify(context.Background()); err != nil {
		t.Fatalf("the good case refused: %v", err)
	}
}

func TestSetupRefusesMissingFacts(t *testing.T) {
	for name, mod := range map[string]func(*Facts){
		"no CABOOSE_HOME": func(f *Facts) { f.CabooseHome = "" },
		"no home":         func(f *Facts) { f.UserHome = "" },
		"no roots":        func(f *Facts) { f.RealRoots = nil },
		"windows":         func(f *Facts) { f.GOOS = "windows" },
	} {
		t.Run(name, func(t *testing.T) {
			f, tmp, _ := testFacts(t)
			mod(&f)
			if _, err := Setup(f, config.KindContainer, config.TrustFallible); err == nil {
				t.Fatal("Setup did not refuse")
			}
			if es, _ := os.ReadDir(tmp); len(es) != 0 {
				t.Fatalf("Setup made %v before refusing", es)
			}
		})
	}
	f, _, _ := testFacts(t)
	if _, err := Setup(f, "docker", config.TrustFallible); err == nil {
		t.Fatal("an unknown isolation was taken")
	}
	if _, err := Setup(f, config.KindContainer, "trusted"); err == nil {
		t.Fatal("an unknown trust was taken")
	}
}

func rewriteMarker(r *rig, mod func(*Marker)) {
	r.t.Helper()
	b, err := os.ReadFile(r.path(MarkerFile))
	if err != nil {
		r.t.Fatal(err)
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		r.t.Fatal(err)
	}
	mod(&m)
	b, _ = json.Marshal(m)
	if err := os.WriteFile(r.path(MarkerFile), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	cases := map[string]struct {
		item string
		mod  func(r *rig)
	}{
		"marker missing": {"marker", func(r *rig) { must(r.t, os.Remove(r.path(MarkerFile))) }},
		"wrong nonce":    {"marker", func(r *rig) { rewriteMarker(r, func(m *Marker) { m.Nonce = strings.Repeat("0", 32) }) }},
		"wrong env in marker": {"marker", func(r *rig) {
			rewriteMarker(r, func(m *Marker) { m.Env = "cf-00000000" })
		}},
		"wrong base in marker": {"marker", func(r *rig) { rewriteMarker(r, func(m *Marker) { m.Base = r.tmp }) }},
		"wrong isolation":      {"marker", func(r *rig) { rewriteMarker(r, func(m *Marker) { m.Isolation = config.KindVM }) }},
		"wrong trust":          {"marker", func(r *rig) { rewriteMarker(r, func(m *Marker) { m.Trust = config.TrustHostile }) }},
		"marker is a symlink": {"marker", func(r *rig) {
			r.write("canary/m", mustRead(r.t, r.path(MarkerFile)))
			must(r.t, os.Remove(r.path(MarkerFile)))
			must(r.t, os.Symlink(r.path("canary/m"), r.path(MarkerFile)))
		}},
		"marker mode 0644": {"marker", func(r *rig) { must(r.t, os.Chmod(r.path(MarkerFile), 0o644)) }},
		"marker mode 0400": {"marker", func(r *rig) { must(r.t, os.Chmod(r.path(MarkerFile), 0o400)) }},
		"marker not ours": {"marker", func(r *rig) {
			old := currentUID
			currentUID = func() int { return old() + 1 }
			r.t.Cleanup(func() { currentUID = old })
		}},
		"real environment name": {"env", func(r *rig) {
			r.g.env = "default"
			rewriteMarker(r, func(m *Marker) { m.Env = "default" })
		}},
		"in-memory name differs": {"marker", func(r *rig) { r.g.env = "cf-deadbeef" }},
		"B not under temp":       {"base", func(r *rig) { r.g.facts.TempRoot = r.real }},
		"B is the home":          {"base", func(r *rig) { r.g.facts.UserHome = r.g.Base() }},
		"B inside the home":      {"base", func(r *rig) { r.g.facts.UserHome = r.tmp }},
		"B contains the home":    {"base", func(r *rig) { r.g.facts.UserHome = r.path("home") }},
		"B is CABOOSE_HOME":      {"base", func(r *rig) { r.g.facts.CabooseHome = r.g.Base() }},
		"B inside CABOOSE_HOME":  {"base", func(r *rig) { r.g.facts.CabooseHome = r.tmp }},
		"B holds CABOOSE_HOME":   {"base", func(r *rig) { r.g.facts.CabooseHome = r.path("canary") }},
		"B inside a real root": {"base", func(r *rig) {
			r.g.facts.RealRoots = func() ([]string, error) { return []string{r.tmp}, nil }
		}},
		"a real root inside B": {"base", func(r *rig) {
			r.g.facts.RealRoots = func() ([]string, error) { return []string{r.path("roots")}, nil }
		}},
		"roots cannot be listed": {"base", func(r *rig) {
			r.g.facts.RealRoots = func() ([]string, error) { return nil, errors.New("no loader") }
		}},
		"no root loader": {"base", func(r *rig) { r.g.facts.RealRoots = nil }},
		"B is a symlink": {"base", func(r *rig) {
			moved := r.g.Base() + ".moved"
			must(r.t, os.Rename(r.g.Base(), moved))
			must(r.t, os.Symlink(moved, r.g.Base()))
		}},
		"B renamed": {"base", func(r *rig) {
			to := filepath.Join(r.tmp, "elsewhere")
			must(r.t, os.Rename(r.g.Base(), to))
			r.g.base = to
		}},
		"root without its marker": {"nonce files", func(r *rig) {
			must(r.t, os.Remove(filepath.Join(r.g.RootPath(r.g.RootNames()[0]), RootMarker)))
		}},
		"root marker wrong": {"nonce files", func(r *rig) {
			r.write("roots/"+r.g.RootNames()[1]+"/"+RootMarker, strings.Repeat("1", 32))
		}},
		"data marker wrong": {"nonce files", func(r *rig) { r.write(r.g.DataMarker(), "x") }},
		"data marker gone":  {"nonce files", func(r *rig) { must(r.t, os.Remove(r.path(r.g.DataMarker()))) }},
		"CABOOSE_DATA_DIR in the launch env": {"launch env", func(r *rig) {
			in := r.g.inputs()
			in.LaunchEnv = append(append([]string(nil), in.LaunchEnv...), "CABOOSE_DATA_DIR="+r.g.DataDir())
			r.g.Attach(in)
		}},
		"no launch env": {"launch env", func(r *rig) {
			in := r.g.inputs()
			in.LaunchEnv = nil
			r.g.Attach(in)
		}},
		"caller's CABOOSE_ENV kept": {"launch env", func(r *rig) {
			in := r.g.inputs()
			in.LaunchEnv = append(in.LaunchEnv, "CABOOSE_ENV=default")
			r.g.Attach(in)
		}},
		"DOCKER_HOST kept": {"launch env", func(r *rig) {
			in := r.g.inputs()
			in.LaunchEnv = append(in.LaunchEnv, "DOCKER_HOST=tcp://h")
			r.g.Attach(in)
		}},
		"launch env lacks the home": {"launch env", func(r *rig) {
			in := r.g.inputs()
			in.LaunchEnv = []string{"CABOOSE_ENV=" + r.g.Env()}
			r.g.Attach(in)
		}},
		"in a sandbox": {"sandbox", func(r *rig) {
			r.g.facts.Detectors = []Detector{{"fake", func() (bool, error) { return true, nil }}}
		}},
		"no inspector": {"mount audit", func(r *rig) {
			in := r.g.inputs()
			in.Inspect = nil
			r.g.Attach(in)
		}},
		"extra mount": {"mount audit", func(r *rig) {
			r.insp.Mounts = append(r.insp.Mounts, Mount{Source: r.real, Target: "/work/dev"})
		}},
		"forbidden mount": {"mount audit", func(r *rig) {
			r.insp.Mounts = append(r.insp.Mounts, Mount{Source: r.path("home"), Target: "/x"})
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, config.KindContainer)
			c.mod(r)
			err := r.g.Verify(context.Background())
			var re *RefusalError
			if !errors.As(err, &re) {
				t.Fatalf("Verify did not refuse: %v", err)
			}
			if re.Item != c.item {
				t.Fatalf("refused for %q (%v), want %q", re.Item, err, c.item)
			}
		})
	}
}

func TestVerifyRefusesAGuardItDidNotMake(t *testing.T) {
	ctx := context.Background()
	var nilGuard *Guard
	if err := nilGuard.Verify(ctx); err == nil {
		t.Fatal("a nil guard verified")
	}
	if err := (&Guard{}).Verify(ctx); err == nil {
		t.Fatal("a zero guard verified")
	}
	// Another run's marker in this run's base: the nonce differs.
	a, b := newRig(t, config.KindContainer), newRig(t, config.KindContainer)
	b.write(MarkerFile, mustRead(t, a.path(MarkerFile)))
	if err := b.g.Verify(ctx); err == nil {
		t.Fatal("another run's marker was taken")
	}
	// A guard pointed at another run's base.
	a.g.base = b.g.Base()
	if err := a.g.Verify(ctx); err == nil {
		t.Fatal("another run's base was taken")
	}
}

// A guard Setup did not make refuses on its own account, even when every
// other field is copied from one that Setup made and that verifies.
func TestHandBuiltGuardRefuses(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, config.KindContainer)
	if _, err := r.g.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	hand := &Guard{
		base: r.g.base, env: r.g.env, nonce: r.g.nonce, isolation: r.g.isolation,
		trust: r.g.trust, started: r.g.started, facts: r.g.facts, in: r.g.in,
		canaries: r.g.canaries, pinned: r.g.pinned, pinnedAudit: r.g.pinnedAudit,
	}
	part := "roots/" + r.g.RootNames()[1] + "/" + WritablePart
	for name, call := range map[string]func(*Guard) error{
		"VerifyBase":        func(g *Guard) error { return g.VerifyBase() },
		"Verify":            func(g *Guard) error { return g.Verify(ctx) },
		"VerifyDestructive": func(g *Guard) error { return g.VerifyDestructive(ctx) },
		"Snapshot":          func(g *Guard) error { _, err := g.Snapshot(); return err },
		"MakeCanary":        func(g *Guard) error { _, err := g.MakeCanary(part, "hand", File); return err },
		"RemoveCanary":      func(g *Guard) error { return g.RemoveCanary(Canary{Rel: part}) },
		"Teardown":          func(g *Guard) error { return g.Teardown(ctx, &fakeRemover{}) },
	} {
		for _, g := range []*Guard{{}, hand} {
			var re *RefusalError
			if err := call(g); !errors.As(err, &re) || re.Item != "guard" {
				t.Fatalf("%s on a guard Setup did not make: %v", name, err)
			}
		}
	}
	if err := r.g.VerifyDestructive(ctx); err != nil {
		t.Fatalf("the guard it was copied from: %v", err)
	}
	if _, err := os.Lstat(r.path(MarkerFile)); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchEnvScrubs(t *testing.T) {
	r := newRig(t, config.KindContainer)
	env := r.g.LaunchEnv([]string{"PATH=/bin", "CABOOSE_ENV=default", "CABOOSE_HOME=/real", "CABOOSE_SESSION=s", "DOCKER_CONTEXT=c", "DOCKER_HOST=h"})
	want := map[string]bool{
		"PATH=/bin": true, "CABOOSE_HOME=" + r.path("home"): true,
		"CABOOSE_ENV=" + r.g.Env(): true, "CABOOSE_NO_AUTO_UPDATE=1": true,
	}
	if len(env) != len(want) {
		t.Fatalf("env %v", env)
	}
	for _, kv := range env {
		if !want[kv] {
			t.Fatalf("env has %q", kv)
		}
	}
	if err := r.g.CheckLaunchEnv(env); err != nil {
		t.Fatal(err)
	}
}

func TestExactNames(t *testing.T) {
	r := newRig(t, config.KindContainer)
	c, ims := r.g.ExactNames()
	if c != "caboose-"+r.g.Env() || len(ims) != 2 || ims[0] != "caboose:"+r.g.Env() || ims[1] != "caboose-base:"+r.g.Env() {
		t.Fatalf("%s %v", c, ims)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
