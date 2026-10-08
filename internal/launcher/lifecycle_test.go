package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/session"
	"github.com/bfreis/caboose/internal/tty"
)

// The sandbox's lifecycle, as each isolation's user reads it: stop,
// restart, version and status call it a VM under vm, a container
// otherwise, and never the other. Status's docker line is this machine's
// engine under docker and gvisor; under vm a dockerd in the VM is the
// sandbox's own, on a line of its own name.
func TestSandboxWords(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			label := fmt.Sprintf("%-9s :", b.noun())
			dockerd := true
			b.box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				if slices.Equal(s.Argv, []string{"test", "-S", "/var/run/docker.sock"}) && !dockerd {
					return backendtest.Fail()
				}
				return nil
			}
			other := map[bool]string{true: "container", false: "VM"}[iso == isolationVM]
			says := func(what, out, errs string) {
				t.Helper()
				if s := out + errs; strings.Contains(s, other) {
					t.Errorf("%s says %s:\n%s", what, other, s)
				}
			}

			if err := b.Version(); err != nil {
				t.Fatal(err)
			}
			out, errs := b.said()
			if !strings.Contains(out, "\n"+label+" box (running, on the local image)\n") {
				t.Errorf("version:\n%s", out)
			}
			says("version", out, errs)

			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			want := []string{"\n" + label + " box (running)\n", "\nisolation : " + iso + " (the default: config.toml defines no profile)\n"}
			if iso == isolationVM {
				want = append(want, "\ndockerd   : in the VM, its own, not this machine's (images kept in ")
				if strings.Contains(out, "\ndocker    :") {
					t.Errorf("status has a docker line under vm:\n%s", out)
				}
			} else {
				want = append(want, "\ndocker    : socket MOUNTED - root-equivalent access to this host\n")
			}
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("status lacks %q:\n%s", w, out)
				}
			}
			says("status", out, errs)
			dockerd = false
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			w := "\ndocker    : cli only, no socket (isolated)\n"
			if iso == isolationVM {
				w = "\ndockerd   : none in the VM ("
			}
			if !strings.Contains(out, w) {
				t.Errorf("status without a socket lacks %q:\n%s", w, out)
			}
			says("status without a socket", out, errs)

			if err := b.Stop(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			if errs != "caboose: "+b.noun()+" stopped\n" {
				t.Errorf("stop: %q", errs)
			}
			says("stop", out, errs)
			if err := b.Stop(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			if errs != "caboose: "+b.noun()+" is not running\n" {
				t.Errorf("stop again: %q", errs)
			}
			says("stop again", out, errs)
			if !slices.Equal(b.box.Calls, []string{"stop"}) {
				t.Errorf("calls: %q", b.box.Calls)
			}

			// What restart says as it removes the old one.
			b.box.Status = "running"
			if err := b.removeContainer(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			if errs != "caboose: "+b.noun()+" removed\n" {
				t.Errorf("restart's removal: %q", errs)
			}
			says("restart's removal", out, errs)
			if err := b.Version(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			if !strings.Contains(out, "\n"+label+" box (absent)\n") {
				t.Errorf("version with none:\n%s", out)
			}
			says("version with none", out, errs)

			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			out, errs = b.said()
			if !strings.Contains(out, "\n"+label+" box (absent)\n") || !strings.HasSuffix(out, "\nnot running — start it by running caboose in a repo.\n") {
				t.Errorf("status with none:\n%s", out)
			}
			says("status with none", out, errs)
		})
	}
}

