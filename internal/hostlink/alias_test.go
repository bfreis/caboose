package hostlink

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// policyDecider decides as a broker would, by the policy itself: its
// aliases and writable set are the test's, its identities computed from
// it as the broker computes them; what asks is pending, numbered.
type policyDecider struct {
	mu      sync.Mutex
	p       policy.Policy
	next    uint64
	decided []policy.Request
}

func newPolicyDecider(t *testing.T, trust policy.Trust, aliases map[string]policy.Alias, writable ...string) *policyDecider {
	t.Helper()
	p := policy.Policy{Trust: trust, Aliases: aliases, Writable: writable}
	for _, w := range writable {
		if fi, err := os.Stat(w); err == nil {
			if id, ok := policy.IDOf(fi); ok {
				p.WritableIDs = append(p.WritableIDs, id)
			}
		}
	}
	return &policyDecider{p: p}
}

func (d *policyDecider) Decide(r policy.Request) (broker.Outcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.decided = append(d.decided, r)
	dec := policy.Decide(d.p, r, time.Now())
	switch dec.Verdict {
	case policy.VerdictAllow:
		return broker.Outcome{Verdict: broker.Allowed, Reason: dec.Reason}, nil
	case policy.VerdictAsk:
		d.next++
		return broker.Outcome{Verdict: broker.Pending, Reason: dec.Reason, Request: d.next}, nil
	}
	return broker.Outcome{Verdict: broker.Denied, Reason: dec.Reason}, nil
}

func (d *policyDecider) File(a broker.Ask) (broker.Outcome, error) { return d.Decide(a.Request) }

func (d *policyDecider) List(broker.Query) ([]broker.RequestInfo, uint64, error) { return nil, 0, nil }

func (d *policyDecider) Wait(context.Context, uint64) (uint64, error) { return 0, nil }

func (d *policyDecider) Aliases() ([]broker.AliasInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []broker.AliasInfo
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range d.p.Aliases {
			if !yield(k) {
				return
			}
		}
	}) {
		a := d.p.Aliases[n]
		out = append(out, broker.AliasInfo{Name: a.Name, Description: a.Description, Params: a.Params, Asks: a.Policy == policy.AliasAsk})
	}
	return out, nil
}

func (d *policyDecider) Alias(name string) (policy.Alias, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.p.Aliases[name]
	return a, ok, nil
}

func (d *policyDecider) last() policy.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decided[len(d.decided)-1]
}

// script writes an executable shell script under dir, a test's own.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// physicalTemp is a fresh temp dir, by its physical path.
func physicalTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// aliasHost runs a Host deciding aliases with d, its aliases' fresh
// directories under a temp dir of the test's.
func aliasHost(t *testing.T, d *policyDecider) (*execAgent, string) {
	t.Helper()
	tmp := filepath.Join(physicalTemp(t), "alias-tmp")
	a := startExecHost(t, Config{Decide: d, AliasTmp: tmp})
	return a, tmp
}

// refusal is how the host refused an alias run.
type refusal2 struct {
	reason, msg string
	request     uint64
}

// openAlias asks for an alias run of argv, and returns the host's stream
// for it, or its refusal.
func (a *execAgent) openAlias(argv []string) (*agentproto.Stream, *refusal2) {
	a.t.Helper()
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.mu.Unlock()
	c := a.chans(id)
	if err := a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: id, Op: agentproto.OpAliasRun,
		Exec: &agentproto.ExecRequest{Argv: argv, Dir: "/work"}}); err != nil {
		a.t.Fatal(err)
	}
	var r agentproto.Message
	select {
	case r = <-c.answer:
	case <-time.After(5 * time.Second):
		a.t.Fatal("no answer to an alias run")
	}
	if !r.OK {
		return nil, &refusal2{reason: r.Reason, msg: r.Error, request: r.RequestID}
	}
	select {
	case st := <-c.stream:
		return st, nil
	case <-time.After(5 * time.Second):
		a.t.Fatal("the host said yes, but opened no stream")
	}
	return nil, nil
}

