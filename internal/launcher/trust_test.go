package launcher

import (
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
)

func hostileApp(t *testing.T, iso string, allow, deny []string) *boxApp {
	t.Helper()
	b := newBoxApp(t, iso, runningBox(iso))
	b.Cfg.Trust = config.TrustHostile
	b.Cfg.Profile = "vm.default"
	b.Cfg.EgressAllow = allow
	b.Cfg.EgressDeny = deny
	return b
}

func TestHostileRefusedOutsideVM(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationGVisor} {
		t.Run(iso, func(t *testing.T) {
			b := hostileApp(t, iso, claudeHosts, nil)
			err := b.Restart()
			if err == nil || !strings.Contains(err.Error(), "trust = hostile needs a vm isolation profile: ") {
				t.Fatalf("create: %v", err)
			}
			if !strings.Contains(err.Error(), `trust = "fallible"`) {
				t.Errorf("no fix named: %v", err)
			}
			// Attaching to a sandbox made under it.
			b = hostileApp(t, iso, claudeHosts, nil)
			err = b.checkExisting()
			if err == nil || !strings.HasPrefix(err.Error(), "trust = hostile needs a vm isolation profile: ") {
				t.Fatalf("attach: %v", err)
			}
		})
	}
}

func TestHostileAttachToContainerCreatedElsewhere(t *testing.T) {
	b := hostileApp(t, isolationVM, claudeHosts, nil)
	b.box.SandboxLabels[assets.LabelIsolation] = isolationContainer
	err := b.checkExisting()
	if err == nil || !strings.Contains(err.Error(), "needs a vm isolation profile") || !strings.Contains(err.Error(), "caboose restart") {
		t.Fatalf("%v", err)
	}
	b.box.SandboxLabels[assets.LabelIsolation] = isolationVM
	b.box.SandboxLabels[assets.LabelNetwork] = networkNone
	if err := b.checkExisting(); err != nil {
		t.Fatalf("a vm sandbox: %v", err)
	}
}

// An existing sandbox is trusted as hostile only when its labels say it
// was created as one: a missing or other label refuses, until restarted.
func TestHostileAttachFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(l map[string]string)
		want   string
	}{
		{"nat", func(l map[string]string) { l[assets.LabelNetwork] = networkNAT }, "created with network nat"},
		{"no network label", func(l map[string]string) { delete(l, assets.LabelNetwork) }, "none recorded"},
		{"empty network", func(l map[string]string) { l[assets.LabelNetwork] = "" }, "none recorded"},
		{"no isolation label", func(l map[string]string) { delete(l, assets.LabelIsolation) }, "records no isolation"},
		{"empty isolation", func(l map[string]string) { l[assets.LabelIsolation] = "" }, "records no isolation"},
		{"container", func(l map[string]string) { l[assets.LabelIsolation] = isolationContainer }, "needs a vm isolation profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := hostileApp(t, isolationVM, claudeHosts, nil)
			b.box.SandboxLabels[assets.LabelNetwork] = networkNone
			tc.mutate(b.box.SandboxLabels)
			err := b.checkExisting()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "caboose restart") {
				t.Fatalf("%v", err)
			}
		})
	}
}

// What boots: the machine the VM host gives the backend.
func TestMachineNetwork(t *testing.T) {
	for _, tc := range []struct{ trust, want string }{
		{config.TrustFallible, "nat"}, {"", "nat"}, {config.TrustHostile, "none"},
	} {
		b := newBoxApp(t, isolationVM, runningBox(isolationVM))
		b.Cfg.Trust = tc.trust
		m, err := (&vmHost{a: b.App}).Machine()
		if err != nil {
			t.Fatal(err)
		}
		if m.Network != tc.want {
			t.Errorf("trust %q: network %q, want %q", tc.trust, m.Network, tc.want)
		}
	}
}

