package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/uiport"
)

// The real Host's calls for the daemon, web UI, answer and credentials-
// neighbour checks (conformance.DaemonOps): each against the suite's own
// daemon under B/home, which the suite started, and the environment's
// configuration under B. None answers a request, revokes a grant, asks
// the daemon for the web page's token, opens a browser, or reaches a
// daemon or a web page but the suite's own.

var _ conformance.DaemonOps = (*confHost)(nil)

// confHostFacts are what the daemon calls read of this machine rather than of
// B: the process table, who listens on a port, the addresses, the web
// page, the host sockets a sandbox is created with. Seams, so that the
// tests depend on none of this machine's.
type confHostFacts struct {
	// ps runs ps with args.
	ps func(args ...string) (string, error)
	// listeners are the PIDs listening on TCP port of this machine.
	listeners func(port int) ([]int, error)
	// addrs are this machine's interface addresses.
	addrs func() ([]netip.Addr, error)
	// get is the HTTP status of a GET of url, following no redirect.
	get func(ctx context.Context, url string) (int, error)
	// sockets are the host sockets createContainer would mount into a's
	// sandbox (the engine's, the SSH agent's).
	sockets func(a *App) []backend.Mount
}

func realConfHostFacts() confHostFacts {
	return confHostFacts{ps: hostPS, listeners: lsofListeners, addrs: interfaceAddrs, get: httpStatus, sockets: socketMounts}
}

// Bounds of the daemon calls.
const (
	confDaemonStateMax = 64 << 10
	confUIGetTimeout   = 5 * time.Second
	confMaxHostAddrs   = 16
	confMaxDoctor      = 1 << 20
	// confDaemonLogMax bounds each daemon log file read: past the
	// daemon's own cap (10 MiB).
	confDaemonLogMax = 11 << 20
)

// lsofListeners are the PIDs lsof says listen on TCP port: none when it
// lists none.
func lsofListeners(port int) ([]int, error) {
	out, err := exec.Command("lsof", "-nP", "-a", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0) {
		return nil, fmt.Errorf("lsof: %v", err)
	}
	return parsePIDs(string(out))
}

