package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
)

// The host-command and alias checks, H1-H10: the sandbox asks the host to
// run things it may not, and each check decides from the host whether
// anything ran. Their commands are caboose-agent's, sent only through the
// envelope's RunCmd; whatever a broken control lets run is one of the
// suite's own scripts, which write only into the alias-out canary, or a
// touch in a canary directory of the read-only root.

const (
	groupHost = "host"
	stepHost  = "host commands and aliases"

	// RoleAliasOut is the directory canary, outside every mount, that the
	// suite's alias scripts write into.
	RoleAliasOut = "alias-out"
	// RoleHostOut is the directory canary, in the read-only root, that a
	// host command a check asks for would write into.
	RoleHostOut = "h1-out"

	// settle is how long a check gives the host to reread what the
	// sandbox wrote before it asks again.
	settle = 5 * time.Second
	// sleepKill and sleepWait are when conf-sleep's caller is ended, and
	// how long after that its marker must still be absent.
	sleepKill = 3
	sleepWait = 70 * time.Second
)

// Exit statuses of caboose-agent: run refusals, host's refusal, and the
// caller's own usage error (a name or argument it refuses before asking).
const (
	exitPending     = 75
	exitDenied      = 77
	exitUnknown     = 127
	exitUnavailable = 126
	exitUsage       = 2
	exitTimeout     = 124
	exitTerm        = 143
)

// HostCanary is a canary the host checks need, made before the run.
type HostCanary struct {
	Parent, Role string
	Kind         c.Kind
}

// HostLayout lists the canaries of HostChecks and TrustChecks: the
// alias-out directory in canary (the host directory no mount reaches) and
// the host-command directory in ro (the read-only root).
func HostLayout(canary, ro string) ([]HostCanary, error) {
	if canary == "" || ro == "" {
		return nil, errors.New("checks: the host checks need the canary directory and the read-only root")
	}
	return []HostCanary{{canary, RoleAliasOut, c.Dir}, {ro, RoleHostOut, c.Dir}}, nil
}

// HostChecks are H1-H10 in the order they run.
func HostChecks() []c.Check {
	return []c.Check{h1(), h2(), h3(), h4(), h5(), h6(), h7(), h8(), h9(), h10()}
}

func hostCheck(id, title, control string, run func(context.Context, *hostRun) c.CheckResult) c.Check {
	return c.Check{
		ID: id, Title: title, Control: control, Step: stepHost, Group: groupHost,
		Class: c.Write, Roles: []string{RoleAliasOut, RoleHostOut},
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			h, bad := prepare(env)
			if bad != nil {
				return *bad
			}
			return run(ctx, h)
		},
	}
}

func hostileOnly(ck c.Check) c.Check {
	ck.Trusts = []string{c.TrustHostile}
	ck.Isolations = []string{"vm"}
	return ck
}

func errorf(format string, args ...any) c.CheckResult {
	return c.CheckResult{Status: c.Error, Reason: fmt.Sprintf(format, args...)}
}

func failf(format string, args ...any) c.CheckResult {
	return c.CheckResult{Status: c.Fail, Reason: fmt.Sprintf(format, args...)}
}

// hostRun is one host check's run: its Env, the host's calls, and the
// canaries it reads.
type hostRun struct {
	env     c.Env
	ops     c.HostOps
	out     c.Canary // alias-out
	hostOut c.Canary // h1-out
	n8      string
}

var n8Of = regexp.MustCompile(`/conf-([0-9a-f]{8})-[a-z0-9-]+$`)

