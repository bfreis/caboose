package conformance

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// The wipe names the run's two roots and its managed canaries, each by a
// literal path, and nothing else: never "/", the home, /work itself, a
// glob, a variable or --no-preserve-root.
func TestWipeCommandNamesOnlyTheRunsPaths(t *testing.T) {
	managed := []SPath{{"/etc/claude-code/conf-0a1b2c3d-managed"}, {"/etc/claude-code/conf-0a1b2c3d-managed-rm"}}
	c, err := wipeCmd("0a1b2c3d", managed)
	if err != nil {
		t.Fatal(err)
	}
	want := "rm -rf /work/conf-0a1b2c3d-ro /work/conf-0a1b2c3d-dev /etc/claude-code/conf-0a1b2c3d-managed /etc/claude-code/conf-0a1b2c3d-managed-rm"
	if c.Body() != want || !c.Destructive() || !c.wipe {
		t.Fatalf("%q (destructive %v, wipe %v)", c.Body(), c.Destructive(), c.wipe)
	}
	if strings.ContainsAny(c.Body(), "*?[]`~$;|&<>(){}\\\"'") {
		t.Errorf("%q has a glob, expansion or metacharacter", c.Body())
	}
	toks := strings.Fields(c.Body())
	if toks[0] != "rm" || toks[1] != "-rf" {
		t.Fatalf("%q", c.Body())
	}
	for _, tok := range toks[2:] {
		switch {
		case tok == "/", tok == "/work", tok == "/etc/claude-code", tok == "--no-preserve-root", strings.HasPrefix(tok, "-"):
			t.Errorf("the wipe names %q", tok)
		case strings.HasPrefix(tok, "/home"), strings.HasPrefix(tok, "/root"), strings.Contains(tok, ".."):
			t.Errorf("the wipe names %q", tok)
		case !strings.Contains(tok, "/conf-0a1b2c3d-"):
			t.Errorf("%q is not the run's", tok)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", c.Argv()[2]).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
	for name, m := range map[string][]SPath{
		"another run's":      {{"/etc/claude-code/conf-ffffffff-managed"}},
		"below a canary":     {{"/etc/claude-code/conf-0a1b2c3d-managed/x"}},
		"the managed dir":    {{"/etc/claude-code"}},
		"a root":             {{"/work/conf-0a1b2c3d-ro"}},
		"a canary in a root": {{"/work/conf-0a1b2c3d-ro/conf-0a1b2c3d-ro-file"}},
		"the root dir":       {{"/"}},
		"twice":              {managed[0], managed[0]},
		"a glob":             {{"/etc/claude-code/conf-0a1b2c3d-*"}},
	} {
		if c, err := wipeCmd("0a1b2c3d", m); err == nil {
			t.Errorf("%s: %q", name, c.Body())
		}
	}
	for _, n8 := range []string{"", "0a1b2c3", "0A1B2C3D", "0a1b2c3d/", "../../x", "$(x)0000"} {
		if c, err := wipeCmd(n8, nil); err == nil {
			t.Errorf("nonce %q: %q", n8, c.Body())
		}
	}
}

// The wipe is reachable only through Env.Wipe, in a check both
// Destructive and Wipe: never through RunCmd, nested, or chained.
func TestWipeOnlyThroughWipe(t *testing.T) {
	g := &gatedRecorder{}
	ctx := context.Background()
	w, err := wipeCmd("0011aabb", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Env{g.sealedEnv(Destructive, true), g.sealedEnv(Destructive, false), g.sealedEnv(Write, false)} {
		for _, u := range []string{"agent", "root"} {
			if _, err := e.RunCmd(ctx, u, w); err == nil {
				t.Errorf("RunCmd ran the wipe in %+v", e.seal)
			}
		}
	}
	if _, err := ShellOf(w); err == nil {
		t.Error("the wipe nested")
	}
	for _, e := range []Env{g.sealedEnv(Destructive, false), g.sealedEnv(Write, true), g.sealedEnv(Observe, false)} {
		if _, err := e.Wipe(ctx); err == nil {
			t.Errorf("Wipe ran in %+v", e.seal)
		}
		if _, err := e.WipeCommand(); err == nil {
			t.Errorf("WipeCommand built in %+v", e.seal)
		}
	}
	if len(g.plain)+len(g.gated) != 0 {
		t.Errorf("ran %q / %q", g.plain, g.gated)
	}
	if _, err := NewEnv(g, runHost{nonce: "0011aabb"}, nil, config.KindContainer, config.TrustFallible).Wipe(ctx); err == nil {
		t.Error("Wipe ran outside the runner")
	}
	d := g.sealedEnv(Destructive, true)
	if _, err := d.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if len(g.gated) != 1 || len(g.plain) != 0 || !strings.Contains(g.gated[0], "rm -rf /work/conf-0011aabb-ro /work/conf-0011aabb-dev") {
		t.Errorf("the wipe went %q / %q", g.plain, g.gated)
	}
}

// ShellOf runs another command through sh -c, to exercise the nested form.
func ShellOf(c Cmd) (Cmd, error) {
	if !c.valid() || c.wipe || strings.Contains(c.body, "'") {
		return Cmd{}, errors.New("conformance: nothing to nest")
	}
	return Cmd{body: "sh -c '" + c.body + "'", destructive: c.destructive}, nil
}
