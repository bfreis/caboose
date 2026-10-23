package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	c "github.com/bfreis/caboose/internal/conformance"
)

// The trust checks, T1-T6: what trust = "hostile" holds the launch to.
// T1 and T2 ask the launcher for its decision only, through HostOps, which
// never starts a session: no command of theirs reaches the sandbox, and
// each counts the Claude Code processes of the environment before and
// after.

const (
	groupTrust = "trust"
	stepTrust  = "trust levels at launch"

	// profileName is the name of the throwaway configuration's profiles.
	profileName = "conformance"

	// stateAbsent is the backend's state of a sandbox that does not exist.
	stateAbsent = "absent"
)

// TrustChecks are T1-T6 in the order they run.
func TrustChecks() []c.Check {
	return []c.Check{t1(), t2(), t3(), t4(), t5(), t6()}
}

func trustCheck(id, title, control string, class c.Class, run func(context.Context, c.Env, c.HostOps) c.CheckResult) c.Check {
	return c.Check{
		ID: id, Title: title, Control: control, Step: stepTrust, Group: groupTrust, Class: class,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			ops, err := env.HostOps()
			if err != nil {
				return errorf("%v", err)
			}
			return run(ctx, env, ops)
		},
	}
}

// noClaude is the count of the environment's Claude Code processes, which
// must be none.
func noClaude(ops c.HostOps, when string) *c.CheckResult {
	n, err := ops.ClaudeProcesses()
	if err != nil {
		r := errorf("the environment's processes could not be listed: %v", err)
		return &r
	}
	if n != 0 {
		r := failf("%s: %d Claude Code process(es) of the environment are running", when, n)
		if when == "before" {
			r = errorf("before: %d Claude Code process(es) of the environment are running", n)
		}
		return &r
	}
	return nil
}

func t1() c.Check {
	ck := trustCheck("T1", "hostile requires vm", "hostile needs vm", c.Write,
		func(ctx context.Context, env c.Env, ops c.HostOps) c.CheckResult {
			if r := noClaude(ops, "before"); r != nil {
				return *r
			}
			if st := env.State(); st != stateAbsent {
				return errorf("a sandbox of the environment exists before the launcher was asked (%s)", st)
			}
			res, err := ops.Create(ctx)
			if r := noClaude(ops, "after"); r != nil {
				return *r
			}
			st := env.State()
			switch {
			case err != nil:
				return errorf("the launcher's decision could not be run: %v", err)
			case res.Created || st != stateAbsent:
				return failf("the launcher created the sandbox under trust = hostile and isolation %s", env.Isolation())
			case !res.Refused:
				return failf("the launcher did not refuse trust = hostile under isolation %s", env.Isolation())
			case !strings.Contains(res.Message, "trust") || !strings.Contains(res.Message, "vm"):
				return errorf("the launcher refused, but not for trust: %s", res.Message)
			}
			return pass()
		})
	ck.Trusts = []string{c.TrustHostile}
	ck.Isolations = []string{"container", "gvisor"}
	return ck
}