// parsePIDs reads one PID a line.
func parsePIDs(out string) ([]int, error) {
	var pids []int
	for _, f := range strings.Fields(out) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("not a process id: %q", f)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

func httpStatus(ctx context.Context, url string) (int, error) {
	c := &http.Client{
		Timeout:       confUIGetTimeout,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

// socketMounts are the host sockets createContainer mounts (hostSockets).
func socketMounts(a *App) []backend.Mount { return a.hostSockets().Mounts }

// confDaemonReader is all of the daemon's methods the daemon calls use:
// its status and the broker's listings. Nothing that answers, revokes or
// hands out the web page's token is in it.
type confDaemonReader interface {
	Status(ctx context.Context) (*daemonproto.StatusResult, error)
	Requests(ctx context.Context, env string, all bool, limit int) (*daemonproto.RequestsResult, error)
	Grants(ctx context.Context, env string) (*daemonproto.GrantsResult, error)
}

// suiteDaemonDir is the suite's daemon's directory: the App's, which must
// be one of the daemon directories of the throwaway CABOOSE_HOME.
func (h *confHost) suiteDaemonDir() (string, error) {
	if h.a == nil {
		return "", errors.New("conformance: the environment's App was not built")
	}
	dir := filepath.Clean(h.a.daemonDir())
	if !confInside(dir, h.G.CabooseHome()) || !slices.Contains(daemon.Dirs(h.G.CabooseHome()), dir) {
		return "", fmt.Errorf("conformance: the daemon's directory %s is not one of %s's", dir, h.G.CabooseHome())
	}
	return dir, nil
}

// suitePID is the PID of the daemon the suite started.
func (h *confHost) suitePID() (int, error) {
	if h.daemon == nil {
		return 0, errors.New("conformance: the suite has no daemon of its own")
	}
	pid := h.daemon.spawned()
	if pid == 0 {
		return 0, errors.New("conformance: the suite has not started its daemon")
	}
	return pid, nil
}

// readDaemon dials the suite's daemon, asking only (nothing is started),
// confirms by its status that it is the daemon the suite started, in the
// suite's directory, and hands fn its read-only methods.
func (h *confHost) readDaemon(ctx context.Context, fn func(confDaemonReader, *daemonproto.StatusResult) error) error {
	dir, err := h.suiteDaemonDir()
	if err != nil {
		return err
	}
	pid, err := h.suitePID()
	if err != nil {
		return err
	}
	c, err := h.a.dialDaemon(ctx, false)
	if err != nil {
		return fmt.Errorf("conformance: the suite's daemon does not answer: %v", err)
	}
	defer c.Close()
	var r confDaemonReader = c
	st, err := r.Status(ctx)
	if err != nil {
		return fmt.Errorf("conformance: the suite's daemon: %v", err)
	}
	switch {
	case st.Daemon.PID != pid:
		return fmt.Errorf("conformance: the daemon that answers is pid %d, not the suite's %d", st.Daemon.PID, pid)
	case st.Daemon.Dir != "" && filepath.Clean(st.Daemon.Dir) != dir:
		return fmt.Errorf("conformance: the daemon that answers is in %s, not %s", st.Daemon.Dir, dir)
	}
	if fn == nil {
		return nil
	}
	return fn(r, st)
}

// procStart is the start time ps gives for pid; ok is false when no such
// process runs.
func (h *confHost) procStart(pid int) (string, bool) {
	out, err := h.facts.ps("-o", "lstart=", "-p", strconv.Itoa(pid))
	s := strings.Join(strings.Fields(out), " ")
	return s, err == nil && s != ""
}

// Daemon is the suite's daemon as its daemon.json, the process table and
// its status say. A daemon.json naming another process than the one the
// suite started is an error, never another daemon to describe.
func (h *confHost) Daemon() (conformance.DaemonInfo, error) {
	dir, err := h.suiteDaemonDir()
	if err != nil {
		return conformance.DaemonInfo{}, err
	}
	pid, err := h.suitePID()
	if err != nil {
		return conformance.DaemonInfo{}, err
	}
	info := conformance.DaemonInfo{Dir: dir}
	rel, err := filepath.Rel(h.G.Base(), dir)
	if err != nil {
		return info, err
	}
	b, _, err := nofollow.Dir(h.G.Base()).ReadFileMax(filepath.ToSlash(rel)+"/"+daemon.StateFile, confDaemonStateMax)
	if errors.Is(err, fs.ErrNotExist) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	var st daemon.State
	if err := json.Unmarshal(b, &st); err != nil {
		return info, fmt.Errorf("%s: %v", daemon.StateFile, err)
	}
	if st.PID != pid {
		return info, fmt.Errorf("conformance: %s names pid %d, not the suite's daemon %d", daemon.StateFile, st.PID, pid)
	}
	info.PID = pid
	started, ok := h.procStart(pid)
	if !ok {
		return info, nil
	}
	info.Running, info.Started = true, started
	ctx, cancel := context.WithTimeout(context.Background(), daemonStatusBudget)
	defer cancel()
	var web *daemonproto.WebState
	if err := h.readDaemon(ctx, func(_ confDaemonReader, s *daemonproto.StatusResult) error {
		web = s.Daemon.Web
		return nil
	}); err != nil {
		return info, err
	}
	if web != nil && web.Listening && web.Addr != "" {
		info.WebAddr = web.Addr
		info.WebOwned = h.webOwned(web.Addr, pid)
	}
	return info, nil
}

// webOwned reports whether the web page's address is a web UI port on
// the loopback and every process listening on that port of this machine
// is the daemon pid: the throwaway daemon's own listener, and no one
// else's (the person's own daemon's included).
func (h *confHost) webOwned(addr string, pid int) bool {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() || !uiport.Is(int(ap.Port())) {
		return false
	}
	pids, err := h.facts.listeners(int(ap.Port()))
	if err != nil || len(pids) == 0 {
		return false
	}
	for _, p := range pids {
		if p != pid {
			return false
		}
	}
	return true
}

func (h *confHost) HostHello(ctx context.Context) error { return h.readDaemon(ctx, nil) }

// LinkSocket is where the link's socket is in a sandbox.
func (h *confHost) LinkSocket() string { return agent.SocketPath }

// HostAddrs are this machine's addresses but loopback, link-local (which
// need a zone no sandbox has) and multicast ones, each once, in order, at
// most confMaxHostAddrs. A vm guest's gateway is the address of this
// machine's side of its network, among them while the guest runs.
func (h *confHost) HostAddrs() ([]netip.Addr, error) {
	as, err := h.facts.addrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		a = a.Unmap()
		if !a.IsValid() || a.Zone() != "" || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
			continue
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	if len(out) > confMaxHostAddrs {
		out = out[:confMaxHostAddrs]
	}
	return out, nil
}

// UIStatic is the status of the web page's GET /, with no token, asked
// only of a listener the throwaway daemon owns.
func (h *confHost) UIStatic(ctx context.Context) (int, error) {
	info, err := h.Daemon()
	if err != nil {
		return 0, err
	}
	if !info.Running || info.WebAddr == "" || !info.WebOwned {
		return 0, errors.New("conformance: the web UI's listener is not the suite's daemon's: nothing was asked of it")
	}
	return h.facts.get(ctx, "http://"+info.WebAddr+"/")
}

// TryUI loads a copy of config.toml changed as v says, with the
// launcher's own loader, and decides the reach to the web UI's port it
// makes with the product's own decisions; nothing is written.
func (h *confHost) TryUI(v conformance.UIVariant) (conformance.UIDecision, error) {
	if h.a == nil {
		return conformance.UIDecision{}, errors.New("conformance: the environment's App was not built")
	}
	e, decide, err := h.uiEdit(v)
	if err != nil {
		return conformance.UIDecision{}, err
	}
	orig, err := h.readConfig()
	if err != nil {
		return conformance.UIDecision{}, err
	}
	p := filepath.Join(h.G.Base(), filepath.FromSlash(h.configRel()))
	edited := config.EditFile(orig, e)
	if err := config.CheckEdit(p, orig, edited, e); err != nil {
		return conformance.UIDecision{Stage: conformance.UIAtLoad, Message: err.Error()}, nil
	}
	cfg, err := config.Load(h.a.Cfg.Getenv, confOverlay{path: p, data: edited}, h.a.Cfg.Env)
	if err != nil {
		return conformance.UIDecision{Stage: conformance.UIAtLoad, Message: err.Error()}, nil
	}
	return decide(cfg)
}

// uiEdit is the change v makes, and how a configuration that loads with
// it decides the reach. v sets exactly one of its fields.
func (h *confHost) uiEdit(v conformance.UIVariant) (config.Edit, func(*config.Config) (conformance.UIDecision, error), error) {
	set := 0
	for _, on := range []bool{v.ForwardPorts != "", len(v.OpenURLsAllow) > 0, len(v.EgressAllow) > 0, len(v.RunArgs) > 0} {
		if on {
			set++
		}
	}
	if set != 1 {
		return config.Edit{}, nil, fmt.Errorf("conformance: a UI variant changes one setting, not %d", set)
	}
	refused := func(err error) (conformance.UIDecision, error) {
		return conformance.UIDecision{Stage: conformance.UIAtDecide, Message: err.Error()}, nil
	}
	accepted := conformance.UIDecision{}
	switch {
	case v.ForwardPorts != "":
		r, err := confPortRange(v.ForwardPorts)
		if err != nil {
			return config.Edit{}, nil, err
		}
		e := config.Edit{Set: map[string]any{"link.forward_ports": []string{r}}}
		// What the link forwards: forward_ports as it parses it, less
		// the web UI's ports, which it never forwards.
		return e, func(cfg *config.Config) (conformance.UIDecision, error) {
			set, err := config.ParsePorts(cfg.ForwardPorts)
			if err != nil {
				return refused(err)
			}
			if err := config.CheckForwardPorts("forward_ports", set); err != nil {
				return refused(err)
			}
			return accepted, nil
		}, nil
	case len(v.OpenURLsAllow) > 0:
		e := config.Edit{Set: map[string]any{"link.open_urls_allow": slices.Clone(v.OpenURLsAllow)}}
		// What the link opens: every URL goes through CheckURL first.
		return e, func(cfg *config.Config) (conformance.UIDecision, error) {
			for _, o := range cfg.OpenURLsAllow {
				if _, err := hostlink.CheckURL(o + "/"); err != nil {
					return refused(err)
				}
			}
			return accepted, nil
		}, nil
	case len(v.EgressAllow) > 0:
		// egress_allow names hosts and addresses; each entry here is an
		// address and the port the reach is to, which egress_ports then
		// holds, so that the decision gets past the port to the address.
		// A vm profile's keys: the copy uses the suite's vm profile, with
		// the outbound proxy on.
		prof := config.KindVM + "." + confProfile
		var allow, ports []string
		var targets []netip.AddrPort
		for _, s := range v.EgressAllow {
			ap, err := netip.ParseAddrPort(s)
			if err != nil {
				return config.Edit{}, nil, fmt.Errorf("conformance: egress_allow variant %q is not an address and port", s)
			}
			allow = append(allow, ap.Addr().String())
			ports = append(ports, confPorts([]int{int(ap.Port())})...)
			targets = append(targets, ap)
		}
		e := config.Edit{Tables: []string{prof}, Set: map[string]any{
			"isolation": prof, prof + ".egress": true, prof + ".egress_allow": allow, prof + ".egress_ports": ports,
		}}
		return e, func(cfg *config.Config) (conformance.UIDecision, error) {
			eg, err := confEgressPolicy(cfg)
			if err != nil {
				return refused(err)
			}
			own, err := h.facts.addrs()
			if err != nil {
				return conformance.UIDecision{}, err
			}
			p := policy.Policy{Trust: policy.Trust(cfg.Trust), Egress: eg}
			for _, t := range targets {
				d := policy.Decide(p, policy.Request{Kind: policy.KindEgress, Host: t.Addr().String(), Literal: true,
					Port: int(t.Port()), Addrs: []netip.Addr{t.Addr()}, Own: own}, time.Now())
				if d.Verdict != policy.VerdictAllow {
					return conformance.UIDecision{Stage: conformance.UIAtDecide, Message: d.Reason}, nil
				}
			}
			return accepted, nil
		}, nil
	default:
		// run_args is a container or gvisor profile's: the copy uses the
		// suite's profile of the running kind, or its container one
		// under vm.
		kind := h.G.Isolation()
		if kind != config.KindContainer && kind != config.KindGVisor {
			kind = config.KindContainer
		}
		prof := kind + "." + confProfile
		e := config.Edit{Tables: []string{prof}, Set: map[string]any{"isolation": prof, prof + ".run_args": slices.Clone(v.RunArgs)}}
		// What a creation refuses (checkRunArgs, as createContainer
		// checks them before anything is built), then what doctor
		// reports of them.
		return e, func(cfg *config.Config) (conformance.UIDecision, error) {
			if err := checkRunArgs(cfg.RunArgs, nil, cfg.Roots); err != nil {
				return refused(err)
			}
			a, err := h.appWith(cfg)
			if err != nil {
				return conformance.UIDecision{}, err
			}
			c := &checkup{}
			a.doctorRunArgs(c)
			for _, r := range c.rows {
				if r.level == levelProblem {
					return conformance.UIDecision{Stage: conformance.UIAtDoctor, Message: r.text}, nil
				}
			}
			return accepted, nil
		}, nil
	}
}

// confPortRange is a port or a range as config.EditFile's read-back check
// takes it in a string list: "N" as "N-N".
func confPortRange(s string) (string, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		hi = lo
	}
	a, err1 := strconv.Atoi(lo)
	b, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || a < 1 || b < a || b > 65535 {
		return "", fmt.Errorf("conformance: %q is not a port or a range", s)
	}
	return strconv.Itoa(a) + "-" + strconv.Itoa(b), nil
}

// confEgressPolicy is the outbound proxy's policy for cfg, as the broker
// builds it from config.toml: an error where the broker would refuse
// everything.
func confEgressPolicy(cfg *config.Config) (*policy.Egress, error) {
	if cfg.Isolation != config.KindVM || !cfg.Egress {
		return nil, errors.New("the outbound proxy is off")
	}
	ports, err := config.ParsePortsOf("egress_ports", cfg.EgressPorts)
	if err != nil {
		return nil, err
	}
	allow, err := policy.ParseAllowList("egress_allow", cfg.EgressAllow)
	if err != nil {
		return nil, err
	}
	deny, err := policy.ParseAllowList("egress_deny", cfg.EgressDeny)
	if err != nil {
		return nil, err
	}
	return &policy.Egress{Ports: ports, Allow: allow, Deny: deny}, nil
}

// Requests are the broker's requests for the environment, every state,
// as the daemon lists them: read only.
func (h *confHost) Requests() ([]conformance.RequestInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonLinkRequest)
	defer cancel()
	var out []conformance.RequestInfo
	err := h.readDaemon(ctx, func(r confDaemonReader, _ *daemonproto.StatusResult) error {
		res, err := r.Requests(ctx, h.G.Env(), true, daemonproto.MaxRequestsLimit)
		if err != nil {
			return err
		}
		if res.Truncated {
			return errors.New("conformance: the daemon left requests out of its listing")
		}
		for _, v := range res.Requests {
			ri := conformance.RequestInfo{ID: v.ID, State: v.State, Kind: v.Kind}
			if v.Kind == string(policy.KindAlias) {
				ri.Alias, _ = strings.CutPrefix(v.Key, string(policy.KindAlias)+":")
			}
			out = append(out, ri)
		}
		return nil
	})
	return out, err
}

// Grants is how many grants the broker holds for the environment: read
// only.
func (h *confHost) Grants() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonLinkRequest)
	defer cancel()
	n := 0
	err := h.readDaemon(ctx, func(r confDaemonReader, _ *daemonproto.StatusResult) error {
		res, err := r.Grants(ctx, h.G.Env())
		if err != nil {
			return err
		}
		if res.Truncated {
			return errors.New("conformance: the daemon left grants out of its listing")
		}
		n = len(res.Grants)
		return nil
	})
	return n, err
}