// caboose restart removes the sandbox and creates it anew from the
// current image, under the configured isolation, and says so in its
// words. Under vm what brings the sandbox up starts the link, which
// carries the SSH agent there.
func TestRestartRecreates(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.ImageID = "sha256:older"
			b := newBoxApp(t, iso, box)
			if err := b.Restart(); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			_, errs := b.said()
			if !slices.Equal(box.Calls, []string{"remove", "create"}) {
				t.Errorf("calls: %q", box.Calls)
			}
			for _, w := range []string{"caboose: " + b.noun() + " removed\n", "caboose: " + b.noun() + " recreated\n"} {
				if !strings.Contains(errs, w) {
					t.Errorf("stderr lacks %q:\n%s", w, errs)
				}
			}
			if box.Image() != boxImageID {
				t.Errorf("recreated from %q", box.Image())
			}
			spec, _ := box.Spec()
			labels, _ := box.Labels()
			if spec.Image != "img" || labels[assets.LabelIsolation] != iso || labels[assets.LabelPlatform] != "linux-arm64" {
				t.Errorf("created from %q, labelled %v", spec.Image, labels)
			}
			if !box.Ran("test", "-f", ReadyMarker) {
				t.Error("not waited for")
			}
			_, probed := os.Stat(b.engine + "/probe")
			link := slices.ContainsFunc(b.spawned, func(a []string) bool { return slices.Contains(a, "link") })
			switch iso {
			case isolationContainer:
				if spec.Runtime != "" || spec.User != "" || probed == nil || link {
					t.Errorf("docker: runtime %q, user %q, probed %v, link %v", spec.Runtime, spec.User, probed == nil, link)
				}
			case isolationGVisor:
				if spec.Runtime != "runsc" || spec.User != "" || probed != nil || link {
					t.Errorf("gvisor: runtime %q, user %q, probed %v, link %v", spec.Runtime, spec.User, probed == nil, link)
				}
			case isolationVM:
				if spec.Runtime != "" || spec.User != rootUser || !link || !strings.Contains(errs, "caboose: starting the sandbox's VM\n") ||
					!slices.Contains(spec.Volumes, backend.Volume{Name: dockerVolume, Target: "/var/lib/docker"}) {
					t.Errorf("vm: runtime %q, user %q, link %v, volumes %v\n%s", spec.Runtime, spec.User, link, spec.Volumes, errs)
				}
			}
		})
	}
}

// Restart and stop end the sessions a sandbox holds only when told to:
// CABOOSE_FORCE=1, or a yes on the terminal. With neither they refuse, and leave
// the sandbox as it was.
func TestSessionLossNeedsConsent(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				if len(s.Argv) > 1 && s.Argv[0] == "tmux" && s.Argv[1] == "list-sessions" {
					return backendtest.Reply("proj-abc123\nproj-abc123-2\n", "", 0)
				}
				return nil
			}
			if !tty.IsTerminal(os.Stdin.Fd()) {
				for what, run := range map[string]func() error{"stop": b.Stop, "restart": b.Restart} {
					err := run()
					if err == nil || !strings.Contains(err.Error(), "refusing to "+what+" non-interactively with live sessions (set CABOOSE_FORCE=1 to override)") {
						t.Errorf("%s: %v", what, err)
					}
					if _, errs := b.said(); !strings.Contains(errs, "caboose: "+what+" will kill these live session(s):\n  proj-abc123\n  proj-abc123-2\n") {
						t.Errorf("%s said:\n%s", what, errs)
					}
				}
				if len(b.box.Calls) != 0 || b.box.State() != "running" {
					t.Errorf("refused, yet: %q", b.box.Calls)
				}
			}
			b.Cfg.Getenv = func(k string) string { return map[string]string{"CABOOSE_FORCE": "1"}[k] }
			if err := b.Stop(); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); !strings.Contains(errs, "caboose: CABOOSE_FORCE=1 set, continuing.\n") || b.box.State() != "exited" {
				t.Errorf("forced stop: %s\n%s", b.box.State(), errs)
			}
		})
	}
}

// A launch starts a stopped sandbox rather than recreating it; one that
// will not start says why, and what to do, in the isolation's words.
func TestLaunchStartsAStoppedSandbox(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.Status = "exited"
			b := newBoxApp(t, iso, box)
			if err := b.ensureRunning(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			if !slices.Equal(box.Calls, []string{"start"}) || box.State() != "running" {
				t.Errorf("calls %q, %s", box.Calls, box.State())
			}
			if link := len(b.spawned) > 0; link != (iso == isolationVM) {
				t.Errorf("link started: %v (%q)", link, b.spawned)
			}

			box.Status, box.Calls = "exited", nil
			box.StartErr = errors.New("the engine said no\nand more besides")
			err := b.ensureRunning(false)
			want := b.noun() + " box did not start: the engine said no\n" +
				"       'caboose restart' recreates it; it is stopped, so no sessions are lost."
			if err == nil || err.Error() != want {
				t.Errorf("err = %v\nwant %s", err, want)
			}
			if !slices.Equal(box.Calls, []string{"start"}) {
				t.Errorf("calls %q", box.Calls)
			}
		})
	}
}