// prepare is what every host check needs before it sends anything: a
// running sandbox, the host's calls, the stubs in place (so nothing a
// broken control lets through can show the person a notification or a
// dialog), and both canaries on the host.
func prepare(env c.Env) (*hostRun, *c.CheckResult) {
	bad := func(r c.CheckResult) (*hostRun, *c.CheckResult) { return nil, &r }
	if st := env.State(); st != c.StateRunning {
		return bad(errorf("the sandbox is not running (%s)", st))
	}
	ops, err := env.HostOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	if err := ops.StubsInPlace(); err != nil {
		return bad(errorf("the suite's stub opener and notifier are not in place, so nothing was sent: %v", err))
	}
	h := &hostRun{env: env, ops: ops}
	for _, x := range []struct {
		role string
		dst  *c.Canary
	}{{RoleAliasOut, &h.out}, {RoleHostOut, &h.hostOut}} {
		cn, err := env.Canary(x.role)
		if err != nil {
			return bad(errorf("canary %s is missing: %v", x.role, err))
		}
		if s, err := env.Snapshot(cn); err != nil || len(s) == 0 {
			return bad(errorf("canary %s is not on the host", x.role))
		}
		*x.dst = cn
	}
	m := n8Of.FindStringSubmatch(h.out.Rel)
	if m == nil {
		return bad(errorf("canary %s has no nonce in its name", RoleAliasOut))
	}
	h.n8 = m[1]
	return h, nil
}

// lines counts line in the alias-out file name, from the host: 0 when the
// file is not there.
func (h *hostRun) lines(name, line string) (int, error) {
	b, err := h.ops.ReadCanary(h.out.Rel + "/" + name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := strings.Count(string(b), line)
	if n*len(line) != len(b) {
		return 0, fmt.Errorf("the alias output %s holds something the suite's script does not write", name)
	}
	return n, nil
}

func (h *hostRun) ran() (int, error) { return h.lines(c.OutRan, c.RanLine) }

func (h *hostRun) run(ctx context.Context, cmd c.Cmd, err error) (c.ExecResult, error) {
	if err != nil {
		return c.ExecResult{}, err
	}
	res, err := h.env.RunCmd(ctx, h.env.SessionUser(), cmd)
	if err == nil && res.RC < 0 {
		err = fmt.Errorf("the sandbox's shell did not run the command: %s", res.Msg)
	}
	return res, err
}

// marker is the positive control: conf-marker runs, once, and the host
// sees it run.
func (h *hostRun) marker(ctx context.Context, args ...string) *c.CheckResult {
	before, err := h.ran()
	if err != nil {
		r := errorf("positive control failed: %v", err)
		return &r
	}
	cmd, err := c.AgentRun(c.AliasMarker, args...)
	res, err := h.run(ctx, cmd, err)
	if err != nil {
		r := errorf("positive control failed: %v", err)
		return &r
	}
	if res.RC != 0 {
		r := errorf("positive control failed: %s exited %d%s", c.AliasMarker, res.RC, msgOf(res))
		return &r
	}
	after, err := h.ran()
	if err != nil {
		r := errorf("positive control failed: %v", err)
		return &r
	}
	if after != before+1 {
		r := errorf("positive control failed: %s ran %d times on the host, not once", c.AliasMarker, after-before)
		return &r
	}
	return nil
}

func msgOf(r c.ExecResult) string {
	if r.Msg == "" {
		return ""
	}
	return ": " + r.Msg
}

// refusedRun is an alias run that must not run: its exit status must be a
// refusal in ok, and the host must have run nothing.
func (h *hostRun) refusedRun(ctx context.Context, ok []int, name string, args ...string) *c.CheckResult {
	before, err := h.ran()
	if err != nil {
		r := errorf("%v", err)
		return &r
	}
	cmd, err := c.AgentRun(name, args...)
	res, err := h.run(ctx, cmd, err)
	after, rerr := h.ran()
	what := strings.Join(append([]string{name}, args...), " ")
	switch {
	case rerr != nil:
		r := errorf("the alias output could not be read back: %v", rerr)
		return &r
	case after != before:
		r := failf("run %s: the host ran the suite's script %d time(s)", what, after-before)
		return &r
	case err != nil:
		r := errorf("run %s: %v", what, err)
		return &r
	case res.RC == 0:
		r := failf("run %s: exited 0", what)
		return &r
	case res.RC == exitUsage && !strings.HasPrefix(res.Msg, "caboose-agent: run:"):
		r := errorf("run %s: exited 2 without caboose-agent's refusal%s", what, msgOf(res))
		return &r
	case !slices.Contains(ok, res.RC):
		r := failf("run %s: exited %d, not a refusal%s", what, res.RC, msgOf(res))
		return &r
	}
	return nil
}

// refusedHost is a host command that must be refused for host_exec: 126
// with a message naming it, and nothing new in h1-out.
func (h *hostRun) refusedHost(ctx context.Context, withDir bool, argv ...string) *c.CheckResult {
	before, err := h.env.Snapshot(h.hostOut)
	if err != nil || len(before) == 0 {
		r := errorf("canary %s could not be read: %v", RoleHostOut, err)
		return &r
	}
	var dir *c.SPath
	if withDir {
		p, err := c.SandboxPath(h.hostOut)
		if err != nil {
			r := errorf("%v", err)
			return &r
		}
		dir = &p
	}
	cmd, err := c.AgentHost(dir, argv...)
	res, err := h.run(ctx, cmd, err)
	after, serr := h.env.Snapshot(h.hostOut)
	what := strings.Join(argv, " ")
	switch {
	case serr != nil:
		r := errorf("canary %s could not be read back: %v", RoleHostOut, serr)
		return &r
	case changed(before, after):
		r := failf("host %s: the host ran it (canary %s changed)", what, RoleHostOut)
		return &r
	case err != nil:
		r := errorf("host %s: %v", what, err)
		return &r
	case res.RC == 0:
		r := failf("host %s: exited 0", what)
		return &r
	case res.RC != exitUnavailable:
		r := failf("host %s: exited %d, not host exec's refusal%s", what, res.RC, msgOf(res))
		return &r
	case !strings.Contains(res.Out, "host_exec"):
		r := errorf("host %s: refused, but not for host_exec%s", what, msgOf(res))
		return &r
	}
	return nil
}

// changed reports whether two snapshots of a canary differ: a path new,
// gone, or of another identity.
func changed(before, after map[string]c.Identity) bool {
	if len(before) != len(after) {
		return true
	}
	for p, b := range before {
		a, ok := after[p]
		if !ok {
			return true
		}
		if a.Mode.IsDir() && b.Mode.IsDir() {
			if a.Dev != b.Dev || a.Ino != b.Ino || a.Mode != b.Mode {
				return true
			}
			continue
		}
		if a != b {
			return true
		}
	}
	return false
}

func pass(lines ...string) c.CheckResult { return c.CheckResult{Status: c.Pass, Lines: lines} }

func h1() c.Check {
	return hostCheck("H1", "a host command without host_exec is refused", "host exec off",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			if r := h.refusedHost(ctx, false, "true"); r != nil {
				return *r
			}
			if r := h.refusedHost(ctx, true, "touch", "conf-"+h.n8+"-h1-marker"); r != nil {
				return *r
			}
			return pass()
		})
}