func TestClaudeHostsNeedPort443(t *testing.T) {
	a := &App{Cfg: &config.Config{Isolation: isolationVM, Trust: config.TrustHostile, EgressAllow: claudeHosts, EgressPorts: "22 80"}}
	if p := a.claudeHostsProblem(); !strings.Contains(p, "egress_ports") || !strings.Contains(p, "443") {
		t.Errorf("problem %q", p)
	}
	for _, ports := range []string{"", "22 80 443", "400-500"} {
		a.Cfg.EgressPorts = ports
		if p := a.claudeHostsProblem(); p != "" {
			t.Errorf("ports %q: %s", ports, p)
		}
	}
}

func TestHostExecIgnoredNotes(t *testing.T) {
	a := &App{Cfg: &config.Config{Isolation: isolationVM, Trust: config.TrustHostile, HostExec: true}}
	c := &checkup{}
	a.doctorHostExec(c)
	if len(c.rows) != 0 {
		t.Errorf("doctorHostExec says on under hostile: %v", c.rows)
	}
	if a.hostExecSummary() != "" {
		t.Error("status says host exec is on under hostile")
	}
	a.Cfg.Trust = config.TrustFallible
	a.doctorHostExec(c)
	if len(c.rows) != 1 || a.hostExecSummary() == "" {
		t.Error("fallible host exec not shown")
	}
}

// The drift warning reaches status and a launch; doctor shows its rows and
// the trust row is wired in.
func TestTrustDriftWarned(t *testing.T) {
	setup := func() *boxApp {
		b := newBoxApp(t, isolationVM, runningBox(isolationVM)) // created fallible, nat
		b.Cfg.Trust = config.TrustHostile
		b.Cfg.EgressAllow = claudeHosts
		return b
	}
	b := setup()
	b.warnIfTrustDrifted()
	if out, errs := b.said(); !strings.Contains(errs+out, "created with trust fallible") || !strings.Contains(errs+out, "caboose restart") {
		t.Errorf("warn: %q %q", out, errs)
	}
	b = setup()
	if err := b.Status(); err != nil {
		t.Fatal(err)
	}
	if out, errs := b.said(); !strings.Contains(errs+out, "created with trust fallible") {
		t.Errorf("status: %q %q", out, errs)
	}
	// A launch of a fallible sandbox the configuration now calls hostile
	// refuses (the network label), so the warning shows for a fallible one
	// drifting the other way.
	b = newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.box.SandboxLabels[assets.LabelTrust] = config.TrustHostile
	b.box.SandboxLabels[assets.LabelNetwork] = networkNone
	b.Cfg.Trust = config.TrustFallible
	b.mountLocal("local/linux-arm64")
	_ = b.ensureRunning(false)
	if out, errs := b.said(); !strings.Contains(errs+out, "created with trust hostile") {
		t.Errorf("ensureRunning: %q %q", out, errs)
	}
}

func TestDoctorShowsTrust(t *testing.T) {
	b := newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.Cfg.Trust = config.TrustHostile
	c := &checkup{}
	b.doctorConfig(c)
	if len(c.rows) == 0 || c.rows[0].label != "trust" {
		t.Errorf("doctorConfig rows: %v", c.rows)
	}
	c = &checkup{}
	b.doctorContainer(c, true)
	found := false
	for _, r := range c.rows {
		if r.label == "trust" && r.level == levelProblem && strings.Contains(r.text, "created with trust fallible") {
			found = true
		}
	}
	if !found {
		t.Errorf("no trust drift row: %v", c.rows)
	}
}

