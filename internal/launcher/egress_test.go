package launcher

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/vm"
)

// Under vm the sandbox is created with the outbound proxy in every
// process's environment, the agent told to point ssh at it, and a label
// saying so; with egress_proxy off, and under docker and gvisor, with none
// of it. Under vm the link starts before the wait for the ready file, for
// the entrypoint's own downloads.
func TestCreateWithEgress(t *testing.T) {
	for _, tc := range []struct {
		name, iso, proxy string
		on               bool
	}{
		{"docker", isolationDocker, "", false},
		{"gvisor", isolationGVisor, "", false},
		{"vm", isolationVM, "", true},
		{"vm, on", isolationVM, "on", true},
		{"vm, off", isolationVM, "off", false},
		{"docker, on", isolationDocker, "on", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := runningBox(tc.iso)
			b := newBoxApp(t, tc.iso, box)
			b.Cfg.EgressProxy = tc.proxy
			linkedAt := -1
			b.App.Spawn = func(exe string, args ...string) error {
				if slices.Contains(args, "link") && linkedAt < 0 {
					linkedAt = len(box.Execs)
				}
				return nil
			}
			if err := b.Restart(); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			spec, _ := box.Spec()
			labels, _ := box.Labels()
			proxyEnv := agentproto.EgressEnv(agentproto.EgressListen)
			for _, e := range proxyEnv {
				if slices.Contains(spec.Env, e) != tc.on {
					t.Errorf("env has %s: %v", e, !tc.on)
				}
			}
			if want := map[bool]string{true: agentproto.EgressListen}[tc.on]; spec.Egress != want {
				t.Errorf("spec.Egress %q, want %q", spec.Egress, want)
			}
			if got, ok := labels[assets.LabelEgress]; !ok || got != egressLabel(tc.on) {
				t.Errorf("label %q (set %v)", got, ok)
			}
			if tc.iso == isolationVM {
				ready := slices.IndexFunc(box.Execs, func(e backend.ExecSpec) bool { return slices.Equal(e.Argv, []string{"test", "-f", ReadyMarker}) })
				if linkedAt < 0 || ready >= 0 && linkedAt > ready {
					t.Errorf("link started at exec %d, the wait for ready at %d", linkedAt, ready)
				}
			}
		})
	}
}

// A bad egress_proxy is said before anything is created, under vm.
func TestCreateRefusesBadEgress(t *testing.T) {
	b := newBoxApp(t, isolationVM, &backendtest.Fake{Status: "absent"})
	b.Cfg.EgressProxy = "maybe"
	if err := b.createContainer(false); err == nil || !strings.Contains(err.Error(), "egress_proxy") {
		t.Errorf("err = %v", err)
	}
	if _, ok := b.box.Spec(); ok {
		t.Error("created")
	}
}

func TestEgressDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		iso    string
		proxy  string
		drift  string
	}{
		{"same, on", map[string]string{assets.LabelIsolation: "vm", assets.LabelEgress: "on"}, "vm", "on", ""},
		{"same, off", map[string]string{assets.LabelIsolation: "vm", assets.LabelEgress: ""}, "vm", "off", ""},
		{"turned off", map[string]string{assets.LabelIsolation: "vm", assets.LabelEgress: "on"}, "vm", "off",
			"created with egress_proxy on; the configuration says off"},
		{"turned on", map[string]string{assets.LabelIsolation: "vm"}, "vm", "", "created with egress_proxy off; the configuration says on"},
		{"a container", map[string]string{assets.LabelIsolation: "docker"}, "docker", "on", ""},
		{"was a container", map[string]string{assets.LabelIsolation: "docker"}, "vm", "on", ""},
		{"bad value", map[string]string{assets.LabelIsolation: "vm"}, "vm", "maybe", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{Status: "running", SandboxLabels: tc.labels}
			a := &App{Cfg: &config.Config{Container: "box", Isolation: tc.iso, EgressProxy: tc.proxy}, Backend: box}
			if got := a.egressDrift(); !strings.Contains(got, tc.drift) || (tc.drift == "") != (got == "") {
				t.Errorf("drift = %q, want %q", got, tc.drift)
			}
		})
	}
}

