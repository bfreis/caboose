package launcher

import (
	"fmt"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// Trust, as the launch enforces it. config.toml's trust is "fallible" (the
// default) or "hostile". Hostile is only as strong as the isolation under
// it, so it needs a VM: the launch refuses to create the sandbox under any
// other, and refuses to attach to one made under another. Under a VM the
// VM has no network of its own (vmNetwork), so the only way out is the
// outbound proxy, whose every connection the host decides, and the proxy
// is on whatever egress says (hostileEgress). The trust and the network
// the sandbox was created with are labelled (assets.LabelTrust,
// assets.LabelNetwork), so a change says caboose restart.

// claudeHosts are the hosts Claude Code itself reaches, exact names. With
// the VM's NAT off and every host not named in egress_allow asked about, a
// hostile sandbox cannot run Claude Code unless egress_allow names them,
// so a hostile launch refuses until it does. They are never allowed
// implicitly: the user writes them in config.toml.
//
//   - api.anthropic.com: the API every request goes to.
//   - claude.ai: the installer (https://claude.ai/install.sh, which the
//     entrypoint downloads) and the login.
//   - platform.claude.com: the login's token exchange.
//   - downloads.claude.ai: the release binaries the installer and the
//     self-update fetch.
//
// The list is what starting and logging in need, and may be incomplete for
// optional features (telemetry, an MCP proxy): a host it lacks is added
// to egress_allow, or its request approved when it asks.
var claudeHosts = []string{
	"api.anthropic.com",
	"claude.ai",
	"platform.claude.com",
	"downloads.claude.ai",
}

// hostile reports whether config.toml says trust = "hostile".
func (a *App) hostile() bool { return a.Cfg.Trust == config.TrustHostile }

// trustLabel is assets.LabelTrust's value: the trust the sandbox is
// created under, "fallible" when config.toml says none.
func trustLabel(c *config.Config) string {
	if c.Trust == "" {
		return config.TrustFallible
	}
	return c.Trust
}

// Network settings of a VM.
const (
	networkNAT  = "nat"
	networkNone = "none"
)

// vmNetwork is the network a VM is created with: none under hostile, so
// that only the outbound proxy leaves it, else the NAT.
func (a *App) vmNetwork() string {
	if a.hostile() {
		return networkNone
	}
	return networkNAT
}

// networkLabel is assets.LabelNetwork's value: the VM's network, "" for a
// sandbox that is no VM.
func (a *App) networkLabel() string {
	if a.isVM() {
		return a.vmNetwork()
	}
	return ""
}

// hostileNeedsVM is the refusal of trust = "hostile" under iso, or "" when
// it holds. Used when the sandbox is created (what config.toml says) and
// when it is attached to (what it was created with, restart set).
func hostileNeedsVM(iso string, restart bool) string {
	if iso == isolationVM {
		return ""
	}
	if restart {
		return fmt.Sprintf(`trust = hostile needs a vm isolation profile: the sandbox was created with isolation %s; set isolation to a vm.* profile in config.toml (or trust = "fallible"), then run 'caboose restart' (this kills running sessions)`, proposal.Printable(iso))
	}
	return fmt.Sprintf(`trust = hostile needs a vm isolation profile: the isolation is %s; set isolation to a vm.* profile in config.toml (caboose setup isolation), or set trust = "fallible"`, proposal.Printable(iso))
}

// checkTrust is the launch's refusal of a configuration hostile cannot be
// enforced under: another isolation than vm, or an egress_allow that
// leaves Claude Code without its own hosts. Nothing under fallible.
func (a *App) checkTrust() error {
	if !a.hostile() {
		return nil
	}
	if m := hostileNeedsVM(isolationOf(a.Cfg), false); m != "" {
		return Die("%s", m)
	}
	if m := a.claudeHostsProblem(); m != "" {
		return Die("%s", m)
	}
	return nil
}

// checkTrustExisting is checkTrust for a sandbox that exists: it also
// refuses one that was not created as a hostile one needs, failing closed
// when a label is missing or unreadable: a vm isolation and a VM created
// with no network.
func (a *App) checkTrustExisting() error {
	if !a.hostile() {
		return nil
	}
	labels, err := a.box().Labels()
	if err != nil {
		return Die("trust = hostile needs a sandbox created as one: its labels cannot be read (%v), so it is not trusted; run 'caboose restart' (this kills running sessions)", err)
	}
	iso := labels[assets.LabelIsolation]
	if iso == "" {
		return Die("trust = hostile needs a vm isolation profile: the sandbox records no isolation; run 'caboose restart' (this kills running sessions)")
	}
	if m := hostileNeedsVM(iso, true); m != "" {
		return Die("%s", m)
	}
	if n := labels[assets.LabelNetwork]; n != networkNone {
		return Die("trust = hostile needs a VM created with no network: this one was created with network %s; run 'caboose restart' (this kills running sessions)", or(n, "none recorded"))
	}
	return a.checkTrust()
}

// claudeHostsMissing are the claudeHosts egress_allow does not name (by
// name or by a *.suffix pattern, as the proxy reads it), and those
// egress_deny names, which no egress_allow lifts. Only config.toml counts:
// a grant or an answer given at run time is not a standing rule.
func claudeHostsMissing(c *config.Config) (missing, denied []string, err error) {
	allow, err := policy.ParseAllow(strings.Join(c.EgressAllow, " "))
	if err != nil {
		return nil, nil, err
	}
	deny, err := policy.ParseAllowOf("egress_deny", strings.Join(c.EgressDeny, " "))
	if err != nil {
		return nil, nil, err
	}
	for _, h := range claudeHosts {
		switch {
		case deny.Name(h):
			denied = append(denied, h)
		case !allow.Name(h):
			missing = append(missing, h)
		}
	}
	return missing, denied, nil
}

// quoteList is entries as a TOML array.
func quoteList(entries []string) string {
	q := make([]string, len(entries))
	for i, e := range entries {
		q[i] = fmt.Sprintf("%q", e)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// claudeHostsProblem says, for a hostile configuration, which of
// claudeHosts it leaves out and the line that fixes it, or "" when every
// one is allowed.
func (a *App) claudeHostsProblem() string {
	if !a.hostile() || !a.isVM() {
		return ""
	}
	c := a.Cfg
	missing, denied, err := claudeHostsMissing(c)
	if err != nil {
		return fmt.Sprintf("trust = hostile: egress_allow or egress_deny cannot be read: %v", err)
	}
	var parts []string
	if ports, perr := config.ParsePortsOf("egress_ports", or(c.EgressPorts, config.DefaultEgressPorts)); perr != nil || !ports.Has(443) {
		parts = append(parts, fmt.Sprintf("trust = hostile: egress_ports %q does not allow port 443, which Claude Code's hosts use; add 443 to egress_ports in [%s] of config.toml", or(c.EgressPorts, config.DefaultEgressPorts), or(c.Profile, "vm.NAME")))
	}
	if len(missing) == 0 && len(denied) == 0 {
		return strings.Join(parts, "; ")
	}
	if len(missing) > 0 {
		profile := or(c.Profile, "vm.NAME")
		parts = append(parts, fmt.Sprintf("trust = hostile: egress_allow does not name %s, which Claude Code needs to start and log in, and a hostile sandbox's outbound connections are allowed only when egress_allow names them; in [%s] of config.toml set: egress_allow = %s",
			strings.Join(missing, ", "), profile, quoteList(append(append([]string(nil), c.EgressAllow...), missing...))))
	}
	if len(denied) > 0 {
		parts = append(parts, fmt.Sprintf("egress_deny names %s, which Claude Code needs: remove it from egress_deny", strings.Join(denied, ", ")))
	}
	return strings.Join(parts, "; ")
}

// trustDrift says how the sandbox's trust differs from the
// configuration's, or "" when it does not or cannot be told.
func (a *App) trustDrift() string {
	if d := a.missingLabel(assets.LabelTrust, "trust"); d != "" {
		return d
	}
	labels, err := a.box().Labels()
	if err != nil || labels[assets.LabelTrust] == trustLabel(a.Cfg) {
		return ""
	}
	return fmt.Sprintf("the %s was created with trust %s; the configuration says %s", a.noun(), labels[assets.LabelTrust], trustLabel(a.Cfg))
}

// networkDrift is trustDrift for the VM's network, which the trust sets.
func (a *App) networkDrift() string {
	if !a.isVM() {
		return ""
	}
	if iso, _, ok := a.createdIsolation(); !ok || iso != isolationVM {
		return "" // the isolation's drift says it
	}
	if d := a.missingLabel(assets.LabelNetwork, "network"); d != "" {
		return d
	}
	labels, err := a.box().Labels()
	if err != nil || labels[assets.LabelNetwork] == a.networkLabel() {
		return ""
	}
	return fmt.Sprintf("the %s was created with network %s; the configuration now gives it %s", a.noun(), or(labels[assets.LabelNetwork], "none recorded"), a.networkLabel())
}

// warnIfTrustDrifted is warnIfRunArgsDrifted for the trust and the network.
func (a *App) warnIfTrustDrifted() {
	for _, d := range []string{a.trustDrift(), a.networkDrift()} {
		if d != "" {
			a.Note("%s.", d)
			a.Note("run 'caboose restart' to recreate it with it (this kills running sessions).")
		}
	}
}

// doctorTrust is doctor's rows on the trust level, the first after the
// environment's own.
func (a *App) doctorTrust(c *checkup) {
	if !a.hostile() {
		c.ok("trust", "%s", trustLabel(a.Cfg))
		return
	}
	if m := hostileNeedsVM(isolationOf(a.Cfg), false); m != "" {
		c.problem("trust", `set isolation to a vm.* profile, or trust = "fallible"`, "hostile, and %s", strings.TrimPrefix(m, "trust = hostile "))
		return
	}
	c.ok("trust", "hostile: the VM has no network of its own, so only the outbound proxy leaves it, and every host egress_allow does not name asks")
	if a.Cfg.HostExec {
		c.note("host exec", "host_exec = true is ignored under trust = hostile: the link does not offer it")
	}
	if m := a.claudeHostsProblem(); m != "" {
		c.problem("claude hosts", "add the hosts to egress_allow", "%s", m)
	} else {
		c.ok("claude hosts", "egress_allow names %s", strings.Join(claudeHosts, ", "))
	}
	c.note("ssh agent", "%s", a.sshAgentTrustNote())
}

// sshAgentTrustNote says what a hostile VM gets of the SSH agent: what a
// fallible one does.
func (a *App) sshAgentTrustNote() string {
	if a.Cfg.SSHAgent == "none" {
		return "ssh_agent = none: the VM gets no SSH agent"
	}
	return "trust does not change the SSH agent: the link carries it into the VM (ssh_agent's, else the one ssh here would use), unrestricted, so the sandbox can sign and log in wherever its keys reach (set ssh_agent = \"none\" for none)"
}
