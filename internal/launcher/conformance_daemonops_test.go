package launcher

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/saferm"
	"github.com/bfreis/caboose/internal/uiport"
)

// confDaemonRig is newConfOps with the suite's daemon started (pid 4242,
// as newConfOps' start seam says), its daemon.json written, and every
// fact of this machine pinned: ps, the listeners, the addresses, the web
// page and the host sockets are the rig's, never this machine's.
type confDaemonRig struct {
	*confOps
	dir       string
	ps        map[string]string // "lstart"/"rss" -> output; absent fails
	listening []int
	listenErr error
	addrs     []netip.Addr
	gets      []string
}

const confRigPID = 4242

func newConfDaemonRig(t *testing.T, iso string) *confDaemonRig {
	t.Helper()
	o := newConfOps(t, iso, config.TrustFallible)
	r := &confDaemonRig{confOps: o, ps: map[string]string{"lstart": "Sat Oct 17 10:00:00 2026", "rss": "2048"}}
	if err := o.h.a.Spawn("/x/caboose", "daemon"); err != nil {
		t.Fatal(err)
	}
	r.dir = o.h.a.daemonDir()
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.writeState(t, confRigPID)
	o.daemon.Stat = &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: confRigPID, Dir: r.dir,
		Web: &daemonproto.WebState{Listening: true, Addr: uiport.Addr(false).String()}}}
	r.listening = []int{confRigPID}
	r.addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.168.64.1")}
	o.h.facts = confHostFacts{
		ps: func(args ...string) (string, error) {
			if len(args) != 4 || args[0] != "-o" || args[2] != "-p" || args[3] != strconv.Itoa(confRigPID) {
				return "", errors.New("no such process")
			}
			out, ok := r.ps[strings.TrimSuffix(args[1], "=")]
			if !ok {
				return "", errors.New("no such process")
			}
			return out + "\n", nil
		},
		listeners: func(port int) ([]int, error) {
			if !uiport.Is(port) {
				t.Errorf("asked who listens on %d", port)
			}
			return r.listening, r.listenErr
		},
		addrs: func() ([]netip.Addr, error) { return r.addrs, nil },
		get: func(_ context.Context, url string) (int, error) {
			r.gets = append(r.gets, url)
			return 200, nil
		},
		sockets: func(*App) []backend.Mount { return nil },
	}
	return r
}

func (r *confDaemonRig) writeState(t *testing.T, pid int) {
	t.Helper()
	b := `{"pid":` + strconv.Itoa(pid) + `,"version":"dev","protocol":1,"started":"2026-10-17T10:00:00Z","dev":true,"dir":"` + r.dir + `"}`
	if err := os.WriteFile(filepath.Join(r.dir, daemon.StateFile), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readOnly fails the test if anything but status and the broker's
// listings was asked of the daemon: no answer, no web page state (whose
// result carries the token), and never a start.
func (r *confDaemonRig) readOnly(t *testing.T) {
	t.Helper()
	if len(r.daemon.br.answers) != 0 || len(r.daemon.webCalls) != 0 || len(r.daemon.br.revoked) != 0 {
		t.Errorf("the daemon was asked to answer %v, for its page %v, to revoke %v", r.daemon.br.answers, r.daemon.webCalls, r.daemon.br.revoked)
	}
	if slices.Contains(r.daemon.dials, true) {
		t.Errorf("a dial would start a daemon: %v", r.daemon.dials)
	}
}

func TestConfDaemonInfo(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	info, err := r.h.Daemon()
	if err != nil {
		t.Fatal(err)
	}
	want := conformance.DaemonInfo{Running: true, PID: confRigPID, Started: "Sat Oct 17 10:00:00 2026", Dir: r.dir,
		WebAddr: "127.0.0.1:7171", WebOwned: true}
	if info != want {
		t.Fatalf("Daemon = %+v\nwant %+v", info, want)
	}
	if !confInside(info.Dir, r.g.CabooseHome()) {
		t.Errorf("the daemon's directory %s is not under B/home", info.Dir)
	}
	if err := r.h.HostHello(context.Background()); err != nil {
		t.Fatalf("HostHello: %v", err)
	}
	// The web page is the daemon's only while every listener on its port
	// is the daemon, on the loopback and a UI port.
	for name, mod := range map[string]func(){
		"another listener too": func() { r.listening = []int{confRigPID, 99} },
		"another's listener":   func() { r.listening = []int{99} },
		"no listener":          func() { r.listening = nil },
		"lsof fails":           func() { r.listenErr = errors.New("no lsof") },
		"not loopback": func() {
			r.daemon.Stat.Daemon.Web.Addr = "192.168.64.1:7171"
		},
		"not a UI port": func() { r.daemon.Stat.Daemon.Web.Addr = "127.0.0.1:8080" },
		"not listening": func() { r.daemon.Stat.Daemon.Web.Listening = false },
	} {
		r.listening, r.listenErr = []int{confRigPID}, nil
		r.daemon.Stat.Daemon.Web = &daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}
		mod()
		info, err := r.h.Daemon()
		if err != nil || info.WebOwned || !info.Running {
			t.Errorf("%s: %+v, %v", name, info, err)
		}
		r.gets = nil
		if _, err := r.h.UIStatic(context.Background()); err == nil || len(r.gets) != 0 {
			t.Errorf("%s: the page was asked for (%v, %v)", name, r.gets, err)
		}
	}
	r.listening, r.listenErr = []int{confRigPID}, nil
	r.daemon.Stat.Daemon.Web = &daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}
	r.gets = nil
	if st, err := r.h.UIStatic(context.Background()); err != nil || st != 200 || !slices.Equal(r.gets, []string{"http://127.0.0.1:7171/"}) {
		t.Fatalf("UIStatic = %d, %v; asked %v", st, err, r.gets)
	}
	if rss, err := r.h.DaemonRSS(); err != nil || rss != 2048*1024 {
		t.Errorf("DaemonRSS = %d, %v", rss, err)
	}
	r.readOnly(t)
}