func TestFallibleAcceptsAnyIsolation(t *testing.T) {
	b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
	if err := b.checkTrustExisting(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustAndNetworkLabels(t *testing.T) {
	for _, tc := range []struct {
		name, iso, trust, network string
		egress                    bool
	}{
		{"docker", isolationContainer, "fallible", "", false},
		{"gvisor", isolationGVisor, "fallible", "", false},
		{"vm", isolationVM, "fallible", "nat", true},
		{"vm, trust unset", isolationVM, "", "nat", true},
		{"vm, hostile", isolationVM, "hostile", "none", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := runningBox(tc.iso)
			b := newBoxApp(t, tc.iso, box)
			b.Cfg.Trust = tc.trust
			b.Cfg.EgressAllow = claudeHosts
			b.Cfg.Egress = true
			if err := b.Restart(); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			labels, _ := box.Labels()
			wantTrust := tc.trust
			if wantTrust == "" {
				wantTrust = "fallible"
			}
			if got, ok := labels[assets.LabelTrust]; !ok || got != wantTrust {
				t.Errorf("trust label %q (set %v)", got, ok)
			}
			if got, ok := labels[assets.LabelNetwork]; !ok || got != tc.network {
				t.Errorf("network label %q (set %v)", got, ok)
			}
			if got, ok := labels[assets.LabelEgress]; !ok || got != egressLabel(tc.egress) {
				t.Errorf("egress label %q (set %v)", got, ok)
			}
		})
	}
}

func TestVMNetworkAndHostileEgress(t *testing.T) {
	a := &App{Cfg: &config.Config{Isolation: isolationVM, Egress: false, Trust: config.TrustHostile}}
	if got := a.vmNetwork(); got != "none" {
		t.Errorf("hostile network %q", got)
	}
	if on, _ := a.egressOn(); !on {
		t.Error("hostile has egress off")
	}
	a.Cfg.Trust = config.TrustFallible
	if got := a.vmNetwork(); got != "nat" {
		t.Errorf("fallible network %q", got)
	}
	if on, _ := a.egressOn(); on {
		t.Error("fallible egress = false turned on")
	}
}

func TestTrustDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		trust  string
		drift  string
	}{
		{"same", map[string]string{assets.LabelIsolation: "vm", assets.LabelTrust: "fallible", assets.LabelNetwork: "nat"}, "fallible", ""},
		{"same hostile", map[string]string{assets.LabelIsolation: "vm", assets.LabelTrust: "hostile", assets.LabelNetwork: "none"}, "hostile", ""},
		{"turned hostile", map[string]string{assets.LabelIsolation: "vm", assets.LabelTrust: "fallible", assets.LabelNetwork: "nat"}, "hostile", "created with trust fallible"},
		{"no labels", map[string]string{assets.LabelIsolation: "vm"}, "fallible", "records no trust"},
		{"network only", map[string]string{assets.LabelIsolation: "vm", assets.LabelTrust: "hostile", assets.LabelNetwork: "nat"}, "hostile", "created with network nat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{Status: "running", SandboxLabels: tc.labels}
			a := &App{Cfg: &config.Config{Container: "box", Isolation: "vm", Trust: tc.trust}, Backend: box}
			got := a.trustDrift() + a.networkDrift()
			if (tc.drift == "") != (got == "") || !strings.Contains(got, tc.drift) {
				t.Errorf("drift %q, want %q", got, tc.drift)
			}
		})
	}
}

func TestClaudeHosts(t *testing.T) {
	cfg := func(allow, deny []string) *config.Config {
		return &config.Config{Isolation: isolationVM, Trust: config.TrustHostile, Profile: "vm.dev", EgressAllow: allow, EgressDeny: deny}
	}
	for _, tc := range []struct {
		name        string
		allow, deny []string
		want        []string // substrings of the problem; nil for none
	}{
		{"exact", claudeHosts, nil, nil},
		{"patterns", []string{"*.anthropic.com", "*.claude.ai", "claude.ai", "*.claude.com"}, nil, nil},
		{"none", nil, nil, []string{"api.anthropic.com", "claude.ai", `egress_allow = ["api.anthropic.com"`, "[vm.dev]"}},
		{"one missing", claudeHosts[1:], nil, []string{"does not name api.anthropic.com, which"}},
		{"keeps the existing", append([]string{"git.corp"}, claudeHosts[1:]...), nil, []string{`egress_allow = ["git.corp", "claude.ai", "platform.claude.com", "downloads.claude.ai", "api.anthropic.com"]`}},
		{"denied", claudeHosts, []string{"claude.ai"}, []string{"egress_deny names claude.ai"}},
		{"deny pattern", claudeHosts, []string{"*.anthropic.com"}, []string{"egress_deny names api.anthropic.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{Cfg: cfg(tc.allow, tc.deny)}
			got := a.claudeHostsProblem()
			if tc.want == nil && got != "" {
				t.Fatalf("problem: %s", got)
			}
			if tc.want != nil && got == "" {
				t.Fatal("no problem")
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q lacks %q", got, w)
				}
			}
			err := a.checkTrust()
			if (err == nil) != (tc.want == nil) {
				t.Errorf("checkTrust: %v", err)
			}
		})
	}
	// Not under fallible.
	c := cfg(nil, nil)
	c.Trust = config.TrustFallible
	if p := (&App{Cfg: c}).claudeHostsProblem(); p != "" {
		t.Errorf("fallible: %s", p)
	}
}

