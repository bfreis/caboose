package launcher

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// The outbound proxy, as the sandbox is created with it: under vm with
// egress on, every process in the VM has HTTP(S)_PROXY naming the
// agent's proxy (agentproto.EgressEnv) -- the entrypoint and its first
// install of Claude Code, the VM's dockerd and its pulls, every exec and
// so every session -- and the agent points ssh there at boot
// (BootSpec.Egress). The containers the VM's dockerd runs get none of it:
// they stay on its NAT. The environment is fixed at creation, so the VM
// is labelled with it (assets.LabelEgress) and a change says caboose
// restart; the link (link.go) serves it whatever the label, from
// config.toml.

// egressOn reports whether the sandbox's outbound connections go through
// this machine: under vm, with the profile's egress on.
func (a *App) egressOn() (bool, error) {
	return a.isVM() && a.Cfg.Egress, nil
}

// egressLabel is assets.LabelEgress's value.
func egressLabel(on bool) string {
	if on {
		return "on"
	}
	return ""
}

// withEgress sets the outbound proxy in spec when on, and labels it
// either way.
func withEgress(spec *backend.Spec, on bool) {
	if on {
		spec.Env = append(spec.Env, agentproto.EgressEnv(agentproto.EgressListen)...)
		spec.Egress = agentproto.EgressListen
	}
	spec.Labels = append(spec.Labels, assets.LabelEgress+"="+egressLabel(on))
}

// createdEgress is whether the sandbox was created with the outbound
// proxy; ok is false when that cannot be told, or the sandbox records
// none.
func (a *App) createdEgress() (on, ok bool) {
	labels, err := a.box().Labels()
	if err != nil {
		return false, false
	}
	v, ok := labels[assets.LabelEgress]
	return v == "on", ok
}

// egressDrift says how the VM's outbound proxy differs from the
// configuration's, or "" when it does not, it cannot be told, or the
// sandbox is no VM.
func (a *App) egressDrift() string {
	want, err := a.egressOn()
	if err != nil || !a.isVM() {
		return ""
	}
	if iso, _, ok := a.createdIsolation(); !ok || iso != isolationVM {
		return "" // the isolation's drift says it
	}
	if d := a.missingLabel(assets.LabelEgress, "outbound proxy setting"); d != "" {
		return d
	}
	have, ok := a.createdEgress()
	if !ok || have == want {
		return ""
	}
	return fmt.Sprintf("the VM was created with egress %s; the configuration says %s", onOff(have), onOff(want))
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// warnIfEgressDrifted is warnIfRunArgsDrifted for the outbound proxy.
// Turned off, the VM's programs still name the proxy, which no longer
// serves: nothing reaches out until the restart.
func (a *App) warnIfEgressDrifted() {
	if d := a.egressDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("run 'caboose restart' to recreate it with it (this kills running sessions).")
	}
}

// egressSummary is status's and doctor's line on the outbound proxy,
// under vm.
func (a *App) egressSummary() string {
	on, err := a.egressOn()
	switch {
	case err != nil:
		return err.Error()
	case on:
		return fmt.Sprintf("through this machine, so its VPN routes and DNS apply (egress on; egress_ports %q)",
			or(a.Cfg.EgressPorts, config.DefaultEgressPorts))
	}
	return "the VM's own NAT, which reaches none of this machine's VPN routes (egress off)"
}

// doctorEgress is doctor's row on the outbound proxy, under vm.
func (a *App) doctorEgress(c *checkup) {
	c.ok("egress", "%s", a.egressSummary())
}

// proxyWait bounds how long a launch waits for the VM's outbound proxy
// (awaitProxy).
const proxyWait = 15 * time.Second

// linkStopPoll is how often awaitProxy looks for a stop of the link while
// it waits.
const linkStopPoll = 100 * time.Millisecond

// awaitProxy waits, up to proxyWait, until the VM's outbound proxy
// accepts connections, before a launch runs something there that may
// reach out: the proxy is the link's, which a VM's start only begins
// (ensureRunning), so a command run straight after it found nothing
// listening. Only under vm with egress on; a launch never fails for
// it, but says what a timeout means.
//
// A link that has stopped for good (link.stop) is not waited on: the
// proxy is not coming, and the stop says why and what to do. The helper
// the launch has just started may only get there during the wait, so the
// wait ends at the stop as well. No helper running at all is not this
// one's to fix: ensureRunning has just started one under vm, and one that
// fails to start runs into the timeout, which says where to look.
func (a *App) awaitProxy() {
	if on, err := a.egressOn(); err != nil || !on {
		return
	}
	if a.saidLinkStop() {
		return
	}
	secs := strconv.Itoa(int(proxyWait / time.Second))
	var errb bytes.Buffer
	cmd := a.box().Command(backend.ExecSpec{Argv: []string{AgentPath, "wait-proxy", secs}})
	cmd.Stdout, cmd.Stderr = io.Discard, &limitedWriter{w: &errb, n: 4096}
	// A killed exec's leftovers do not hold the launch on its pipes.
	cmd.WaitDelay = time.Second
	err := cmd.Start()
	if err == nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t := time.NewTicker(linkStopPoll)
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case <-t.C:
				if _, stopped := readLinkStop(a.Cfg.DataDir); stopped {
					_ = cmd.Process.Kill()
					<-done
					t.Stop()
					a.saidLinkStop()
					return
				}
			}
		}
		t.Stop()
	}
	if err == nil {
		return
	}
	if a.saidLinkStop() {
		return
	}
	a.Note("the VM's outbound proxy is not up after %ss, so connections out of the sandbox fail until it is; it usually follows within seconds (if not, %s says why, and 'caboose link --restart' restarts it)",
		secs, filepath.Join(a.Cfg.DataDir, linkLogFile))
}

// saidLinkStop says, when the link has stopped for good, that the VM's
// outbound proxy is down and why, in the stop's own words, which say what
// to do; and reports whether it did.
func (a *App) saidLinkStop() bool {
	msg, stopped := readLinkStop(a.Cfg.DataDir)
	if stopped {
		a.Note("the VM's outbound proxy is down, since the link to it stopped: %s", proposal.Printable(strings.TrimSpace(msg)))
	}
	return stopped
}