// A daemon that is not the suite's is never described as it: daemon.json
// naming another process, a daemon that answers as another, a process
// gone.
func TestConfDaemonNotTheSuites(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	r.writeState(t, 77)
	if _, err := r.h.Daemon(); err == nil {
		t.Error("a daemon.json naming another process was taken")
	}
	r.writeState(t, confRigPID)
	r.daemon.Stat.Daemon.PID = 77
	if _, err := r.h.Daemon(); err == nil {
		t.Error("a daemon answering as another process was taken")
	}
	if err := r.h.HostHello(context.Background()); err == nil {
		t.Error("HostHello took another daemon")
	}
	if _, err := r.h.Requests(); err == nil {
		t.Error("Requests asked another daemon")
	}
	r.daemon.Stat.Daemon.PID = confRigPID
	r.daemon.Stat.Daemon.Dir = filepath.Join(t.TempDir(), "daemon")
	if _, err := r.h.Grants(); err == nil {
		t.Error("Grants asked a daemon in another directory")
	}
	r.daemon.Stat.Daemon.Dir = r.dir
	delete(r.ps, "lstart")
	if info, err := r.h.Daemon(); err != nil || info.Running {
		t.Errorf("a daemon whose process is gone: %+v, %v", info, err)
	}
	if err := os.Remove(filepath.Join(r.dir, daemon.StateFile)); err != nil {
		t.Fatal(err)
	}
	if info, err := r.h.Daemon(); err != nil || info.Running || info.PID != 0 {
		t.Errorf("no daemon.json: %+v, %v", info, err)
	}
	r.readOnly(t)
}

// Only the throwaway CABOOSE_HOME's daemon is ever asked: an App whose
// daemon directory is anywhere else is refused before any dial.
func TestConfDaemonOnlyUnderB(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	r.h.a.Cfg.CabooseHome = t.TempDir()
	if _, err := r.h.Daemon(); err == nil {
		t.Error("Daemon read another CABOOSE_HOME's daemon")
	}
	if err := r.h.HostHello(context.Background()); err == nil {
		t.Error("HostHello asked another CABOOSE_HOME's daemon")
	}
	if _, err := r.h.Requests(); err == nil {
		t.Error("Requests asked another CABOOSE_HOME's daemon")
	}
	if _, err := r.h.Grants(); err == nil {
		t.Error("Grants asked another CABOOSE_HOME's daemon")
	}
	if len(r.daemon.dials) != 0 {
		t.Errorf("dialed %v", r.daemon.dials)
	}
}