func h2() c.Check {
	return hostCheck("H2", "an undefined alias is refused", "only defined aliases run",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			if r := h.refusedRun(ctx, []int{exitUnknown, exitDenied}, "conf-nosuch"); r != nil {
				return *r
			}
			for _, name := range []string{"../../usr/bin/env", "conf-marker/x", "conf marker", "/usr/bin/env"} {
				if r := h.refusedRun(ctx, []int{exitUnknown, exitDenied, exitUsage}, name); r != nil {
					return *r
				}
			}
			return pass("not tried: a name holding NUL, which no command line can carry",
				"names with a slash or a space are refused in the sandbox, before the host is asked")
		})
}

func h3() c.Check {
	return hostCheck("H3", "an alias runs only as defined", "alias arguments bound",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			if r := h.marker(ctx, c.AliasParam+"="+c.AliasParamValue); r != nil {
				return *r
			}
			for _, args := range [][]string{
				{c.AliasParam + "=two"},
				{"other=x"},
				{c.AliasParam + "=" + c.AliasParamValue, "other=x"},
			} {
				if r := h.refusedRun(ctx, []int{exitDenied}, c.AliasMarker, args...); r != nil {
					return *r
				}
			}
			if r := h.refusedRun(ctx, []int{exitDenied, exitUsage}, c.AliasMarker, "extra"); r != nil {
				return *r
			}
			return pass()
		})
}