// runAlias runs argv's alias to its end, with stdin.
func (a *execAgent) runAlias(argv []string, stdin string) (execResult, *refusal2) {
	a.t.Helper()
	st, ref := a.openAlias(argv)
	if ref != nil {
		return execResult{}, ref
	}
	var out, errs bytes.Buffer
	code, err := agentproto.Exec(st, agentproto.ExecRequest{Argv: argv, Stdin: stdin != ""},
		agentproto.ExecIO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errs})
	if err != nil {
		a.t.Fatalf("alias %v: %v", argv, err)
	}
	return execResult{code, out.String(), errs.String()}, nil
}

// entries are the names in dir, which must exist.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// What runs is the alias's argv, its parameters put in, each element one
// argument, through no shell; in a fresh empty directory under AliasTmp,
// removed afterwards; with PATH, HOME and LANG and nothing else.
func TestAliasRuns(t *testing.T) {
	bin := physicalTemp(t)
	home := physicalTemp(t)
	t.Setenv("HOME", home)
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "agent.sock"))
	t.Setenv("GITHUB_TOKEN", "not-a-token")
	cmd := script(t, bin, "show", `for a in "$@"; do printf 'arg:%s\n' "$a"; done
printf 'pwd:%s\n' "$(pwd -P)"
printf 'ls:%s\n' "$(ls -A)"
cat
exit 4
`)
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"show": {Name: "show", Command: []string{cmd, "{suite}", "$(touch pwned)", "a b;c", "{suite}x"},
			Params: map[string][]string{"suite": {"unit", "all"}}},
	})
	a, tmp := aliasHost(t, d)
	if !a.hello.AliasesOn {
		t.Fatal("the hello does not offer aliases")
	}
	r, ref := a.runAlias([]string{"show", "suite=all"}, "in")
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	var args, env []string // env stays empty: the script prints none
	var pwd, ls string
	for _, l := range strings.Split(strings.TrimSuffix(r.stdout, "in"), "\n") {
		switch k, v, _ := strings.Cut(l, ":"); k {
		case "arg":
			args = append(args, v)
		case "pwd":
			pwd = v
		case "ls":
			ls = v
		}
	}
	if want := []string{"all", "$(touch pwned)", "a b;c", "{suite}x"}; !slices.Equal(args, want) {
		t.Errorf("arguments %q, want %q", args, want)
	}
	_ = env
	if filepath.Dir(pwd) != tmp || ls != "" {
		t.Errorf("ran in %q holding %q, want a fresh empty directory under %s", pwd, ls, tmp)
	}
	if r.code != 4 || !strings.HasSuffix(r.stdout, "in") {
		t.Errorf("exit %d, stdout %q: want the command's 4 and its stdin passed through", r.code, r.stdout)
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Errorf("the fresh directory is left: %v", left)
	}
	if _, err := os.Stat(filepath.Join(pwd, "pwned")); err == nil {
		t.Error("an argument ran as a shell command")
	}
	// The resolved path was what was decided, and what ran.
	if got := d.last(); got.Command != cmd || len(got.CommandIDs) == 0 {
		t.Errorf("decided %+v", got)
	}
	// The environment: exactly PATH, HOME and LANG, whatever the link's
	// own has.
	envPath, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env")
	}
	d.p.Aliases["env"] = policy.Alias{Name: "env", Command: []string{envPath}}
	r, ref = a.runAlias([]string{"env"}, "")
	if ref != nil || r.code != 0 {
		t.Fatalf("env: %+v %+v", r, ref)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		k, v, _ := strings.Cut(l, "=")
		names = append(names, k)
		if k == "HOME" && v != home {
			t.Errorf("HOME=%s, want %s", v, home)
		}
	}
	sort.Strings(names)
	if strings.Join(names, " ") != "HOME LANG PATH" {
		t.Errorf("the alias's environment is %v, want HOME LANG PATH only", names)
	}
	// With a cwd of its own, it runs there, and makes no directory.
	work := physicalTemp(t)
	d.p.Aliases["here"] = policy.Alias{Name: "here", Command: []string{cmd}, Cwd: work}
	r, ref = a.runAlias([]string{"here"}, "")
	if ref != nil || !strings.Contains(r.stdout, "pwd:"+work+"\n") {
		t.Errorf("an alias with a cwd: %+v %+v", r, ref)
	}
}