// A stopped container created under runsc, which docker no longer has,
// can never start: it is recreated with the configured isolation, or,
// when that cannot work either, left alone with an error that says what
// to do. Doctor says so too. A VM has no runtime to lose.
func TestRuntimeGone(t *testing.T) {
	stopped := func(iso string) *backendtest.Fake {
		box := runningBox(iso)
		box.Status = "exited"
		return box
	}
	b := newBoxApp(t, isolationContainer, stopped(isolationGVisor))
	b.write(t, "runtimes", `{"runc":{}}`)
	if rt := b.runtimeGone(); rt != "runsc" {
		t.Fatalf("runtimeGone = %q", rt)
	}
	c := &checkup{}
	b.doctorContainerIsolation(c)
	if got := c.String(); !strings.Contains(got, "created under runsc, which docker no longer has") || c.count(levelProblem) != 1 {
		t.Errorf("doctor: %s", got)
	}
	// A launch recreates it, rather than start what cannot.
	if err := b.ensureRunning(false); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	if !slices.Equal(b.box.Calls, []string{"remove", "create"}) {
		t.Errorf("calls %q", b.box.Calls)
	}
	spec, _ := b.box.Spec()
	if spec.Runtime != "" || !slices.Contains(spec.Labels, assets.LabelIsolation+"=container") {
		t.Errorf("created with: %+v", spec)
	}
	if !strings.Contains(b.errb.String(), "no sessions are lost: recreating it with isolation container") {
		t.Errorf("said:\n%s", b.errb)
	}

	b = newBoxApp(t, isolationGVisor, stopped(isolationGVisor))
	b.write(t, "runtimes", `{"runc":{}}`)
	err := b.recreateForRuntime("runsc", false)
	if err == nil || !strings.Contains(err.Error(), "cannot start.\n") || !strings.Contains(err.Error(), "setup isolation' registers it") {
		t.Errorf("err = %v", err)
	}
	if len(b.box.Calls) != 0 {
		t.Errorf("with nothing to replace it: %q", b.box.Calls)
	}

	// A container of today's runtime is not gone, nor is a VM.
	for _, iso := range []string{isolationContainer, isolationVM} {
		b = newBoxApp(t, iso, stopped(iso))
		b.write(t, "runtimes", `{"runc":{}}`)
		if rt := b.runtimeGone(); rt != "" {
			t.Errorf("%s: runtimeGone = %q", iso, rt)
		}
	}
}

func TestIsolationDrift(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		config  string
		profile string
		drift   string
	}{
		{"no label, container", map[string]string{}, "container", "", "records no isolation"},
		{"no label, gvisor", map[string]string{}, "gvisor", "", "records no isolation"},
		{"same", map[string]string{assets.LabelIsolation: "gvisor", assets.LabelProfile: "gvisor"}, "gvisor", "", ""},
		{"back to container", map[string]string{assets.LabelIsolation: "gvisor", assets.LabelProfile: "gvisor.default"}, "", "", "created with isolation gvisor; the configuration says container"},
		{"a VM", map[string]string{assets.LabelIsolation: "vm", assets.LabelProfile: "vm.default"}, "vm", "vm.default", ""},
		{"same kind, other profile", map[string]string{assets.LabelIsolation: "vm", assets.LabelProfile: "vm.default"}, "vm", "vm.big",
			"created with profile vm.default; the configuration says vm.big"},
		{"profile added to the default", map[string]string{assets.LabelIsolation: "gvisor", assets.LabelProfile: "gvisor"}, "gvisor", "gvisor.default",
			"created with profile gvisor; the configuration says gvisor.default"},
		{"kind and profile differ", map[string]string{assets.LabelIsolation: "vm", assets.LabelProfile: "vm.big"}, "gvisor", "gvisor.a",
			"created with isolation vm; the configuration says gvisor"},
		{"no profile label", map[string]string{assets.LabelIsolation: "vm"}, "vm", "vm.default", "records no isolation profile"},
		{"none", nil, "gvisor", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{SandboxLabels: tc.labels}
			if tc.labels != nil {
				box.Status = "running"
			}
			a := &App{Cfg: &config.Config{Container: "box", Isolation: tc.config, Profile: tc.profile}, Backend: box}
			if got := a.isolationDrift(); !strings.Contains(got, tc.drift) || (tc.drift == "") != (got == "") {
				t.Errorf("drift = %q, want %q", got, tc.drift)
			}
		})
	}
}