func t2() c.Check {
	ck := trustCheck("T2", "a sandbox made under fallible is refused under hostile", "hostile refuses an existing sandbox", c.Write,
		func(ctx context.Context, env c.Env, ops c.HostOps) (out c.CheckResult) {
			if st := env.State(); st != c.StateRunning {
				return errorf("the sandbox is not running (%s)", st)
			}
			if r := noClaude(ops, "before"); r != nil {
				return *r
			}
			labels := env.Labels()
			hash, err := ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			pc, err := ops.Attach(ctx)
			if err != nil {
				return errorf("positive control failed: %v", err)
			}
			if pc.Refused {
				return errorf("positive control failed: the attach is refused under fallible too: %s", pc.Message)
			}
			if err := ops.SetTrust(c.TrustHostile); err != nil {
				return errorf("config.toml could not be changed: %v", err)
			}
			defer func() {
				err := ops.SetTrust(c.TrustFallible)
				if err == nil {
					var now string
					if now, err = ops.ConfigHash(); err == nil && now != hash {
						err = fmt.Errorf("its hash differs from the run's")
					}
				}
				if err != nil {
					out = errorf("config.toml was not put back as it was (%v); the run's configuration is no longer the audited one", err)
				}
			}()
			res, err := ops.Attach(ctx)
			if r := noClaude(ops, "after"); r != nil {
				return *r
			}
			switch {
			case err != nil:
				return errorf("the launcher's decision could not be run: %v", err)
			case !res.Refused:
				return failf("under trust = hostile the launcher attaches to a sandbox made under fallible")
			case !strings.Contains(res.Message, "caboose restart"):
				return failf("the launcher refused without saying caboose restart: %s", res.Message)
			}
			if st := env.State(); st != c.StateRunning {
				return failf("the sandbox is %s after the refusal", st)
			}
			if !maps.Equal(labels, env.Labels()) {
				return failf("the sandbox's labels changed after the refusal")
			}
			return pass()
		})
	ck.Trusts = []string{c.TrustFallible}
	return ck
}

func t3() c.Check {
	ck := hostCheck("T3", "hostile cannot be turned off from the sandbox", "trust is the host's",
		func(ctx context.Context, h *hostRun) c.CheckResult {
			if r := h.marker(ctx); r != nil {
				return *r
			}
			hash, err := h.ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			line := []string{`trust = "fallible"`}
			pf, err := c.ProposalFile(h.n8, "trust")
			if err != nil {
				return errorf("%v", err)
			}
			sf, err := c.SyncFile(h.n8, "trust")
			if err != nil {
				return errorf("%v", err)
			}
			for _, f := range []c.HomeFile{pf, sf} {
				if r := h.write(ctx, f, line); r != nil {
					return *r
				}
			}
			if err := h.ops.Wait(ctx, settle); err != nil {
				return errorf("%v", err)
			}
			if t, err := h.ops.EffectiveTrust(); err != nil {
				return errorf("the host's trust could not be read: %v", err)
			} else if t != c.TrustHostile {
				return failf("the host now decides by trust %q", t)
			}
			if r := h.unchangedConfig(hash); r != nil {
				return *r
			}
			r := h.trustRefusal(ctx, c.AliasWritable)
			r.Lines = []string{"the link's own trust message is the daemon checks' to send",
				"not tried: the sandbox config, which holds no trust key and whose earlier tables would take one"}
			return r
		})
	ck.ID, ck.Group, ck.Step = "T3", groupTrust, stepTrust
	return hostileOnly(ck)
}

func t4() c.Check {
	ck := trustCheck("T4", "the labels say what runs", "labels describe the sandbox", c.Observe,
		func(ctx context.Context, env c.Env, ops c.HostOps) c.CheckResult {
			if st := env.State(); st != c.StateRunning {
				return errorf("the sandbox is not running (%s)", st)
			}
			labels := env.Labels()
			if len(labels) == 0 {
				return errorf("the sandbox's labels could not be read")
			}
			cn, err := env.Canary(RoleHostOut)
			if err != nil {
				return errorf("canary %s is missing: %v", RoleHostOut, err)
			}
			m := n8Of.FindStringSubmatch(cn.Rel)
			if m == nil {
				return errorf("canary %s has no nonce in its name", RoleHostOut)
			}
			iso, trust := env.Isolation(), env.Trust()
			network := ""
			if iso == "vm" {
				network = "nat"
				if trust == c.TrustHostile {
					network = "none"
				}
			}
			want := map[string]string{
				assets.LabelTrust:     trust,
				assets.LabelIsolation: iso,
				assets.LabelProfile:   iso + "." + profileName,
				assets.LabelNetwork:   network,
			}
			var bad []string
			for _, k := range slices.Sorted(maps.Keys(want)) {
				if got, ok := labels[k]; !ok || got != want[k] {
					bad = append(bad, fmt.Sprintf("%s is %q, want %q", k, got, want[k]))
				}
			}
			ro, dev := "/work/conf-"+m[1]+"-ro", "/work/conf-"+m[1]+"-dev"
			var w map[string][]string
			if err := json.Unmarshal([]byte(labels[assets.LabelWritable]), &w); err != nil {
				bad = append(bad, assets.LabelWritable+" cannot be read")
			} else if roParts, ok := w[ro]; len(w) != 2 || !ok || len(roParts) != 0 || !slices.Equal(w[dev], []string{"part"}) {
				bad = append(bad, assets.LabelWritable+" does not say the configured writable part alone")
			}
			if len(bad) > 0 {
				return failf("%s", strings.Join(bad, "; "))
			}
			return pass("not tried: a change to the configuration noting a restart")
		})
	ck.Roles = []string{RoleHostOut}
	return ck
}