// The broker's requests and grants, listed and never answered.
func TestConfDaemonBroker(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	r.daemon.br.List = &daemonproto.RequestsResult{Requests: []daemonproto.RequestView{
		{ID: 3, Kind: "alias", Key: "alias:conf-ask", State: "pending"},
		{ID: 4, Kind: "open_url", Key: "open_url:http://example.invalid", State: "denied"},
	}}
	r.daemon.br.Gs = &daemonproto.GrantsResult{Grants: []daemonproto.GrantView{{}, {}}}
	got, err := r.h.Requests()
	want := []conformance.RequestInfo{{ID: 3, State: "pending", Kind: "alias", Alias: "conf-ask"}, {ID: 4, State: "denied", Kind: "open_url"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Requests = %+v, %v", got, err)
	}
	if !slices.Equal(r.daemon.br.listed, []bool{true}) {
		t.Errorf("listings (all) %v", r.daemon.br.listed)
	}
	if n, err := r.h.Grants(); err != nil || n != 2 {
		t.Fatalf("Grants = %d, %v", n, err)
	}
	r.daemon.br.List.Truncated = true
	if _, err := r.h.Requests(); err == nil {
		t.Error("a cut listing was taken as whole")
	}
	r.daemon.br.Gs.Truncated = true
	if _, err := r.h.Grants(); err == nil {
		t.Error("a cut grants listing was counted as whole")
	}
	r.readOnly(t)
}

func TestConfHostAddrs(t *testing.T) {
	r := newConfDaemonRig(t, config.KindVM)
	r.addrs = []netip.Addr{
		netip.MustParseAddr("192.168.64.1"), netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"),
		netip.MustParseAddr("fe80::1%en0"), netip.MustParseAddr("fe80::2"), netip.MustParseAddr("10.0.0.5"),
		netip.MustParseAddr("::ffff:10.0.0.5"), netip.MustParseAddr("fd00::1"), netip.MustParseAddr("224.0.0.1"),
	}
	got, err := r.h.HostAddrs()
	if err != nil || len(got) != 3 || got[0].String() != "10.0.0.5" || got[1].String() != "192.168.64.1" || got[2].String() != "fd00::1" {
		t.Fatalf("HostAddrs = %v, %v", got, err)
	}
	if r.h.LinkSocket() == "" || !strings.HasPrefix(r.h.LinkSocket(), "/") {
		t.Errorf("LinkSocket = %q", r.h.LinkSocket())
	}
}

// Each reach to the web UI's port is decided on a copy of config.toml by
// the product's loader and decisions, and nothing is written.
func TestConfTryUI(t *testing.T) {
	for _, iso := range []string{config.KindContainer, config.KindVM} {
		t.Run(iso, func(t *testing.T) {
			r := newConfDaemonRig(t, iso)
			hash, _ := r.h.ConfigHash()
			if d, err := r.h.TryUI(conformance.UIVariant{ForwardPorts: "3000"}); err != nil || d.Stage != "" {
				t.Fatalf("forward_ports = 3000: %+v, %v", d, err)
			}
			for _, v := range []conformance.UIVariant{
				{ForwardPorts: "7171"}, {ForwardPorts: "7172"}, {ForwardPorts: "7000-7200"},
				{OpenURLsAllow: []string{uiport.Origin(false)}},
				{EgressAllow: []string{uiport.Addr(false).String()}},
				// Refused by the creation's own check of run_args.
				{RunArgs: []string{"--publish=" + uiport.Addr(false).String() + ":7171"}},
			} {
				d, err := r.h.TryUI(v)
				if err != nil || (d.Stage != conformance.UIAtLoad && d.Stage != conformance.UIAtDecide) || !strings.Contains(d.Message, "web UI") {
					t.Errorf("%+v: %+v, %v", v, d, err)
				}
			}
			// An egress reach to an address that is not this machine's
			// gets past the web UI's rule.
			if d, err := r.h.TryUI(conformance.UIVariant{EgressAllow: []string{"192.0.2.7:7171"}}); err != nil || strings.Contains(d.Message, "web UI") {
				t.Errorf("an egress reach to another machine: %+v, %v", d, err)
			}
			// Today only doctor reports the host's network; a creation that
			// refused it would do too.
			d, err := r.h.TryUI(conformance.UIVariant{RunArgs: []string{"--network=host"}})
			if err != nil || d.Stage == "" || !strings.Contains(d.Message, "network") {
				t.Errorf("run_args --network host: %+v, %v", d, err)
			}
			if _, err := r.h.TryUI(conformance.UIVariant{ForwardPorts: "3000", RunArgs: []string{"-e", "X=1"}}); err == nil {
				t.Error("two changes at once were tried")
			}
			if _, err := r.h.TryUI(conformance.UIVariant{EgressAllow: []string{"127.0.0.1"}}); err == nil {
				t.Error("an egress reach with no port was tried")
			}
			if now, _ := r.h.ConfigHash(); now != hash {
				t.Error("config.toml changed")
			}
			r.readOnly(t)
		})
	}
}

// The next launch's mounts are createContainer's plan, read and never
// realised: the roots by their writable parts, the kept home, the
// managed directory read-only.
func TestConfNextMounts(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	if _, err := r.h.NextMounts(); err == nil {
		t.Fatal("the plan was read with no sandbox to take the platform from")
	}
	r.h.box = &confBox{g: r.g, iso: config.KindContainer}
	r.h.box.setInspection(conformance.Inspection{Labels: map[string]string{assets.LabelPlatform: "linux-arm64"}})
	ms, err := r.h.NextMounts()
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]conformance.Mount{}
	for _, m := range ms {
		byTarget[m.Target] = m
	}
	ro, dev := r.g.RootNames()[0], r.g.RootNames()[1]
	for target, readOnly := range map[string]bool{
		"/work/" + ro: true, "/work/" + dev: true, "/work/" + dev + "/" + conformance.WritablePart: false,
		"/etc/claude-code": true, "/home/agent/.config/caboose": false, "/home/agent/.local/bin": false,
	} {
		m, ok := byTarget[target]
		if !ok || m.ReadOnly != readOnly {
			t.Errorf("%s: %+v (%v)", target, m, ok)
		}
	}
	if m := byTarget["/home/agent/.config/caboose"]; m.Source != filepath.Join(r.g.DataDir(), "home", ".config", "caboose") {
		t.Errorf("the kept config's source is %s", m.Source)
	}
	if len(r.box.Calls) != 0 {
		t.Errorf("the sandbox was touched: %v", r.box.Calls)
	}
	// A writable part reached through a link is never planned.
	part := filepath.Join(r.g.RootPath(dev), conformance.WritablePart)
	if err := os.Rename(part, part+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(part+".moved", part); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.NextMounts(); err == nil {
		t.Error("a writable part that is a link was planned")
	}
}

// The host sockets are createContainer's own: the engine's when
// engine_socket names a socket, and none under vm.
func TestConfHostSockets(t *testing.T) {
	saved := goos
	goos = "linux"
	t.Cleanup(func() { goos = saved })
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saferm.RemoveRoot(dir) })
	sock := filepath.Join(dir, "e.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, iso := range []string{config.KindContainer, config.KindVM} {
		r := newConfDaemonRig(t, iso)
		a := r.h.a
		a.Cfg.EngineSocket, a.Cfg.SSHAgent = true, "none"
		get := a.Cfg.Getenv
		a.Cfg.Getenv = func(k string) string {
			if k == "DOCKER_HOST" {
				return "unix://" + sock
			}
			return get(k)
		}
		ms := socketMounts(a)
		if iso == config.KindVM {
			if len(ms) != 0 {
				t.Errorf("a VM gets host sockets: %v", ms)
			}
			continue
		}
		if len(ms) != 1 || ms[0].Source != sock || ms[0].Target != "/var/run/docker.sock" {
			t.Fatalf("socket mounts %v", ms)
		}
		if s := a.hostSockets(); s.Engine != sock || s.NotSocket != "" {
			t.Errorf("hostSockets %+v", s)
		}
		a.Cfg.Getenv = func(k string) string {
			if k == "DOCKER_HOST" {
				return "unix://" + filepath.Join(dir, "none")
			}
			return get(k)
		}
		if s := a.hostSockets(); len(s.Mounts) != 0 || s.NotSocket == "" {
			t.Errorf("a path that is no socket: %+v", s)
		}
	}
}