// A launcher that meets a sandbox of another compat refuses to launch into
// it, and status says why, with what to do: caboose restart.
func TestCompatRefused(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.SandboxLabels[assets.LabelCompat] = strconv.Itoa(assets.Compat - 1)
			b := newBoxApp(t, iso, box)
			want := fmt.Sprintf("box was created by an older caboose, which this one cannot work with (compat %d, this caboose's %d)",
				assets.Compat-1, assets.Compat)
			err := b.ensureRunning(false)
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "run 'caboose restart' to recreate it") {
				t.Errorf("launch: %v", err)
			}
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); !strings.Contains(errs, want+"; run 'caboose restart'") {
				t.Errorf("status:\n%s", errs)
			}
			box.SandboxLabels[assets.LabelCompat] = strconv.Itoa(assets.Compat + 1)
			if err := b.ensureRunning(false); err == nil || !strings.Contains(err.Error(), "created by a newer caboose") {
				t.Errorf("newer: %v", err)
			}
			if len(box.Calls) != 0 {
				t.Errorf("calls %q", box.Calls)
			}
		})
	}
}

// A sandbox created from an image the tag no longer names runs on: a
// launch warns, and version says so, with the restart that moves it.
func TestCreatedFromAnOlderImage(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.ImageID = "sha256:older"
			b := newBoxApp(t, iso, box)
			if err := b.ensureRunning(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			if _, errs := b.said(); !strings.Contains(errs, "caboose: "+b.noun()+" is running an older image than 'img'.\n"+
				"caboose: run 'caboose restart' to pick it up (this kills running sessions).\n") {
				t.Errorf("launch:\n%s", errs)
			}
			if err := b.Version(); err != nil {
				t.Fatal(err)
			}
			out, errs := b.said()
			if !strings.Contains(out, fmt.Sprintf("\n%-9s : box (running, on an older image than the local one)\n", b.noun())) ||
				!strings.Contains(errs, "run 'caboose restart' to move it onto the local image") {
				t.Errorf("version:\n%s%s", out, errs)
			}
			if len(box.Calls) != 0 {
				t.Errorf("calls %q", box.Calls)
			}
		})
	}
}

// A sandbox that never becomes ready is given up on with its last log
// lines, and one that exits first is said to have. (A VM's agent says
// when it is ready instead: waitVMReady.)
func TestWaitUntilReadyFails(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationGVisor} {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.Log = "line 1\nthe entrypoint's last word\n"
			exits := false
			box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				if slices.Equal(s.Argv, []string{"test", "-f", ReadyMarker}) {
					if exits {
						box.Status = "exited"
					}
					return backendtest.Fail()
				}
				return nil
			}
			b := newBoxApp(t, iso, box)
			err := b.waitUntilReady()
			if err == nil || !strings.Contains(err.Error(), "giving up (raise ready_timeout in [session]") {
				t.Errorf("err = %v", err)
			}
			if _, errs := b.said(); errs != "caboose: not ready after 0s; last log lines:\nline 1\nthe entrypoint's last word\n" {
				t.Errorf("said %q", errs)
			}
			exits = true
			err = b.waitUntilReady()
			if err == nil || err.Error() != "startup failed" {
				t.Errorf("err = %v", err)
			}
			if _, errs := b.said(); !strings.HasPrefix(errs, "caboose: container exited before becoming ready; last log lines:\n") {
				t.Errorf("said %q", errs)
			}
			if !slices.Equal(box.Calls, []string{"logs 30", "logs 30"}) {
				t.Errorf("calls %q", box.Calls)
			}
		})
	}
}

// caboose detach detaches the clients of this project's sessions, and of
// no other's; with no sandbox running there is nothing to detach.
func TestDetach(t *testing.T) {
	cwd, err := config.Cwd()
	if err != nil {
		t.Fatal(err)
	}
	base := session.NameFor(cwd, "")
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				switch {
				case slices.Equal(s.Argv, []string{"tmux", "list-sessions", "-F", "#{session_name}"}):
					return backendtest.Reply(base+"\n"+base+"-2\n"+base+"-x\nother\n", "", 0)
				case slices.Equal(s.Argv, []string{"tmux", "list-clients", "-t", "=" + base + "-2"}),
					slices.Equal(s.Argv, []string{"tmux", "list-clients", "-t", "=other"}):
					return backendtest.Reply("/dev/pts/0\n", "", 0)
				}
				return nil
			}
			b := newBoxApp(t, iso, box)
			if err := b.Detach(); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); errs != "caboose: detached clients from '"+base+"-2' (session still running)\n" {
				t.Errorf("said %q", errs)
			}
			for _, s := range []string{base, base + "-x", "other"} {
				if box.Ran("tmux", "detach-client", "-s", "="+s) {
					t.Errorf("detached %s", s)
				}
			}
			b.Suffix = "x"
			if err := b.Detach(); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); errs != "caboose: no attached client on session '"+base+"-x'\n" {
				t.Errorf("said %q", errs)
			}
			box.Status = "exited"
			if err := b.Detach(); err != nil {
				t.Fatal(err)
			}
			if _, errs := b.said(); errs != "caboose: "+b.noun()+" is not running\n" {
				t.Errorf("said %q", errs)
			}
		})
	}
}