// NextMounts are the mounts the next creation would give the sandbox
// (createContainer's: the host sockets, then dataMounts, then its
// volumes, which a VM has as disks and the audit reads as mounts by
// their targets), from config.toml
// and the sandbox config as they are now and the running sandbox's
// platform, creating and building nothing.
func (h *confHost) NextMounts() ([]conformance.Mount, error) {
	a, err := h.app()
	if err != nil {
		return nil, err
	}
	if h.box == nil {
		return nil, errors.New("conformance: the sandbox is not reachable")
	}
	platform := h.box.Labels()[assets.LabelPlatform]
	if platform == "" {
		return nil, errors.New("conformance: the running sandbox names no Claude Code platform")
	}
	sb, err := a.sandboxConfig()
	if err != nil {
		return nil, err
	}
	// h.app() resolved the roots, which refuses a writable part reached
	// through a link or missing a component, as createContainer refuses
	// it again before a creation.
	ms := append(h.facts.sockets(a), dataMounts(a.Cfg.DataDir, platform, sb, a.Cfg.Roots)...)
	out := make([]conformance.Mount, len(ms))
	for i, m := range ms {
		out[i] = conformance.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly}
	}
	for _, v := range a.sandboxVolumes() {
		out = append(out, conformance.Mount{Volume: v.Name, Target: v.Target})
	}
	return out, nil
}