// Doctor is the product's report, run in this process, its problems no
// error; nothing reaches an engine (every docker call fails).
func TestConfDoctor(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	r.h.a.Docker = &docker.CLI{Path: "false"}
	out, err := r.h.Doctor(context.Background())
	if err != nil || !strings.Contains(out, "caboose doctor") || !strings.Contains(out, r.g.Env()) {
		t.Fatalf("Doctor = %.300q, %v", out, err)
	}
	r.readOnly(t)
}

// open_urls is the running link's, as its link.json says; none without
// a link.
func TestConfOpenURLs(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	if _, err := r.h.OpenURLs(); err == nil {
		t.Fatal("open_urls was read with no link running")
	}
	if err := os.MkdirAll(r.g.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockLink(r.g.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := writeLinkState(r.g.DataDir(), linkState{linkSettings: linkSettings{OpenURLs: "allow"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.h.OpenURLs(); err != nil || got != "allow" {
		t.Fatalf("OpenURLs = %q, %v", got, err)
	}
}

// The daemon checks' canaries go where their layout says, with the
// suite's daemon's directory, and only one under the throwaway
// CABOOSE_HOME; none shares a role with another group's.
func TestConfDaemonCanaries(t *testing.T) {
	r := newConfDaemonRig(t, config.KindContainer)
	for _, d := range []string{filepath.Join(t.TempDir(), "daemon"), filepath.Join(r.g.CabooseHome(), "daemon-x"), filepath.Join(r.g.CabooseHome(), "daemon", "sub")} {
		if _, err := confDaemonCanaries(r.g, d); err == nil {
			t.Errorf("%s was taken as the daemon's directory", d)
		}
	}
	specs, err := confDaemonCanaries(r.g, r.dir)
	if err != nil {
		t.Fatal(err)
	}
	before, store, after, err := confLayout(r.g)
	if err != nil {
		t.Fatal(err)
	}
	others := map[string]bool{}
	for _, s := range slices.Concat(before, store, after) {
		others[s.Role] = true
	}
	parents := map[string]string{}
	for _, s := range specs {
		if others[s.Role] {
			t.Errorf("role %s is another group's too", s.Role)
		}
		parents[s.Role] = s.Parent
	}
	rel, _ := filepath.Rel(r.g.Base(), r.dir)
	if parents[checks.RoleDDaemon] != filepath.ToSlash(rel) || parents[checks.RoleDHome] != r.g.DataRel()+"/home/.config/caboose" ||
		parents[checks.RoleDCkpt] != r.g.StoreRel() || parents[checks.RoleDGrants] != r.g.EnvDirRel() {
		t.Fatalf("parents %v", parents)
	}
	for _, d := range confLayoutDirs(r.g) {
		if fi, err := os.Stat(filepath.Join(r.g.Base(), d)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not made: %v", d, err)
		}
	}
	if err := makeCanaries(r.h, specs); err != nil {
		t.Fatalf("the canaries cannot be made where the launch leaves things: %v", err)
	}
	home, err := r.h.Canary(checks.RoleDHome)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := conformance.HomePath(home); err != nil || p.String() != conformance.HomeCanaryDir+"/"+r.g.CanaryName(checks.RoleDHome) {
		t.Errorf("the home canary is seen at %v (%v)", p, err)
	}
}

// Under vm the next launch's mounts, planned into a VM as the vm backend
// plans them (its share's paths, its volumes as disks), read back by the
// audit as the running VM's are: the same set D6 compares, so a VM made
// from the plan is the plan.
func TestConfNextMountsVM(t *testing.T) {
	r := newConfDaemonRig(t, config.KindVM)
	r.h.box = &confBox{g: r.g, iso: config.KindVM}
	r.h.box.setInspection(conformance.Inspection{Labels: map[string]string{assets.LabelPlatform: "linux-arm64"}})
	next, err := r.h.NextMounts()
	if err != nil {
		t.Fatal(err)
	}
	var binds []backend.Mount
	var rec confVMRecord
	for _, m := range next {
		if m.Volume != "" {
			rec.State.Volumes = append(rec.State.Volumes, m.Volume)
			rec.State.Boot.Disks = append(rec.State.Boot.Disks, agentproto.GuestDisk{Device: "/dev/vdc", Target: m.Target})
			continue
		}
		binds = append(binds, backend.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if len(rec.State.Volumes) == 0 {
		t.Fatal("the plan has no volume: a VM is made with the guest's docker disk")
	}
	shares, mounts, err := backend.PlanGuest(binds)
	if err != nil {
		t.Fatal(err)
	}
	rec.Machine.Shares, rec.State.Boot.Mounts = shares, mounts
	in, err := rec.inspection()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := conformance.MountSet(in.Mounts), conformance.MountSet(next); !slices.Equal(got, want) {
		t.Fatalf("running %q, planned %q", got, want)
	}
}