// listed is the alias names the sandbox sees.
func (h *hostRun) listed(ctx context.Context) ([]string, *c.CheckResult) {
	res, err := h.run(ctx, c.AgentAliases(), nil)
	if err != nil {
		r := errorf("aliases: %v", err)
		return nil, &r
	}
	if res.RC != 0 {
		r := errorf("aliases exited %d%s", res.RC, msgOf(res))
		return nil, &r
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(res.Out), &list); err != nil {
		r := errorf("aliases did not print a list: %v", err)
		return nil, &r
	}
	names := make([]string, len(list))
	for i, a := range list {
		names[i] = a.Name
	}
	return names, nil
}

// unchangedConfig is a FAIL when config.toml's hash moved from before.
func (h *hostRun) unchangedConfig(before string) *c.CheckResult {
	now, err := h.ops.ConfigHash()
	if err != nil {
		r := errorf("config.toml could not be read back: %v", err)
		return &r
	}
	if now != before {
		r := failf("the host's config.toml changed")
		return &r
	}
	return nil
}

// evilAlias is a definition of alias name whose command is the suite's
// marker script, as the sandbox would write one.
func (h *hostRun) evilAlias(name string) ([]string, *c.CheckResult) {
	cmdPath, err := h.ops.AliasCommand(c.AliasMarker)
	if err != nil {
		r := errorf("%v", err)
		return nil, &r
	}
	return []string{"[alias." + name + "]", `command = ["` + cmdPath + `"]`}, nil
}

// write appends lines to f in the sandbox, which must work: it is the
// sandbox's own file.
func (h *hostRun) write(ctx context.Context, f c.HomeFile, lines []string) *c.CheckResult {
	cmd, err := c.AppendLines(f, lines...)
	res, err := h.run(ctx, cmd, err)
	if err != nil {
		r := errorf("positive control failed: writing %s: %v", f, err)
		return &r
	}
	if res.RC != 0 {
		r := errorf("positive control failed: writing %s exited %d%s", f, res.RC, msgOf(res))
		return &r
	}
	return nil
}

func h4() c.Check {
	const evil = "conf-evil"
	return hostCheck("H4", "the sandbox cannot define an alias", "aliases are the host's",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			names, r := h.listed(ctx)
			if r != nil {
				return *r
			}
			if !slices.Contains(names, c.AliasMarker) {
				return errorf("positive control failed: aliases does not list %s", c.AliasMarker)
			}
			hash, err := h.ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			def, r := h.evilAlias(evil)
			if r != nil {
				return *r
			}
			if r := h.write(ctx, c.SandboxConfigFile, def); r != nil {
				return *r
			}
			sf, err := c.SyncFile(h.n8, "alias")
			if err != nil {
				return errorf("%v", err)
			}
			if r := h.write(ctx, sf, def); r != nil {
				return *r
			}
			if err := h.ops.Wait(ctx, settle); err != nil {
				return errorf("%v", err)
			}
			if names, r = h.listed(ctx); r != nil {
				return *r
			}
			if slices.Contains(names, evil) {
				return failf("aliases lists %s, which the sandbox wrote", evil)
			}
			if r := h.refusedRun(ctx, []int{exitUnknown, exitDenied}, evil); r != nil {
				return *r
			}
			if r := h.unchangedConfig(hash); r != nil {
				return *r
			}
			return pass()
		})
}