// Only the parameters an alias declares, with the values it lists, are
// taken; anything else is refused before anything runs.
func TestAliasArgsRefused(t *testing.T) {
	bin := physicalTemp(t)
	marker := filepath.Join(bin, "ran")
	cmd := script(t, bin, "mark", "touch '"+marker+"'\n")
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"mark": {Name: "mark", Command: []string{cmd, "{suite}"}, Params: map[string][]string{"suite": {"unit", "all"}}},
	})
	a, _ := aliasHost(t, d)
	for _, argv := range [][]string{
		{"mark", "suite=nope"},
		{"mark", "other=unit"},
		{"mark", "suite=unit;rm"},
		{"mark", "suite=$(id)"},
		{"mark", "suite=unit", "suite=all"},
		{"mark", "suite"},
		{"mark", "--suite=unit"},
		{"Mark"},
		{"mark", "suite=" + strings.Repeat("a", 65)},
		{},
	} {
		_, ref := a.runAlias(argv, "")
		if ref == nil || ref.reason != agentproto.AliasDenied {
			t.Errorf("%q: %+v, want refused as denied", argv, ref)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused alias ran")
	}
	if _, ref := a.runAlias([]string{"mark", "suite=all"}, ""); ref != nil {
		t.Fatalf("a good one: %+v", ref)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the good one did not run")
	}
}

// A command within what a sandbox can write is refused: by its path, by a
// symlink it is reached through, and by its identity when the writable
// set spells the directory another way.
func TestAliasCommandWritable(t *testing.T) {
	writable := physicalTemp(t)
	safe := physicalTemp(t)
	marker := filepath.Join(safe, "ran")
	inside := script(t, writable, "tool", "touch '"+marker+"'\n")
	outside := script(t, safe, "tool", "touch '"+marker+"'\n")
	// A symlink outside, to the command inside.
	viaOut := filepath.Join(safe, "via-out")
	if err := os.Symlink(inside, viaOut); err != nil {
		t.Fatal(err)
	}
	// A symlink inside, to the command outside: the sandbox could
	// repoint it.
	viaIn := filepath.Join(writable, "via-in")
	if err := os.Symlink(outside, viaIn); err != nil {
		t.Fatal(err)
	}
	// The writable directory by another name: a symlink to it, standing
	// for a firmlink or another normalization, which the paths do not
	// show.
	other := filepath.Join(physicalTemp(t), "other-name")
	if err := os.Symlink(writable, other); err != nil {
		t.Fatal(err)
	}
	// Outside, through a symlink inside, to outside again: the hop is
	// the sandbox's to repoint.
	mid := filepath.Join(writable, "mid")
	if err := os.Symlink(outside, mid); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(safe, "hop")
	if err := os.Symlink(mid, hop); err != nil {
		t.Fatal(err)
	}
	// A script whose interpreter is inside.
	interp := script(t, writable, "interp", "exit 0\n")
	byInterp := filepath.Join(safe, "by-interp")
	if err := os.WriteFile(byInterp, []byte("#!"+interp+"\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, cmd string
		writable  string
	}{
		{"inside", inside, writable},
		{"outside, through a hop inside, to outside", hop, writable},
		{"a script whose interpreter is inside", byInterp, writable},
		{"through a symlink to inside", viaOut, writable},
		{"through a symlink inside", viaIn, writable},
		{"by identity alone", inside, other},
	} {
		d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"tool": {Name: "tool", Command: []string{c.cmd}}}, c.writable)
		a, _ := aliasHost(t, d)
		_, ref := a.runAlias([]string{"tool"}, "")
		if ref == nil || ref.reason != agentproto.AliasDenied || !strings.Contains(ref.msg, "writ") {
			t.Errorf("%s: %+v, want denied as writable", c.name, ref)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command within a writable path ran")
	}
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"tool": {Name: "tool", Command: []string{outside}}}, writable)
	a, _ := aliasHost(t, d)
	if _, ref := a.runAlias([]string{"tool"}, ""); ref != nil {
		t.Fatalf("a command outside: %+v", ref)
	}
}