// The builder's link serves the outbound proxy alone, for as long as the
// build holds it: its hello offers the proxy and no SSH agent, and closing
// it ends the session.
func TestBuilderLink(t *testing.T) {
	// Short: a Unix socket's path has a limit.
	dir, err := os.MkdirTemp("", "bl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := vm.Dir(dir)
	ln, err := net.Listen("unix", d.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hello := make(chan agentproto.Message, 1)
	ended := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if port, err := hvsock.Accept(c, time.Second); err != nil || port != agentproto.PortLink || hvsock.Ready(c, 1) != nil {
			t.Errorf("port %d, %v", port, err)
			return
		}
		s := agentproto.NewSession(c, c, false)
		m, _ := agentproto.Decode(<-s.Control())
		hello <- m
		_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
		<-s.Done()
		close(ended)
	}()
	var stderr bytes.Buffer
	a := &App{Cfg: &config.Config{}, Stderr: &stderr}
	lc, err := settingsOf(&config.Config{ForwardPorts: "3000", OpenURLs: "ask"}).check()
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.builderLink(lc.egress)(d)
	if err != nil {
		t.Fatal(err)
	}
	m := <-hello
	if m.Egress != agentproto.EgressListen || m.SSHAgent != "" {
		t.Errorf("hello offered egress %q, ssh agent %q", m.Egress, m.SSHAgent)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived the close")
	}
	if strings.Contains(stderr.String(), "ended") {
		t.Errorf("a close was said as an ending: %s", stderr.String())
	}
}

// The builder link does nothing for the guest but its proxy.
func TestBuilderActions(t *testing.T) {
	var acts hostlink.Actions = builderActions{}
	if acts.OpenURL("https://example.com") == nil || acts.Notify("t", "x") == nil {
		t.Error("the builder link acted")
	}
	if ok, err := acts.Confirm("?"); ok || err == nil {
		t.Error("the builder link confirmed")
	}
}

// A launch that runs something in the sandbox waits for the VM's outbound
// proxy once the sandbox is ready, and before it runs anything there:
// under vm with egress_proxy on, never otherwise. A proxy still down at
// the bound is said, with what to do, and the launch goes on; an agent
// from before wait-proxy answers with its usage, which is no wait and
// nothing to say. A link that stops for good -- before the wait, or
// during it -- ends the wait, and the launch says the stop's own words.
func TestLaunchWaitsForTheProxy(t *testing.T) {
	waitProxy := []string{AgentPath, "wait-proxy", "15"}
	const stopMsg = "the sandbox's caboose-agent speaks another protocol version: 1, this caboose 2 ('caboose restart' rebuilds the image)"
	for _, tc := range []struct {
		name, iso, proxy string
		reply            func() *exec.Cmd
		// stop is when the link stops for good: "before" the launch (a
		// helper that stopped and is not started again), "spawn" (the
		// helper the launch starts stops at once), "during" the wait.
		stop  string
		waits bool
		said  string
	}{
		{name: "vm", iso: isolationVM, waits: true},
		{name: "vm, proxy down", iso: isolationVM, reply: func() *exec.Cmd {
			return backendtest.Reply("", "caboose-agent: no outbound proxy (waited 15s)\n", 1)
		}, waits: true, said: "outbound proxy is not up after 15s"},
		{name: "vm, older agent", iso: isolationVM, reply: func() *exec.Cmd {
			return backendtest.Reply("", "usage: caboose-agent COMMAND\n", 2)
		}, waits: true},
		// The launch starts a helper, which forgets the last one's stop.
		{name: "vm, an earlier link stopped", iso: isolationVM, stop: "before", waits: true},
		{name: "vm, link stopped", iso: isolationVM, stop: "spawn", said: stopMsg},
		{name: "vm, link stops during the wait", iso: isolationVM, stop: "during", reply: func() *exec.Cmd {
			return exec.Command("sh", "-c", "exec sleep 20")
		}, waits: true, said: stopMsg},
		{name: "vm, proxy off", iso: isolationVM, proxy: "off", stop: "before"},
		{name: "docker", iso: isolationDocker, stop: "before"},
		{name: "gvisor", iso: isolationGVisor, stop: "before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := runningBox(tc.iso)
			if tc.proxy == "off" {
				box.SandboxLabels[assets.LabelEgress] = ""
			}
			b := newBoxApp(t, tc.iso, box)
			stop := func() {
				if err := writeLinkStop(b.data, stopMsg); err != nil {
					t.Error(err)
				}
			}
			box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				if !slices.Equal(s.Argv, waitProxy) {
					return nil
				}
				if tc.stop == "during" {
					time.AfterFunc(300*time.Millisecond, stop)
				}
				if tc.reply != nil {
					return tc.reply()
				}
				return nil
			}
			b.Cfg.EgressProxy = tc.proxy
			b.mountLocal("local/linux-arm64")
			if err := os.MkdirAll(b.data, 0o700); err != nil {
				t.Fatal(err)
			}
			switch tc.stop {
			case "before":
				stop()
			case "spawn":
				b.Spawn = func(string, ...string) error { stop(); return nil }
			}
			t.Chdir(b.tmp)
			b.replace = func(*exec.Cmd) error { return nil }
			b.terminal = func() bool { return false }
			start := time.Now()
			if err := b.Shell([]string{"-c", "true"}); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the launch took %v", took)
			}
			index := func(prefix ...string) int {
				return slices.IndexFunc(box.Execs, func(s backend.ExecSpec) bool {
					return len(s.Argv) >= len(prefix) && slices.Equal(s.Argv[:len(prefix)], prefix)
				})
			}
			ready, wait, bash := index("test", "-f", ReadyMarker), index(waitProxy...), index("bash")
			if bash < 0 {
				t.Fatalf("no bash in %+v", box.Execs)
			}
			switch {
			case !tc.waits && wait >= 0:
				t.Errorf("waited for the proxy: %+v", box.Execs)
			case tc.waits && (ready < 0 || wait < 0 || wait < ready || wait > bash):
				t.Errorf("wait-proxy at %d, ready at %d, bash at %d: %+v", wait, ready, bash, box.Execs)
			}
			_, errs := b.said()
			switch {
			case tc.said == "":
				if strings.Contains(errs, "outbound proxy") {
					t.Errorf("said %q", errs)
				}
			case tc.said == stopMsg:
				if strings.Count(errs, "outbound proxy") != 1 || !strings.Contains(errs, "outbound proxy is down, since the link to it stopped: "+stopMsg) ||
					strings.Contains(errs, "caboose link --restart") {
					t.Errorf("said %q", errs)
				}
			default:
				if !strings.Contains(errs, tc.said) || !strings.Contains(errs, "link.log") || !strings.Contains(errs, "caboose link --restart") {
					t.Errorf("said %q", errs)
				}
			}
		})
	}
}
