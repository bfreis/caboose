package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// confOps is a suite environment laid out as Prepare lays it out, with
// its App on a fake sandbox and a fake daemon: nothing reaches docker, a
// VM or a real daemon.
type confOps struct {
	g      *conformance.Guard
	h      *confHost
	box    *backendtest.Fake
	daemon *fakeDaemon
	// started are the daemon starts the suite made: exe, args and env.
	started [][]string
}

func newConfOps(t *testing.T, iso, trust string) *confOps {
	t.Helper()
	g := confTestGuard(t, iso, trust)
	h, store, _, err := confLayOut(g, "", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	environ := g.LaunchEnv([]string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "SHELL=/bin/zsh", "USER=u"})
	engine := ""
	if iso != config.KindVM {
		engine = "unix:///engine.sock"
	}
	a, err := confApp(g, environ, engine, "", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o := &confOps{g: g, h: h, box: &backendtest.Fake{}, daemon: &fakeDaemon{}}
	a.Backend = o.box
	a.DaemonDial = o.daemon.dial
	h.a = a
	h.daemon = newConfDaemon(g, environ, engine)
	h.daemon.start = func(exe string, args, env []string) (int, error) {
		o.started = append(o.started, append(append([]string{exe}, args...), env...))
		return 4242, nil
	}
	a.Spawn = h.daemon.spawn
	if err := confOpenStore(h, store); err != nil {
		t.Fatal(err)
	}
	return o
}

// The daemon is started with the stubs first on its PATH, no SHELL, the
// throwaway CABOOSE_HOME, the pinned engine and the environment canary,
// and nothing of the caller's CABOOSE_ or docker settings.
func TestConfDaemonEnv(t *testing.T) {
	o := newConfOps(t, config.KindContainer, config.TrustFallible)
	env := o.h.daemon.env
	get := envLookup(env)
	switch {
	case !strings.HasPrefix(get("PATH"), o.g.StubPath()+":"):
		t.Errorf("PATH %q", get("PATH"))
	case get("SHELL") != "":
		t.Errorf("SHELL %q", get("SHELL"))
	case get("CABOOSE_HOME") != o.g.CabooseHome():
		t.Errorf("CABOOSE_HOME %q", get("CABOOSE_HOME"))
	case get("DOCKER_HOST") != "unix:///engine.sock":
		t.Errorf("DOCKER_HOST %q", get("DOCKER_HOST"))
	case get(o.h.EnvCanary()) == "":
		t.Errorf("no %s", o.h.EnvCanary())
	case get("CABOOSE_NO_AUTO_UPDATE") != "1":
		t.Error("the daemon would update itself")
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CABOOSE_DATA_DIR" || k == "CABOOSE_ENV" || k == "DOCKER_CONTEXT" {
			t.Errorf("the daemon has %s", kv)
		}
	}
	if !strings.HasPrefix(o.h.EnvCanary(), "CONFORMANCE_CANARY_") {
		t.Errorf("the canary is %q", o.h.EnvCanary())
	}
	vm := newConfOps(t, config.KindVM, config.TrustFallible)
	if envLookup(vm.h.daemon.env)("DOCKER_HOST") != "" {
		t.Error("a VM's daemon is given a docker engine")
	}
}