// Under trust = hostile, a cwd or an absolute argument within a writable
// path is refused too, by path or by identity.
func TestAliasHostileWritable(t *testing.T) {
	writable := physicalTemp(t)
	bin := physicalTemp(t)
	cmd := script(t, bin, "t", "exit 0\n")
	other := filepath.Join(physicalTemp(t), "other-name")
	if err := os.Symlink(writable, other); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		alias policy.Alias
	}{
		{"cwd", policy.Alias{Name: "t", Command: []string{cmd}, Cwd: writable}},
		{"argument", policy.Alias{Name: "t", Command: []string{cmd, filepath.Join(writable, "x")}}},
	} {
		for _, w := range []string{writable, other} {
			d := newPolicyDecider(t, policy.TrustHostile, map[string]policy.Alias{"t": c.alias}, w)
			a, _ := aliasHost(t, d)
			if _, ref := a.runAlias([]string{"t"}, ""); ref == nil || ref.reason != agentproto.AliasDenied || !strings.Contains(ref.msg, "hostile") {
				t.Errorf("%s within %s: %+v", c.name, w, ref)
			}
			// Fallible lets it run.
			d = newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"t": c.alias}, w)
			a, _ = aliasHost(t, d)
			if _, ref := a.runAlias([]string{"t"}, ""); ref != nil {
				t.Errorf("%s within %s, fallible: %+v", c.name, w, ref)
			}
		}
	}
}

func TestAliasRefusals(t *testing.T) {
	bin := physicalTemp(t)
	cmd := script(t, bin, "t", "exit 0\n")
	notExec := filepath.Join(bin, "plain")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"asks":    {Name: "asks", Command: []string{cmd}, Policy: policy.AliasAsk},
		"refused": {Name: "refused", Command: []string{cmd}, Policy: policy.AliasDeny},
		"missing": {Name: "missing", Command: []string{filepath.Join(bin, "gone")}},
		"plain":   {Name: "plain", Command: []string{notExec}},
		"nocwd":   {Name: "nocwd", Command: []string{cmd}, Cwd: filepath.Join(bin, "gone")},
	})
	a, _ := aliasHost(t, d)
	for _, c := range []struct {
		argv   string
		reason string
		text   string
	}{
		{"asks", agentproto.AliasPending, "pending #1"},
		{"refused", agentproto.AliasDenied, `policy = "deny"`},
		{"nope", agentproto.AliasUnknown, "caboose-agent aliases"},
		{"missing", agentproto.AliasUnavailable, "missing on the host"},
		{"plain", agentproto.AliasUnavailable, "no executable file"},
		{"nocwd", agentproto.AliasUnavailable, "cwd"},
	} {
		_, ref := a.runAlias([]string{c.argv}, "")
		if ref == nil || ref.reason != c.reason || !strings.Contains(ref.msg, c.text) {
			t.Errorf("%s: %+v, want %s and %q", c.argv, ref, c.reason, c.text)
		}
		if c.reason == agentproto.AliasPending && ref != nil && ref.request != 1 {
			t.Errorf("pending without its request number: %+v", ref)
		}
	}
	// No broker, or nowhere for fresh directories: no aliases.
	for _, cfg := range []Config{{AliasTmp: t.TempDir()}, {Decide: d}} {
		a := startExecHost(t, cfg)
		if a.hello.AliasesOn {
			t.Errorf("%+v offers aliases", cfg)
		}
		if _, ref := a.runAlias([]string{"asks"}, ""); ref == nil || ref.reason != agentproto.AliasUnavailable {
			t.Errorf("a link that runs no aliases: %+v", ref)
		}
	}
}