// Doctor is caboose doctor --offline's report for the environment, run
// in this process with the suite's App.
func (h *confHost) Doctor(ctx context.Context) (string, error) {
	a, err := h.app()
	if err != nil {
		return "", err
	}
	var out confDoctorOut
	a.Stdout = &out
	err = a.Doctor([]string{"--offline"})
	var ee *ExitError
	if err != nil && !(errors.As(err, &ee) && ee.Code == doctorProblems) {
		return out.String(), err
	}
	return out.String(), nil
}

// confDoctorOut keeps the first confMaxDoctor bytes of doctor's report.
type confDoctorOut struct{ strings.Builder }

func (o *confDoctorOut) Write(p []byte) (int, error) {
	if room := confMaxDoctor - o.Len(); room > 0 {
		o.Builder.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// OpenURLs is the open_urls the running link decides by, as its link.json
// says.
func (h *confHost) OpenURLs() (string, error) {
	st, err := h.readLinkState()
	if err != nil {
		return "", err
	}
	if st.OpenURLs == "" {
		return "", errors.New("the running link's link.json names no open_urls")
	}
	return st.OpenURLs, nil
}

func (h *confHost) StubOpened() ([]string, error) { return h.G.StubOpened() }

// DaemonRSS is the resident memory ps gives for the suite's daemon.
func (h *confHost) DaemonRSS() (int64, error) {
	info, err := h.Daemon()
	if err != nil {
		return 0, err
	}
	if !info.Running {
		return 0, errors.New("conformance: the suite's daemon is not running")
	}
	out, err := h.facts.ps("-o", "rss=", "-p", strconv.Itoa(info.PID))
	if err != nil {
		return 0, fmt.Errorf("ps: %v", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || kb <= 0 {
		return 0, fmt.Errorf("ps gives %q for the daemon's resident memory", strings.TrimSpace(out))
	}
	return kb * 1024, nil
}

// DaemonLog marks the suite daemon's daemon.log.1 and daemon.log, read
// without following a link; a missing file is empty.
func (h *confHost) DaemonLog() (conformance.DaemonLogMark, error) {
	dir, err := h.suiteDaemonDir()
	if err != nil {
		return conformance.DaemonLogMark{}, err
	}
	rel, err := filepath.Rel(h.G.Base(), dir)
	if err != nil {
		return conformance.DaemonLogMark{}, err
	}
	var bs [2][]byte
	for i, f := range []string{daemon.LogFile + ".1", daemon.LogFile} {
		b, _, err := nofollow.Dir(h.G.Base()).ReadFileMax(filepath.ToSlash(rel)+"/"+f, confDaemonLogMax)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return conformance.DaemonLogMark{}, fmt.Errorf("%s: %v", f, err)
		}
		bs[i] = b
	}
	return conformance.MarkDaemonLog(bs[0], bs[1]), nil
}

// HostSilent connects to the suite daemon's socket and closes at once,
// sending nothing.
func (h *confHost) HostSilent(ctx context.Context) error {
	dir, err := h.suiteDaemonDir()
	if err != nil {
		return err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", filepath.Join(dir, daemon.SocketFile))
	if err != nil {
		return fmt.Errorf("conformance: the suite's daemon socket: %v", err)
	}
	return c.Close()
}

// Relays: the SSH agent is the one socket caboose-agent serves for the
// host in the sandbox, under vm, where no socket of the host's can be
// mounted in.
func (h *confHost) Relays() map[string]string {
	return map[string]string{containerAgent: "the host's SSH agent"}
}