func h5() c.Check {
	const prop = "conf-prop"
	return hostCheck("H5", "a proposal is only a proposal", "proposals need caboose apply",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			hash, err := h.ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			def, r := h.evilAlias(prop)
			if r != nil {
				return *r
			}
			pf, err := c.ProposalFile(h.n8, "alias")
			if err != nil {
				return errorf("%v", err)
			}
			if r := h.write(ctx, pf, def); r != nil {
				return *r
			}
			if err := h.ops.Wait(ctx, settle); err != nil {
				return errorf("%v", err)
			}
			if r := h.refusedRun(ctx, []int{exitUnknown, exitDenied}, prop); r != nil {
				return *r
			}
			if r := h.unchangedConfig(hash); r != nil {
				return *r
			}
			return pass("a proposal is a file for caboose apply on the host; no request is filed for it")
		})
}

func h6() c.Check {
	return hostCheck("H6", "an alias that asks does not run unanswered", "ask needs an answer",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			// The stubs must be what a notification reaches: one sent must
			// be recorded by them.
			calls, err := h.ops.StubCalls()
			if err != nil {
				return errorf("the stubs could not be read: %v", err)
			}
			cmd, err := c.AgentNotify("conf-" + h.n8 + " H6")
			res, err := h.run(ctx, cmd, err)
			if err != nil || res.RC != 0 {
				return errorf("positive control failed: notify did not work (%v%s)", err, msgOf(res))
			}
			if now, err := h.ops.StubCalls(); err != nil || now <= calls {
				return errorf("positive control failed: the stub notifier recorded nothing, so a notification may reach the person")
			}
			if r := h.marker(ctx); r != nil {
				return *r
			}
			before, err := h.ran()
			if err != nil {
				return errorf("%v", err)
			}
			cmd, err = c.AgentRun(c.AliasAsk)
			res, err = h.run(ctx, cmd, err)
			after, rerr := h.ran()
			switch {
			case rerr != nil:
				return errorf("the alias output could not be read back: %v", rerr)
			case after != before:
				return failf("run %s: the host ran it unanswered", c.AliasAsk)
			case err != nil:
				return errorf("run %s: %v", c.AliasAsk, err)
			case res.RC == 0:
				return failf("run %s: exited 0", c.AliasAsk)
			case res.RC != exitPending || !strings.Contains(res.Out, "pending"):
				return errorf("run %s: exited %d, not pending%s", c.AliasAsk, res.RC, msgOf(res))
			}
			if err := h.ops.Wait(ctx, settle); err != nil {
				return errorf("%v", err)
			}
			if now, err := h.ran(); err != nil || now != before {
				return failf("run %s: the host ran it later, unanswered", c.AliasAsk)
			}
			return pass("not waited for: the request's expiry (the configuration has no setting to shorten it)")
		})
}

func h7() c.Check {
	return hostileOnly(hostCheck("H7", "hostile: an alias run in a writable path is refused", "hostile refuses writable alias paths",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			return h.trustRefusal(ctx, c.AliasWritable)
		}))
}

// trustRefusal runs alias name, which only trust = hostile refuses: it
// must be denied, naming trust, with nothing run.
func (h *hostRun) trustRefusal(ctx context.Context, name string) c.CheckResult {
	before, err := h.ran()
	if err != nil {
		return errorf("%v", err)
	}
	cmd, err := c.AgentRun(name)
	res, err := h.run(ctx, cmd, err)
	after, rerr := h.ran()
	switch {
	case rerr != nil:
		return errorf("the alias output could not be read back: %v", rerr)
	case after != before:
		return failf("run %s: the host ran it", name)
	case err != nil:
		return errorf("run %s: %v", name, err)
	case res.RC == 0:
		return failf("run %s: exited 0", name)
	case res.RC != exitDenied:
		return failf("run %s: exited %d, not refused%s", name, res.RC, msgOf(res))
	case !strings.Contains(res.Out, "trust"):
		return errorf("run %s: refused, but not for trust%s", name, msgOf(res))
	}
	return pass("not tried: refusing such an alias when config.toml is loaded; the host refuses it when it is run")
}