// The exec protocol's request must be the alias run decided, and asks
// nothing an alias does not take.
func TestAliasExecMustMatch(t *testing.T) {
	bin := physicalTemp(t)
	marker := filepath.Join(bin, "ran")
	cmd := script(t, bin, "t", "touch '"+marker+"'\n")
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"t": {Name: "t", Command: []string{cmd, "{v}"}, Params: map[string][]string{"v": {"a", "b"}}},
	})
	a, _ := aliasHost(t, d)
	for _, req := range []agentproto.ExecRequest{
		{Argv: []string{"t", "v=b"}},
		{Argv: []string{"t", "v=a"}, TTY: true},
		{Argv: []string{"t", "v=a"}, User: "0"},
		{Argv: []string{"t", "v=a"}, Env: []string{"A=b"}},
	} {
		st, ref := a.openAlias([]string{"t", "v=a"})
		if ref != nil {
			t.Fatal(ref)
		}
		var errs bytes.Buffer
		code, err := agentproto.Exec(st, req, agentproto.ExecIO{Stdout: io.Discard, Stderr: &errs})
		if err != nil || code != agentproto.ExitCannotRun {
			t.Errorf("%+v: %d %v %q", req, code, err, errs.String())
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ran what was not decided")
	}
}

// startAliasSleepers runs an alias that leaves two sleeps in its group,
// and returns their PIDs and its own, its stream, and its result.
func startAliasSleepers(t *testing.T, a *execAgent, name string) ([]int, *agentproto.Stream, <-chan int) {
	t.Helper()
	st, ref := a.openAlias([]string{name})
	if ref != nil {
		t.Fatal(ref)
	}
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code, _ := agentproto.Exec(st, agentproto.ExecRequest{Argv: []string{name}}, agentproto.ExecIO{Stdout: pw, Stderr: io.Discard})
		pw.Close()
		done <- code
	}()
	var pids []int
	sc := bufio.NewScanner(pr)
	for len(pids) < 3 && sc.Scan() {
		pid, err := strconv.Atoi(sc.Text())
		if err != nil {
			t.Fatalf("not a PID: %q", sc.Text())
		}
		pids = append(pids, pid)
	}
	if len(pids) < 3 {
		t.Fatal("the alias did not say its PIDs")
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	return pids, st, done
}

const sleepers = "sleep 30 & echo $!; sleep 30 & echo $!; echo $$; wait\n"

// A client that goes away takes its alias with it, and what that
// started; its fresh directory goes after them.
func TestAliasClientGoneKillsGroup(t *testing.T) {
	bin := physicalTemp(t)
	cmd := script(t, bin, "s", sleepers)
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"s": {Name: "s", Command: []string{cmd}}})
	a, tmp := aliasHost(t, d)
	pids, st, done := startAliasSleepers(t, a, "s")
	st.Close()
	gone(t, pids)
	<-done
	waitEmpty(t, tmp)
}

// An alias's timeout ends it, and what it started.
func TestAliasTimeout(t *testing.T) {
	bin := physicalTemp(t)
	cmd := script(t, bin, "s", sleepers)
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"s": {Name: "s", Command: []string{cmd}, Timeout: 300 * time.Millisecond}})
	a, tmp := aliasHost(t, d)
	pids, _, done := startAliasSleepers(t, a, "s")
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit %d, want %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the timeout did not end it")
	}
	gone(t, pids)
	waitEmpty(t, tmp)
}

// What an alias leaves running in its group when it exits goes with it.
func TestAliasRemainsKilled(t *testing.T) {
	bin := physicalTemp(t)
	cmd := script(t, bin, "s", "sleep 30 >/dev/null 2>&1 & echo $!\n")
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"s": {Name: "s", Command: []string{cmd}}})
	a, tmp := aliasHost(t, d)
	r, ref := a.runAlias([]string{"s"}, "")
	if ref != nil || r.code != 0 {
		t.Fatalf("%+v %+v", r, ref)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(r.stdout))
	if err != nil {
		t.Fatalf("no PID: %q", r.stdout)
	}
	gone(t, []int{pid})
	waitEmpty(t, tmp)
}