// caboose shell has a terminal in the sandbox only when it runs on one:
// docker exec -t refuses a stdin that is none, so `caboose shell -c CMD`
// from a script or a pipe ran nothing. Under every isolation, the exec
// asks for one as the launch has it.
func TestShellHasATerminalOnlyOnOne(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.mountLocal("local/linux-arm64")
			t.Chdir(b.tmp)
			var ran []*exec.Cmd
			b.replace = func(cmd *exec.Cmd) error { ran = append(ran, cmd); return nil }
			for _, terminal := range []bool{false, true} {
				b.terminal = func() bool { return terminal }
				b.box.Execs = nil
				if err := b.Shell([]string{"-c", "echo ok"}); err != nil {
					t.Fatalf("terminal %v: %v\n%s", terminal, err, b.errb)
				}
				i := slices.IndexFunc(b.box.Execs, func(s backend.ExecSpec) bool { return len(s.Argv) > 0 && s.Argv[0] == "bash" })
				if i < 0 {
					t.Fatalf("terminal %v: no bash in %+v", terminal, b.box.Execs)
				}
				s := b.box.Execs[i]
				if s.TTY != terminal || !s.Stdin || !slices.Equal(s.Argv, []string{"bash", "-c", "echo ok"}) || s.Dir != "/work" {
					t.Errorf("terminal %v: exec %+v", terminal, s)
				}
			}
			if len(ran) != 2 {
				t.Errorf("ran %d commands, want 2", len(ran))
			}
		})
	}
}

// A sandbox created without caboose's instructions mounted is told apart
// from one that has them, under every isolation: a launch and status
// warn, saying the restart that mounts them (doctor's problem is
// cmd/caboose's TestDoctorInstructionsUnmounted). A launch writes them all
// the same, and leaves ~/.claude/CLAUDE.md, the user's, alone.
func TestInstructionsUnmounted(t *testing.T) {
	for _, iso := range isolations {
		for _, has := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mounted=%v", iso, has), func(t *testing.T) {
				b := newBoxApp(t, iso, runningBox(iso))
				b.box.SandboxMounts = []backend.Mount{mount(b.tmp, "/work")}
				if has {
					b.box.SandboxMounts = append(b.box.SandboxMounts,
						mount(filepath.Join(b.data, datadir.ManagedDir), datadir.ManagedTarget))
				}
				mine := filepath.Join(b.data, datadir.ClaudeDir, "CLAUDE.md")
				if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				warning := "caboose: the " + b.noun() + " does not mount caboose's instructions at /etc/claude-code, so its sessions do not get them.\n" +
					"caboose: run 'caboose restart' to mount them (this kills running sessions).\n"
				if err := b.ensureRunning(false); err != nil {
					t.Fatal(err)
				}
				_, errs := b.said()
				if strings.Contains(errs, warning) == has {
					t.Errorf("launch, mounted %v:\n%s", has, errs)
				}
				if got, err := os.ReadFile(mine); err != nil || string(got) != "mine\n" {
					t.Errorf("~/.claude/CLAUDE.md is now %q, %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(b.data, datadir.ManagedInstructions)); err != nil {
					t.Errorf("instructions not written: %v", err)
				}
				if d := b.instructionsDrift(); (d == "") != has {
					t.Errorf("drift %q", d)
				}
				if err := b.Status(); err != nil {
					t.Fatal(err)
				}
				if _, errs := b.said(); strings.Contains(errs, warning) == has {
					t.Errorf("status, mounted %v:\n%s", has, errs)
				}
			})
		}
	}
}