func TestClaudeHostsAreExactNames(t *testing.T) {
	for _, h := range claudeHosts {
		if strings.ContainsAny(h, "*/:, ") || strings.Count(h, ".") < 1 {
			t.Errorf("%q is not an exact host name", h)
		}
	}
}

func TestDoctorTrustRows(t *testing.T) {
	run := func(a *App) string {
		c := &checkup{}
		a.doctorTrust(c)
		var s strings.Builder
		for _, r := range c.rows {
			s.WriteString(r.label + ": " + r.text + "\n")
		}
		return s.String()
	}
	a := &App{Cfg: &config.Config{Isolation: isolationContainer, Trust: config.TrustHostile}}
	if got := run(a); !strings.HasPrefix(got, "trust: hostile, and needs a vm isolation profile") {
		t.Errorf("container: %s", got)
	}
	a = &App{Cfg: &config.Config{Isolation: isolationVM, Trust: config.TrustHostile, HostExec: true, EgressAllow: []string{"claude.ai"}}}
	got := run(a)
	for _, w := range []string{"trust: hostile", "host exec: host_exec = true is ignored", "claude hosts: trust = hostile: egress_allow does not name api.anthropic.com", "ssh agent: "} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	a.Cfg.EgressAllow = claudeHosts
	if got := run(a); !strings.Contains(got, "claude hosts: egress_allow names api.anthropic.com") {
		t.Errorf("allowed: %s", got)
	}
	a.Cfg.Trust = config.TrustFallible
	if got := run(a); got != "trust: fallible\n" {
		t.Errorf("fallible: %q", got)
	}
}

// Doctor's SSH agent row under hostile says what agentFrom does: no agent
// with ssh_agent unset, the one it names otherwise.
func TestSSHAgentTrustNote(t *testing.T) {
	a := &App{Cfg: &config.Config{Isolation: isolationVM, Trust: config.TrustHostile, Getenv: func(string) string { return "/tmp/agent.sock" }}}
	sock, from := a.agentFrom("", "", a.Cfg.Trust)
	if sock != "" || from != agentFromHostile {
		t.Fatalf("agentFrom = %q, %q", sock, from)
	}
	if got := a.sshAgentTrustNote(); !strings.Contains(got, "the VM gets none") || strings.Contains(got, "unrestricted") {
		t.Errorf("unset: %q", got)
	}
	a.Cfg.SSHAgent = "/tmp/other.sock"
	if got := a.sshAgentTrustNote(); !strings.Contains(got, "ssh_agent names an agent") || !strings.Contains(got, "unrestricted") {
		t.Errorf("set: %q", got)
	}
	a.Cfg.SSHAgent = "none"
	if got := a.sshAgentTrustNote(); !strings.Contains(got, "no SSH agent") {
		t.Errorf("none: %q", got)
	}
}

func TestFactsHostExecEffective(t *testing.T) {
	b := newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.Cfg.HostExec, b.Cfg.Trust = true, config.TrustFallible
	if !b.instructionFacts().HostExec {
		t.Error("fallible with host_exec: not on")
	}
	b.Cfg.Trust = config.TrustHostile
	if b.instructionFacts().HostExec {
		t.Error("hostile: host exec on in the instructions")
	}
}