// The fresh directory is removed inside AliasTmp only: a command that
// puts a symlink to elsewhere in its place has the symlink removed, and
// what it points at kept.
func TestAliasDirRemovedInsideItsRoot(t *testing.T) {
	keep := physicalTemp(t)
	canary := filepath.Join(keep, "canary")
	if err := os.WriteFile(canary, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := physicalTemp(t)
	cmd := script(t, bin, "swap", `d=$(pwd -P); cd .. && rmdir "$d" && ln -s '`+keep+`' "$d"
`)
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{"swap": {Name: "swap", Command: []string{cmd}}})
	a, tmp := aliasHost(t, d)
	if !strings.HasPrefix(tmp, os.TempDir()) && !strings.HasPrefix(tmp, physicalOrSame(os.TempDir())) {
		t.Fatalf("the alias directories' root %s is not under the temp dir", tmp)
	}
	if r, ref := a.runAlias([]string{"swap"}, ""); ref != nil || r.code != 0 {
		t.Fatalf("%+v %+v", r, ref)
	}
	waitEmpty(t, tmp)
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("what the symlink pointed at was removed: %v", err)
	}
}

func physicalOrSame(p string) string {
	if q, err := filepath.EvalSymlinks(p); err == nil {
		return q
	}
	return p
}

// waitEmpty waits for dir to hold nothing.
func waitEmpty(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(entries(t, dir)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s still holds %v", dir, entries(t, dir))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The listing names the aliases, their descriptions and parameters, and
// never their commands or directories.
func TestAliasesListed(t *testing.T) {
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"test": {Name: "test", Description: "run the tests\x1b[2J", Command: []string{"/secret/cmd", "{suite}"}, Cwd: "/secret/dir",
			Params: map[string][]string{"suite": {"unit", "all"}}, Policy: policy.AliasAsk},
		"awake": {Name: "awake", Command: []string{"/usr/bin/caffeinate"}},
	})
	a, _ := aliasHost(t, d)
	r := a.request(agentproto.Message{Op: agentproto.OpAliases})
	if !r.OK || len(r.Aliases) != 2 {
		t.Fatalf("%+v", r)
	}
	b, _ := agentproto.Encode(r)
	if strings.Contains(string(b), "secret") {
		t.Errorf("the listing shows a command or directory: %s", b)
	}
	if got := r.Aliases[1]; got.Name != "test" || !got.Asks || !slices.Equal(got.Params["suite"], []string{"unit", "all"}) || strings.ContainsRune(got.Description, 0x1b) {
		t.Errorf("%+v", got)
	}
}

// Through a real agent link: caboose-agent run's own client, and its
// exit statuses.
func TestAliasThroughAgent(t *testing.T) {
	bin := physicalTemp(t)
	cmd := script(t, bin, "t", `printf '%s\n' "$@"; cat; exit 6`+"\n")
	d := newPolicyDecider(t, policy.TrustFallible, map[string]policy.Alias{
		"t":    {Name: "t", Command: []string{cmd, "{v}"}, Params: map[string][]string{"v": {"a", "b"}}},
		"asks": {Name: "asks", Command: []string{cmd}, Policy: policy.AliasAsk},
		"no":   {Name: "no", Command: []string{cmd}, Policy: policy.AliasDeny},
	})
	socket := linked(t, Config{Decide: d, AliasTmp: filepath.Join(physicalTemp(t), "alias-tmp")})
	var out bytes.Buffer
	code, err := agent.AliasRun(context.Background(), socket, []string{"t", "v=b"}, "/work", agentproto.ExecIO{Stdin: strings.NewReader("in\n"), Stdout: &out, Stderr: io.Discard})
	if err != nil || code != 6 || out.String() != "b\nin\n" {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
	for _, c := range []struct {
		argv []string
		code int
		text string
	}{
		{[]string{"asks"}, agent.ExitPending, "pending #"},
		{[]string{"no"}, agent.ExitDenied, "deny"},
		{[]string{"nope"}, agent.ExitUnknown, "no alias"},
		{[]string{"t", "v=c"}, agent.ExitDenied, "not a value"},
	} {
		code, err := agent.AliasRun(context.Background(), socket, c.argv, "/work", agentproto.ExecIO{Stdout: io.Discard, Stderr: io.Discard})
		if code != c.code || err == nil || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%q: %d %v, want %d and %q", c.argv, code, err, c.code, c.text)
		}
	}
	list, err := agent.Aliases(socket)
	if err != nil || len(list) != 3 {
		t.Fatalf("%v %v", list, err)
	}
}