func t5() c.Check {
	return trustCheck("T5", "hostile with egress = false does not load", "hostile keeps egress on", c.Observe,
		func(ctx context.Context, env c.Env, ops c.HostOps) c.CheckResult {
			if err := ops.TryConfig(c.ConfigVariant{Trust: c.TrustHostile, Isolation: "vm"}); err != nil {
				return errorf("positive control failed: a hostile vm configuration does not load: %v", err)
			}
			off := false
			err := ops.TryConfig(c.ConfigVariant{Trust: c.TrustHostile, Isolation: "vm", Egress: &off})
			switch {
			case err == nil:
				return failf("a hostile vm configuration with egress = false loads")
			case !strings.Contains(err.Error(), "egress"):
				return failf("refused, without naming egress: %v", err)
			}
			return pass()
		})
}

// T6: under hostile, ssh_agent unset forwards no agent: not the host's
// $SSH_AUTH_SOCK, which fallible would forward. The suite's own
// environment forwards none either way (ssh_agent is none, which the
// mount audit checks); this asks the launcher's decision on a copy.
func t6() c.Check {
	ck := trustCheck("T6", "the SSH agent under hostile", "hostile forwards no SSH agent unless ssh_agent names one", c.Observe,
		func(_ context.Context, _ c.Env, ops c.HostOps) c.CheckResult {
			const standIn = "/conformance-stand-in/agent.sock"
			sock, why, err := ops.SSHAgentCarried(c.ConfigVariant{Trust: c.TrustFallible, Isolation: "vm"}, standIn)
			switch {
			case err != nil:
				return errorf("the agent a fallible link would carry could not be asked: %v", err)
			case sock != standIn:
				return errorf("positive control failed: with ssh_agent unset a fallible link carries %q, not the stand-in for the host's default agent (%s), so none under hostile tells nothing", sock, why)
			}
			sock, why, err = ops.SSHAgentCarried(c.ConfigVariant{Trust: c.TrustHostile, Isolation: "vm"}, standIn)
			switch {
			case err != nil:
				return errorf("the agent a hostile link would carry could not be asked: %v", err)
			case sock != "":
				return failf("with ssh_agent unset a hostile link carries %q into the sandbox (%s)", sock, why)
			case !strings.Contains(why, "hostile"):
				return failf("a hostile link carries no agent, but does not say that hostile is why: %q", why)
			}
			return pass()
		})
	ck.Trusts = []string{c.TrustHostile}
	ck.Isolations = []string{"vm"}
	return ck
}

// ForRun are the host and trust checks a run under isolation and trust
// runs: under trust = "hostile" with any isolation but vm, the launcher
// refuses to create the sandbox, so T1 is the run.
func ForRun(isolation, trust string) []c.Check {
	all := append(HostChecks(), TrustChecks()...)
	if trust == c.TrustHostile && isolation != "vm" {
		all = []c.Check{t1()}
	}
	var out []c.Check
	for _, ck := range all {
		if ck.Applies(isolation, trust) {
			out = append(out, ck)
		}
	}
	return out
}