// The suite starts the daemon alone, once, and the stubs are in place
// only once the daemon that answers is the one it started, in its
// directory under the throwaway CABOOSE_HOME.
func TestConfStubsInPlace(t *testing.T) {
	o := newConfOps(t, config.KindContainer, config.TrustFallible)
	if err := o.h.StubsInPlace(); err == nil {
		t.Fatal("the stubs are in place before the daemon started")
	}
	if err := o.h.a.Spawn("/x/caboose", "update", "--background"); err == nil {
		t.Fatal("the suite started an update")
	}
	if err := o.h.a.Spawn("/x/caboose", "daemon"); err != nil {
		t.Fatal(err)
	}
	if err := o.h.a.Spawn("/x/caboose", "daemon"); err == nil {
		t.Fatal("the daemon was started twice")
	}
	if len(o.started) != 1 || !slices.Contains(o.started[0], "PATH="+o.g.StubPath()+":/usr/bin:/bin") {
		t.Fatalf("started %q", o.started)
	}
	dir := o.h.a.daemonDir()
	if !confInside(dir, o.g.CabooseHome()) {
		t.Fatalf("the daemon's directory %s is not under B/home", dir)
	}
	o.daemon.Stat = &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 4242, Dir: dir}}
	if err := o.h.StubsInPlace(); err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]daemonproto.DaemonInfo{
		"another pid":       {PID: 1, Dir: dir},
		"another directory": {PID: 4242, Dir: filepath.Join(t.TempDir(), "daemon-dev")},
	} {
		o.daemon.Stat = &daemonproto.StatusResult{Daemon: st}
		if err := o.h.StubsInPlace(); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	o.daemon.Stat = &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 4242, Dir: dir}}
	if err := os.WriteFile(filepath.Join(o.g.StubPath(), "open"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := o.h.StubsInPlace(); err == nil {
		t.Error("a changed stub is in place")
	}
}

// The configuration calls work on copies, with the launcher's own loader
// and the link's own reading of the settings.
func TestConfConfigCalls(t *testing.T) {
	o := newConfOps(t, config.KindContainer, config.TrustFallible)
	h := o.h
	before, _ := os.ReadFile(filepath.Join(o.g.Base(), confConfigRel(o.g.Env())))
	if err := h.TryConfig(conformance.ConfigVariant{Trust: config.TrustHostile, Isolation: config.KindVM}); err != nil {
		t.Fatalf("a hostile VM does not load: %v", err)
	}
	off := false
	if err := h.TryConfig(conformance.ConfigVariant{Trust: config.TrustHostile, Isolation: config.KindVM, Egress: &off}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("hostile with egress off: %v", err)
	}
	if err := h.TryConfig(conformance.ConfigVariant{Trust: config.TrustFallible, Isolation: config.KindContainer, Egress: &off}); err == nil {
		t.Fatal("egress on a container profile was tried")
	}
	on, _, err := h.HostExecOffered(conformance.ConfigVariant{Trust: config.TrustFallible, Isolation: config.KindVM, HostExec: true})
	if err != nil || !on {
		t.Fatalf("fallible with host_exec: %v, %v", on, err)
	}
	on, why, err := h.HostExecOffered(conformance.ConfigVariant{Trust: config.TrustHostile, Isolation: config.KindVM, HostExec: true})
	if err != nil || on || !strings.Contains(why, "trust") {
		t.Fatalf("hostile with host_exec: %v, %q, %v", on, why, err)
	}
	const standIn = "/stand-in/agent.sock"
	sock, from, err := h.SSHAgentCarried(conformance.ConfigVariant{Trust: config.TrustFallible, Isolation: config.KindVM}, standIn)
	if err != nil || sock != standIn || from != agentFromEnv {
		t.Fatalf("fallible agent: %q (%s), %v", sock, from, err)
	}
	sock, from, err = h.SSHAgentCarried(conformance.ConfigVariant{Trust: config.TrustHostile, Isolation: config.KindVM}, standIn)
	if err != nil || sock != "" || from != agentFromHostile {
		t.Fatalf("hostile agent: %q (%s), %v", sock, from, err)
	}
	after, _ := os.ReadFile(filepath.Join(o.g.Base(), confConfigRel(o.g.Env())))
	if string(before) != string(after) {
		t.Fatal("a copy's change reached config.toml")
	}
	if cmd, err := h.AliasCommand(conformance.AliasMarker); err != nil || cmd != o.g.StubPath()+"/"+conformance.AliasMarker {
		t.Fatalf("AliasCommand = %q, %v", cmd, err)
	}
	if _, err := h.AliasCommand("other"); err == nil {
		t.Fatal("an alias not the suite's has a command")
	}
}

// Create and Attach are the launcher's decisions alone: nothing is
// created or started, and trust round-trips to the same config.toml.
func TestConfLaunchDecisions(t *testing.T) {
	o := newConfOps(t, config.KindContainer, config.TrustHostile)
	res, err := o.h.Create(context.Background())
	if err != nil || !res.Refused || !strings.Contains(res.Message, "trust") || !strings.Contains(res.Message, "vm") || res.Created {
		t.Fatalf("Create under hostile, container: %+v, %v", res, err)
	}
	if len(o.box.Calls) != 0 {
		t.Fatalf("the sandbox was touched: %v", o.box.Calls)
	}

	o = newConfOps(t, config.KindContainer, config.TrustFallible)
	if res, err := o.h.Create(context.Background()); err != nil || res.Refused || res.Created {
		t.Fatalf("Create under fallible: %+v, %v", res, err)
	}
	o.box.Status = "running"
	o.box.SandboxLabels = map[string]string{assets.LabelIsolation: config.KindContainer}
	hash, err := o.h.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	if res, err := o.h.Attach(context.Background()); err != nil || res.Refused {
		t.Fatalf("Attach under fallible: %+v, %v", res, err)
	}
	if err := o.h.SetTrust(config.TrustHostile); err != nil {
		t.Fatal(err)
	}
	res, err = o.h.Attach(context.Background())
	if err != nil || !res.Refused || !strings.Contains(res.Message, "caboose restart") {
		t.Fatalf("Attach under hostile: %+v, %v", res, err)
	}
	if err := o.h.SetTrust(config.TrustFallible); err != nil {
		t.Fatal(err)
	}
	if now, err := o.h.ConfigHash(); err != nil || now != hash {
		t.Fatalf("config.toml after the round trip: %v", err)
	}
	if len(o.box.Calls) != 0 {
		t.Fatalf("the sandbox was touched: %v", o.box.Calls)
	}
	if _, err := o.h.EffectiveTrust(); err == nil {
		t.Fatal("a trust was read with no link running")
	}
	if err := o.h.SetTrust("other"); err == nil {
		t.Fatal("an unknown trust was written")
	}
}

// The egress keys are [vm.conformance]'s, under every isolation, and the
// launch decision is the product's on a copy.
func TestConfEgress(t *testing.T) {
	for _, iso := range []string{config.KindVM, config.KindContainer} {
		t.Run(iso, func(t *testing.T) {
			o := newConfOps(t, iso, config.TrustFallible)
			p := conformance.EgressPolicy{Allow: []string{"192.168.1.0/24"}, Deny: []string{"10.0.0.1"}, Ports: []int{8080, 9090}}
			if err := o.h.SetEgress(p); err != nil {
				t.Fatal(err)
			}
			got, err := o.h.EgressKeys()
			if err != nil || !got.Equal(p) {
				t.Fatalf("EgressKeys = %+v, %v", got, err)
			}
			p.Ports = nil
			if err := o.h.SetEgress(p); err != nil {
				t.Fatal(err)
			}
			if got, err := o.h.EgressKeys(); err != nil || !got.Equal(p) {
				t.Fatalf("EgressKeys = %+v, %v", got, err)
			}
			if _, err := o.h.app(); err != nil {
				t.Fatalf("the edited config.toml does not load: %v", err)
			}
		})
	}
	o := newConfOps(t, config.KindVM, config.TrustHostile)
	keys, err := o.h.EgressKeys()
	if err != nil || len(keys.Allow) == 0 {
		t.Fatalf("a hostile VM's egress_allow: %+v, %v", keys, err)
	}
	if res, err := o.h.LaunchDecision(context.Background(), keys); err != nil || res.Refused {
		t.Fatalf("the run's own keys: %+v, %v", res, err)
	}
	res, err := o.h.LaunchDecision(context.Background(), conformance.EgressPolicy{Allow: []string{"example.com"}})
	if err != nil || !res.Refused {
		t.Fatalf("no Claude hosts: %+v, %v", res, err)
	}
	if now, err := o.h.EgressKeys(); err != nil || !now.Equal(keys) {
		t.Fatal("a launch decision changed config.toml")
	}
}

func TestConfClaudeCount(t *testing.T) {
	top := "UID PID PPID C STIME TTY TIME CMD\n" +
		"1000 10 1 0 10:00 ? 00:00:01 claude --print\n" +
		"1000 11 1 0 10:00 ? 00:00:01 /home/agent/.local/share/claude/versions/2.0.1 x\n" +
		"1000 12 1 0 10:00 ? 00:00:01 node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js\n" +
		"1000 13 1 0 10:00 ? 00:00:01 tmux new -s claude\n" +
		"1000 14 1 0 10:00 ? 00:00:01 /sbin/tini -- /entrypoint\n"
	if n := countClaudeTop(top); n != 3 {
		t.Fatalf("counted %d", n)
	}
	if n := countClaudeTop("no table"); n != 0 {
		t.Fatalf("counted %d", n)
	}
	o := newConfOps(t, config.KindContainer, config.TrustHostile)
	if n, err := o.h.ClaudeProcesses(); err != nil || n != 0 {
		t.Fatalf("no sandbox: %d, %v", n, err)
	}
	o.box.Status = "running"
	if _, err := o.h.ClaudeProcesses(); err == nil {
		t.Fatal("an unpinned sandbox was counted")
	}
}

func TestConfCheckpointParsing(t *testing.T) {
	out := "/work/x/part: took k1@20260101T000000Z (walk, 3 entries)\n  it holds half or less of what k1@20251231T000000Z held: if files were lost, restore that one before it ages out\n"
	tk, err := parseTaken(out)
	if err != nil || tk.ID != "k1@20260101T000000Z" || tk.Shrunk != "k1@20251231T000000Z" {
		t.Fatalf("%+v %v", tk, err)
	}
	if _, err := parseTaken("/work/x/part: not taken: no\n"); err == nil {
		t.Fatal("a refusal was taken for a checkpoint")
	}
	res := daemonproto.CheckpointsResult{Env: "e", Paths: []daemonproto.CheckpointListed{{Path: "/work/x/part", Key: "k1", Shrunk: "s",
		Checkpoints: []daemonproto.CheckpointView{{ID: "k1@1", Reason: "manual", Entries: 3, Bytes: 9}}}}}
	b, _ := json.Marshal(res)
	ps, err := parseListing(b)
	if err != nil || len(ps) != 1 || ps[0].Shrunk != "s" || ps[0].Checkpoints[0] != (conformance.CheckpointInfo{ID: "k1@1", Reason: "manual", Entries: 3, Bytes: 9}) {
		t.Fatalf("%+v %v", ps, err)
	}
}

func TestConfRestoreOutcome(t *testing.T) {
	res := &checkpoint.Restored{Warnings: []string{"w"}}
	cases := []struct {
		rerr    error
		refused conformance.RestoreRefusal
		isErr   bool
	}{
		{nil, conformance.RestoreDone, false},
		{checkpoint.ErrTargetExists, conformance.RefusedExists, false},
		{errors.Join(errors.New("x"), checkpoint.ErrTargetRefused), conformance.RefusedUnsafe, false},
		{errors.New("disk full"), "", true},
	}
	for _, c := range cases {
		o, err := restoreOutcome(res, c.rerr, c.rerr, "")
		if (err != nil) != c.isErr || o.Refused != c.refused || (!c.isErr && !slices.Equal(o.Warnings, []string{"w"})) {
			t.Errorf("%v: %+v, %v", c.rerr, o, err)
		}
	}
	if _, err := restoreOutcome(nil, nil, errors.New("bad id"), "x"); err == nil {
		t.Error("a restore that never ran is an outcome")
	}
}

// The checkpoint calls run the product's own commands in this process,
// on the suite's writable part, and a restore writes only where the guard
// says.
func TestConfCheckpoints(t *testing.T) {
	o := newConfOps(t, config.KindContainer, config.TrustFallible)
	h := o.h
	store, err := h.StorePath()
	if err != nil || store != filepath.Join(o.g.Base(), o.g.StoreRel()) {
		t.Fatalf("StorePath = %s, %v", store, err)
	}
	part := config.WorkDir + "/" + o.g.RootNames()[1] + "/" + conformance.WritablePart
	tk, err := h.TakeCheckpoint(context.Background(), part)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := h.List(context.Background())
	if err != nil || len(paths) != 1 || paths[0].Path != part || len(paths[0].Checkpoints) != 1 || paths[0].Checkpoints[0].ID != tk.ID {
		t.Fatalf("List = %+v, %v", paths, err)
	}
	tree, err := h.Tree(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen, err := h.Canary(checks.RoleSeen)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree[filepath.Base(seen.Rel)]; !ok {
		t.Fatalf("the copy lacks %s: %v", seen.Rel, tree)
	}
	rst, err := h.Canary(checks.RoleRestore)
	if err != nil {
		t.Fatal(err)
	}
	into, err := conformance.TargetIn(rst, "copy")
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Restore(context.Background(), tk.ID, into)
	if err != nil || out.Refused != conformance.RestoreDone {
		t.Fatalf("Restore = %+v, %v", out, err)
	}
	if _, err := os.Lstat(filepath.Join(o.g.Base(), into.Rel(), filepath.Base(seen.Rel))); err != nil {
		t.Fatalf("the restored copy: %v", err)
	}
	at, err := conformance.TargetAt(rst)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := h.Restore(context.Background(), tk.ID, at); err != nil || out.Refused != conformance.RefusedExists {
		t.Fatalf("Restore onto the canary = %+v, %v", out, err)
	}
	// A target no canary of this run makes is never passed to the product.
	other := newConfOps(t, config.KindContainer, config.TrustFallible)
	foreign, err := other.h.Canary(checks.RoleRestore)
	if err != nil {
		t.Fatal(err)
	}
	ft, err := conformance.TargetIn(foreign, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Restore(context.Background(), tk.ID, ft); err == nil {
		t.Fatal("a restore into another run's canary was run")
	}
	if _, err := os.Lstat(filepath.Join(other.g.Base(), ft.Rel())); err == nil {
		t.Fatal("another run's canary was written")
	}
	if _, err := h.Restore(context.Background(), tk.ID, conformance.RestoreTarget{}); err == nil {
		t.Fatal("a zero target was passed")
	}
	dropped, err := h.Prune(context.Background(), time.Now(), []string{tk.ID})
	if err != nil || slices.Contains(dropped, tk.ID) {
		t.Fatalf("Prune = %v, %v", dropped, err)
	}
	if _, err := h.Prune(context.Background(), time.Now(), []string{"not an id"}); err == nil {
		t.Fatal("a bad pin was taken")
	}
}

// The probe goes into the writable part, from the directory make
// conformance-probe built it into, and only from there.
func TestPlaceConfProbe(t *testing.T) {
	g := confTestGuard(t, config.KindContainer, config.TrustFallible)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "conformance-probe-linux-arm64"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := placeConfProbe(g, dir, "riscv64"); err == nil {
		t.Fatal("a probe for an architecture with none was placed")
	}
	if err := placeConfProbe(g, dir, "amd64"); err == nil {
		t.Fatal("a missing probe was placed")
	}
	if err := placeConfProbe(g, dir, "arm64"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(g.RootPath(g.RootNames()[1]), conformance.WritablePart, conformance.ProbeName)
	if fi, err := os.Lstat(p); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the probe: %v", err)
	}
	if b, err := os.ReadFile(p); err != nil || g.ProbeHash() != fmt.Sprintf("%x", sha256.Sum256(b)) {
		t.Errorf("the probe's recorded hash %q: %v", g.ProbeHash(), err)
	}
	if err := placeConfProbe(g, dir, "arm64"); err == nil {
		t.Fatal("the probe was written over")
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "conformance-probe-linux-arm64"), filepath.Join(link, "conformance-probe-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	g2 := confTestGuard(t, config.KindContainer, config.TrustFallible)
	if err := placeConfProbe(g2, link, "amd64"); err == nil {
		t.Fatal("a symlinked probe was followed")
	}
	if err := placeConfProbe(g2, "", "amd64"); err != nil {
		t.Fatal("no probe dir is an error")
	}
}