func h8() c.Check {
	return hostileOnly(hostCheck("H8", "hostile: host_exec is refused even when set", "hostile refuses host exec",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			on, _, err := h.ops.HostExecOffered(c.ConfigVariant{Trust: c.TrustFallible, Isolation: "vm", HostExec: true})
			if err != nil {
				return errorf("%v", err)
			}
			if !on {
				return errorf("positive control failed: a fallible configuration with host_exec on offers no host exec either")
			}
			on, why, err := h.ops.HostExecOffered(c.ConfigVariant{Trust: c.TrustHostile, Isolation: "vm", HostExec: true})
			switch {
			case err != nil:
				return errorf("%v", err)
			case on:
				return failf("with trust = hostile and host_exec = true, the hello offers host exec")
			case !strings.Contains(why, "trust"):
				return failf("with trust = hostile and host_exec = true, host exec is off, but not for trust: %s", why)
			}
			if r := h.marker(ctx); r != nil {
				return *r
			}
			if r := h.refusedHost(ctx, false, "true"); r != nil {
				return *r
			}
			return pass("the running environment keeps host_exec off (the mount audit requires it); host_exec = true was judged on a copy")
		}))
}

// allowedEnv are the variables an alias may see: the daemon's three, and
// what the script's own shell adds.
var allowedEnv = []string{"PATH", "HOME", "LANG", "PWD", "OLDPWD", "SHLVL", "_"}

var envLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)

func h9() c.Check {
	return hostCheck("H9", "an alias's environment is minimal", "alias environment",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			envRel := h.out.Rel + "/" + c.OutEnv
			before, berr := h.env.Identity(envRel)
			if r := h.marker(ctx); r != nil {
				return *r
			}
			after, err := h.env.Identity(envRel)
			if err != nil || berr == nil && before == after {
				return errorf("positive control failed: %s did not write its environment", c.AliasMarker)
			}
			b, err := h.ops.ReadCanary(envRel)
			if err != nil {
				return errorf("the alias's environment could not be read: %v", err)
			}
			var names, extra []string
			for _, l := range strings.Split(string(b), "\n") {
				if m := envLine.FindStringSubmatch(l); m != nil {
					names = append(names, m[1])
				}
			}
			if !slices.Contains(names, "PATH") {
				return errorf("positive control failed: the alias's environment has no PATH")
			}
			canary := h.ops.EnvCanary()
			for _, n := range names {
				if n == canary {
					return failf("the alias saw %s, which the suite exported in its own environment", canary)
				}
				if !slices.Contains(allowedEnv, n) {
					extra = append(extra, n)
				}
			}
			if len(extra) > 0 {
				slices.Sort(extra)
				return failf("the alias saw variables beyond PATH, HOME and LANG: %s", strings.Join(slices.Compact(extra), ", "))
			}
			return pass()
		})
}

func h10() c.Check {
	return hostCheck("H10", "an alias's processes end with its caller", "alias lifetime",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			starts, err := h.lines(c.OutStart, c.StartLine)
			if err != nil {
				return errorf("%v", err)
			}
			dones, err := h.lines(c.OutDone, c.DoneLine)
			if err != nil {
				return errorf("%v", err)
			}
			cmd, err := c.AgentRunFor(sleepKill, c.AliasSleep)
			res, err := h.run(ctx, cmd, err)
			if err != nil {
				return errorf("run %s: %v", c.AliasSleep, err)
			}
			if res.RC != exitTimeout && res.RC != exitTerm {
				return errorf("positive control failed: run %s exited %d, not ended by its caller's timeout%s", c.AliasSleep, res.RC, msgOf(res))
			}
			if now, err := h.lines(c.OutStart, c.StartLine); err != nil || now != starts+1 {
				return errorf("positive control failed: %s did not start on the host", c.AliasSleep)
			}
			if err := h.ops.Wait(ctx, sleepWait); err != nil {
				return errorf("%v", err)
			}
			now, err := h.lines(c.OutDone, c.DoneLine)
			if err != nil {
				return errorf("%v", err)
			}
			if now != dones {
				return failf("a process %s started outlived its caller by %s", c.AliasSleep, sleepWait)
			}
			return pass()
		})
}
