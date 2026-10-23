package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
	"github.com/bfreis/caboose/internal/uiport"
)

// The daemon, web UI, answer and credentials-neighbour checks, D0-D9, and
// the two gap lines X1 and X2. The sandbox looks for what of the host's it
// must not see (by name and by content), for the daemon's socket, for the
// web UI's port, and tries to answer its own request, to widen its own
// configuration, to open the UI in the host's browser, to flood the link
// and to signal the daemon; every verdict is the host's: the canaries'
// identities, the broker's requests and grants, config.toml's hash, the
// daemon's pid and the suite's stubs. Nothing here reads a token or a
// credential, answers a request, or opens a browser: a URL the host would
// open reaches the suite's stub opener, which the checks require first.

const (
	groupDaemon = "daemon"
	stepDaemon  = "the daemon, the web UI and the credentials' neighbours"

	// The canaries the sandbox must find: in the writable part and in the
	// sandbox's home (the data dir's home/).
	RoleDRoot = "d-root"
	RoleDHome = "d-home"
	// The canaries the sandbox must not find, at the host location of what
	// each stands for.
	RoleDOutside = "d-outside"
	RoleDCred    = "d-cred"
	RoleDDaemon  = "d-daemon"
	RoleDGrants  = "d-grants"
	RoleDCfg     = "d-cfg"
	RoleDCkpt    = "d-ckpt"
	// RoleX1 stands for the credentials Claude Code keeps in the sandbox's
	// home: a file of the suite's own, never a real one.
	RoleX1 = "x1-cred"

	// daemonSettle is how long a check gives the host to act on what the
	// sandbox wrote or sent before it reads the host's state.
	daemonSettle = 5 * time.Second
	// refill is how long the link's request bucket takes to fill again.
	refill = 12 * time.Second
	// floodN is how many requests a flood sends at once, and floodMax how
	// many of them may get through for the flood to count as bounded.
	floodN, floodMax = 40, 20
	// floodRequestsMax bounds the requests a flood of the same request
	// may file.
	floodRequestsMax = 8
	// rssFactor and rssSlack bound the daemon's memory after the floods.
	rssFactor = 2
	rssSlack  = 64 << 20
	// maxSockets is how many sockets D2 tries.
	maxSockets = 32
	// witnessWait is how long D2 gives the daemon to log a connection it
	// refused before reading its log: past its hello timeout (2 s).
	witnessWait = 3 * time.Second

	gapX1Open   = "kept home readable and writable by the agent (by design)"
	gapX1Closed = "closed: the sandbox's home is no longer readable and writable by the agent; update docs/security.md"
	gapX2       = "any process of the user's can speak to the daemon: its socket takes its own user's connections (the daemon is the user's; a hostile local process is out of the model)"
)

// hostOnlyRoles are the canaries no sandbox may see, by name or content:
// this check family's and the file checks' watched ones.
var hostOnlyRoles = append([]string{RoleDOutside, RoleDCred, RoleDDaemon, RoleDGrants, RoleDCfg, RoleDCkpt}, watchRoles...)

// DaemonDirs are the host directories, relative to B, that the canaries
// of DaemonChecks are made in.
type DaemonDirs struct {
	// Part is the writable part, Home the data dir's home/.config/caboose
	// (the sandbox's ~/.config/caboose, which is always kept), Outside a directory no mount reaches (canary/outside-root),
	// Creds the stand-in credentials' directory (canary/creds), Daemon the
	// daemon's directory (home/daemon or home/daemon-dev), Env the
	// environment's directory (home/envs/<name>, beside config.toml and
	// grants.json), Checkpoints its checkpoint store.
	Part, Home, Outside, Creds, Daemon, Env, Checkpoints string
}

// DaemonLayout lists the canaries of DaemonChecks. The probe binary
// (conformance.ProbeRel) is placed beside them by the suite.
func DaemonLayout(d DaemonDirs) ([]HostCanary, error) {
	for _, p := range []string{d.Part, d.Home, d.Outside, d.Creds, d.Daemon, d.Env, d.Checkpoints} {
		if p == "" || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") {
			return nil, errors.New("checks: the daemon checks need every directory, relative to B")
		}
	}
	switch {
	case !strings.HasSuffix(d.Part, "/part"):
		return nil, errors.New("checks: Part is the writable part")
	case d.Home != "data/home/.config/caboose" && d.Home != d.Env+"/data/home/.config/caboose":
		return nil, errors.New("checks: Home is the data dir's home/.config/caboose")
	case !strings.HasPrefix(d.Daemon, "home/daemon"):
		return nil, errors.New("checks: Daemon is the daemon's directory under the throwaway CABOOSE_HOME")
	case !strings.HasPrefix(d.Env, "home/envs/") || d.Checkpoints != d.Env+"/checkpoints":
		return nil, errors.New("checks: Env is the environment's directory, Checkpoints its store")
	}
	return []HostCanary{
		{d.Part, RoleDRoot, c.File}, {d.Home, RoleDHome, c.File}, {d.Home, RoleX1, c.File},
		{d.Outside, RoleDOutside, c.File}, {d.Creds, RoleDCred, c.File},
		{d.Daemon, RoleDDaemon, c.File}, {d.Env, RoleDGrants, c.File}, {d.Env, RoleDCfg, c.File},
		{d.Checkpoints, RoleDCkpt, c.File},
	}, nil
}

// DaemonChecks are D0-D9, X1 and X2 in the order they run: the ones that
// only look, then the ones that write or send.
func DaemonChecks() []c.Check {
	return []c.Check{d0(), d1(), d2(), d3(), d4(), x2(), d5(), d6(), d7(), d8(), d9(), x1()}
}

// DaemonForRun are the daemon checks for a run: none under hostile
// without vm, where no sandbox is made.
func DaemonForRun(iso, trust string) []c.Check {
	if trust == c.TrustHostile && iso != "vm" {
		return nil
	}
	return DaemonChecks()
}

// daemonRun is one daemon check's run.
type daemonRun struct {
	env   c.Env
	ops   c.HostOps
	d     c.DaemonOps
	n8    string
	nonce string
}

func daemonCheck(id, title, control string, class c.Class, roles []string, run func(context.Context, *daemonRun) c.CheckResult) c.Check {
	return c.Check{
		ID: id, Title: title, Control: control, Step: stepDaemon, Group: groupDaemon,
		Class: class, Roles: roles,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			d, bad := prepareDaemon(env)
			if bad != nil {
				return *bad
			}
			return run(ctx, d)
		},
	}
}

var contentRe = regexp.MustCompile(`^caboose-conformance ([0-9a-z-]{8,48}) `)

// prepareDaemon is what every daemon check needs first: a running
// sandbox, the host's calls, the stubs in place, and the shared home
// canary on the host, whose content gives the run's nonce.
func prepareDaemon(env c.Env) (*daemonRun, *c.CheckResult) {
	bad := func(r c.CheckResult) (*daemonRun, *c.CheckResult) { return nil, &r }
	if st := env.State(); st != c.StateRunning {
		return bad(errorf("the sandbox is not running (%s)", st))
	}
	ops, err := env.HostOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	d, err := env.DaemonOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	if err := ops.StubsInPlace(); err != nil {
		return bad(errorf("the suite's stub opener and notifier are not in place, so nothing was sent: %v", err))
	}
	home, err := env.Canary(RoleDHome)
	if err != nil {
		return bad(errorf("canary %s is missing: %v", RoleDHome, err))
	}
	m := n8Of.FindStringSubmatch(home.Rel)
	if m == nil {
		return bad(errorf("canary %s has no nonce in its name", RoleDHome))
	}
	b, err := ops.ReadCanary(home.Rel)
	if err != nil {
		return bad(errorf("canary %s could not be read on the host: %v", RoleDHome, err))
	}
	cm := contentRe.FindSubmatch(b)
	if cm == nil || !strings.HasPrefix(string(cm[1]), m[1]) {
		return bad(errorf("canary %s does not hold the run's nonce", RoleDHome))
	}
	return &daemonRun{env: env, ops: ops, d: d, n8: m[1], nonce: string(cm[1])}, nil
}

// sh runs cmd in the sandbox as user; a shell that did not run the
// wrapper is an error.
func (d *daemonRun) sh(ctx context.Context, user string, cmd c.Cmd, err error) (c.ExecResult, error) {
	if err != nil {
		return c.ExecResult{}, err
	}
	res, err := d.env.RunCmd(ctx, user, cmd)
	if err == nil && res.RC < 0 {
		err = fmt.Errorf("the sandbox's shell did not run the command: %s", res.Msg)
	}
	return res, err
}

// link runs a caboose-agent command that the link's request bucket
// meters: refused for its rate, it is sent again once the bucket has
// filled, so that the rate limit never answers in a control's place.
func (d *daemonRun) link(ctx context.Context, cmd c.Cmd, err error) (c.ExecResult, error) {
	res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
	if err != nil || !strings.Contains(res.Out, c.TooMany) {
		return res, err
	}
	if err := d.ops.Wait(ctx, refill); err != nil {
		return res, err
	}
	res, err = d.sh(ctx, d.env.SessionUser(), cmd, nil)
	if err == nil && strings.Contains(res.Out, c.TooMany) {
		err = errors.New("the link's rate limit answered twice, a bucket's fill apart")
	}
	return res, err
}

// lines are the non-empty lines of a command's output.
func lines(out string) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	return ls
}

// scanned is what one scan of the sandbox found.
type scanned struct {
	names, content []string
}

// scan runs both searches, as root, and requires each to have gone
// through and to have found the shared canaries by name and by content:
// a search that cannot find what is there proves nothing by finding
// nothing.
func (d *daemonRun) scan(ctx context.Context) (scanned, *c.CheckResult) {
	var s scanned
	for _, x := range []struct {
		what string
		dst  *[]string
		mk   func() (c.Cmd, error)
	}{
		{"by name", &s.names, func() (c.Cmd, error) { return c.ScanNames(d.n8) }},
		{"by content", &s.content, func() (c.Cmd, error) { return c.ScanContent(d.n8, d.nonce) }},
	} {
		cmd, err := x.mk()
		res, err := d.sh(ctx, "root", cmd, err)
		if err != nil {
			r := errorf("the search %s could not be run: %v", x.what, err)
			return s, &r
		}
		ls := lines(res.Out)
		if res.RC == 0 && len(ls) > c.ScanMax && !slices.Contains(ls, c.ScanComplete(d.n8)) {
			// The cap cut the completion line off: the search may have gone
			// through, but what it found past the cap is not known.
			r := errorf("the search %s found more than the %d paths it may print", x.what, c.ScanMax)
			return s, &r
		}
		if res.RC != 0 || !slices.Contains(ls, c.ScanComplete(d.n8)) {
			r := errorf("the search %s did not go through (exit %d)%s", x.what, res.RC, msgOf(res))
			return s, &r
		}
		*x.dst = slices.DeleteFunc(ls, func(l string) bool { return l == c.ScanComplete(d.n8) || !strings.HasPrefix(l, "/") })
	}
	for _, role := range []string{RoleDRoot, RoleDHome} {
		cn, err := d.env.Canary(role)
		if err != nil {
			r := errorf("canary %s is missing: %v", role, err)
			return s, &r
		}
		want, err := d.sharedPath(cn)
		if err != nil {
			r := errorf("%v", err)
			return s, &r
		}
		if !slices.Contains(s.names, want) {
			r := errorf("positive control failed: the search by name did not find %s, which the sandbox has", want)
			return s, &r
		}
		if !slices.Contains(s.content, want) {
			r := errorf("positive control failed: the search by content did not find %s, which the sandbox has", want)
			return s, &r
		}
	}
	return s, nil
}

// sharedPath is where the sandbox sees a shared canary.
func (d *daemonRun) sharedPath(cn c.Canary) (string, error) {
	if p, err := c.HomePath(cn); err == nil {
		return p.String(), nil
	}
	p, err := c.SandboxPath(cn)
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// roleIn is the canary role a path's components name, the last one that
// does: "" for none.
func (d *daemonRun) rolesIn(p string) []string {
	var out []string
	prefix := "conf-" + d.n8 + "-"
	for _, seg := range strings.Split(p, "/") {
		if r, ok := strings.CutPrefix(seg, prefix); ok {
			out = append(out, r)
		}
	}
	return out
}

// isHomeCanary reports whether p is the sandbox's path of a canary of the
// run that was made in the sandbox's home canaries' directory: by its
// role, as made, never by its name alone, since the name search prints a
// directory and not what is below it, so that any other of the suite's
// directories mounted there would pass for one.
func (d *daemonRun) isHomeCanary(p string) bool {
	rest, ok := strings.CutPrefix(p, c.HomeCanaryDir+"/conf-"+d.n8+"-")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return false
	}
	cn, err := d.env.Canary(rest)
	if err != nil {
		return false
	}
	hp, err := c.HomePath(cn)
	return err == nil && hp.String() == p
}

// inDir reports whether p is dir or below it.
func inDir(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// rootsOf are the sandbox paths of the suite's two roots.
func (d *daemonRun) rootsOf() []string {
	return []string{"/work/conf-" + d.n8 + "-ro", "/work/conf-" + d.n8 + "-dev"}
}

func d0() c.Check {
	return daemonCheck("D0", "the scan finds what is shared, and only where it is mounted", "B reached only through the suite's mounts", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			s, r := d.scan(ctx)
			if r != nil {
				return *r
			}
			// The home holds the suite's own home canaries and the files
			// the checks write into its proposals and sync mounts; any other
			// of the suite's names there came through a mount of the host's.
			places := append(d.rootsOf(), "/etc/claude-code", "/home/agent/.caboose-proposals", "/home/agent/.caboose-sync")
			var stray []string
			for _, p := range s.names {
				if !d.isHomeCanary(p) && !slices.ContainsFunc(places, func(dir string) bool { return inDir(p, dir) }) {
					stray = append(stray, p)
				}
			}
			if len(stray) > 0 {
				slices.Sort(stray)
				return c.CheckResult{Status: c.Fail, Reason: fmt.Sprintf("the suite's files are reachable where no mount the suite made puts them: %d path(s)", len(stray)),
					Lines: []string{"look at: " + strings.Join(clipList(stray), ", ")}}
			}
			return pass(fmt.Sprintf("found %d path(s) by name and %d by content, all in the suite's mounts", len(s.names), len(s.content)))
		})
}

func clipList(ps []string) []string {
	if len(ps) > 8 {
		return append(slices.Clone(ps[:8]), fmt.Sprintf("and %d more", len(ps)-8))
	}
	return ps
}

func d1() c.Check {
	return daemonCheck("D1", "host files outside the roots, the daemon dir, the policy and the grants are not in the sandbox", "host-only files unreachable", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			var neg []c.Canary
			for _, role := range []string{RoleDOutside, RoleDCred, RoleDDaemon, RoleDGrants, RoleDCfg, RoleDCkpt} {
				cn, err := d.env.Canary(role)
				if err != nil {
					return errorf("canary %s is missing: %v", role, err)
				}
				if snap, err := d.env.Snapshot(cn); err != nil || len(snap) == 0 {
					return errorf("canary %s is not on the host", role)
				}
				neg = append(neg, cn)
			}
			before := map[string]map[string]c.Identity{}
			for _, cn := range neg {
				before[cn.Role], _ = d.env.Snapshot(cn)
			}
			s, r := d.scan(ctx)
			if r != nil {
				return *r
			}
			var byName []string
			for _, p := range s.names {
				for _, role := range d.rolesIn(p) {
					if slices.Contains(hostOnlyRoles, role) {
						byName = append(byName, p)
						break
					}
				}
			}
			// The home canaries' directory is a kept one, shared by design:
			// the suite's canaries there (these checks' and the checkpoint
			// checks') hold the nonce, and a host-only one there is caught
			// by name above.
			var byContent []string
			for _, p := range s.content {
				if slices.ContainsFunc(d.rootsOf(), func(dir string) bool { return inDir(p, dir) }) || d.isHomeCanary(p) {
					continue
				}
				byContent = append(byContent, p)
			}
			for _, cn := range neg {
				after, err := d.env.Snapshot(cn)
				if err != nil || changed(before[cn.Role], after) {
					return failf("canary %s changed on the host while the sandbox searched", cn.Role)
				}
			}
			switch {
			case len(byName) > 0:
				slices.Sort(byName)
				return c.CheckResult{Status: c.Fail, Reason: fmt.Sprintf("the sandbox found %d host-only canary path(s) by name", len(byName)),
					Lines: []string{"look at: " + strings.Join(clipList(byName), ", ")}}
			case len(byContent) > 0:
				slices.Sort(byContent)
				return c.CheckResult{Status: c.Fail, Reason: fmt.Sprintf("the run's nonce is in %d file(s) of the sandbox the suite did not share", len(byContent)),
					Lines: []string{"look at: " + strings.Join(clipList(byContent), ", ")}}
			}
			return pass()
		})
}

// probeOutcomes are the JSON lines the probe printed.
func probeOutcomes(out string) ([]probe.Outcome, error) {
	var outs []probe.Outcome
	for _, l := range lines(out) {
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var o probe.Outcome
		if err := json.Unmarshal([]byte(l), &o); err != nil {
			return nil, fmt.Errorf("the probe printed a line that is not its own: %v", err)
		}
		outs = append(outs, o)
	}
	return outs, nil
}

// probeInPlace is nil when the suite's probe is in the writable part, as
// the suite placed it: the sandbox can write there, and a probe it had
// replaced would answer for itself.
func (d *daemonRun) probeInPlace() error {
	rel, err := c.ProbeRel(d.n8)
	if err != nil {
		return err
	}
	want, err := d.env.ProbeHash()
	if err != nil {
		return fmt.Errorf("the probe is not in place in the writable part (%v)", err)
	}
	id, err := d.env.Identity(rel)
	if err != nil || !id.Mode.IsRegular() || id.Mode.Perm()&0o100 == 0 {
		return fmt.Errorf("the probe is not in place in the writable part (%v)", err)
	}
	if id.Hash != want {
		return errors.New("the probe in the writable part is not the one the suite placed")
	}
	return nil
}

// covers reports whether host path a is b or holds it.
func covers(a, b string) bool {
	a, b = strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/")
	return a != "" && b != "" && (a == b || strings.HasPrefix(b, a+"/"))
}

// procNetUnixPath is the path of one /proc/net/unix line, "" for none
// (an unnamed or abstract socket, or the header).
func procNetUnixPath(l string) string {
	f := strings.Fields(l)
	if len(f) < 8 || !strings.HasPrefix(f[len(f)-1], "/") {
		return ""
	}
	return f[len(f)-1]
}

// sameSockets is a comparison of a socket the search found with those
// /proc/net/unix names, by the file's identity: it says the bound path
// that is the same socket file, or "" for none. Its control is the
// link's socket, which must be found the same as itself first; a
// comparison that cannot tell that proves nothing by finding none.
func (d *daemonRun) sameSockets(ctx context.Context, link c.SockPath, bound map[string]bool) (func(context.Context, c.SockPath) (string, error), *c.CheckResult) {
	var names []c.SockPath
	for p := range bound {
		if sp, err := c.NewSockPath(p); err == nil {
			names = append(names, sp)
		}
	}
	slices.SortFunc(names, func(a, b c.SockPath) int { return strings.Compare(a.String(), b.String()) })
	if len(names) > c.MaxBoundSockets {
		r := errorf("the sandbox binds %d sockets, more than the %d the check compares", len(names), c.MaxBoundSockets)
		return nil, &r
	}
	cmp := func(ctx context.Context, s c.SockPath) ([]string, error) {
		if len(names) == 0 {
			return nil, nil
		}
		cmd, err := c.SameSocket(s, names)
		res, err := d.sh(ctx, "root", cmd, err)
		if err != nil || res.RC != 0 {
			return nil, fmt.Errorf("the sockets could not be compared with those /proc/net/unix names: %v%s", err, msgOf(res))
		}
		var out []string
		for _, l := range lines(res.Out) {
			if l == c.SocketGone {
				return nil, fmt.Errorf("the socket %s was gone by the time it was compared with those /proc/net/unix names, so nothing is proven of it", s)
			}
			if bound[l] {
				out = append(out, l)
			}
		}
		return out, nil
	}
	ctl, err := cmp(ctx, link)
	if err != nil {
		r := errorf("%v", err)
		return nil, &r
	}
	if !slices.Contains(ctl, link.String()) {
		r := errorf("positive control failed: the link's socket %s was not found the same file as itself among those /proc/net/unix names, so the comparison cannot be trusted to find another", link)
		return nil, &r
	}
	return func(ctx context.Context, s c.SockPath) (string, error) {
		got, err := cmp(ctx, s)
		if err != nil || len(got) == 0 {
			return "", err
		}
		return got[0], nil
	}, nil
}

// noDaemonAnswer reports whether the probe connected to a socket and got
// no answer in the daemon's protocol: it said nothing, closed, or spoke
// another protocol. A hello, a refusal or any other frame the daemon's
// reader took is the daemon's protocol, from the daemon or a relay to it,
// wherever the socket is bound.
func noDaemonAnswer(o probe.Outcome) bool {
	return o.Outcome == probe.Foreign || o.Outcome == probe.Closed
}

// procNetUnixListener is the path and inode of a /proc/net/unix line of a
// listening socket (its flags __SO_ACCEPTCON), "" for any other line: an
// accepted connection shows its listener's path too.
func procNetUnixListener(l string) (string, string) {
	f := strings.Fields(l)
	p := procNetUnixPath(l)
	if p == "" || len(f) != 8 || f[3] != "00010000" {
		return "", ""
	}
	for _, r := range f[6] {
		if r < '0' || r > '9' {
			return "", ""
		}
	}
	return p, f[6]
}

// witness is the host's count of the connections the throwaway daemon
// took and refused without a frame, by its log.
type witness struct {
	d *daemonRun
}

// newWitness proves the witness first: a connection the host makes and
// closes without a hello must show in the daemon's log.
func (d *daemonRun) newWitness(ctx context.Context) (*witness, *c.CheckResult) {
	w := &witness{d}
	before, r := w.mark()
	if r != nil {
		return nil, r
	}
	if err := d.d.HostSilent(ctx); err != nil {
		r := errorf("positive control failed: %v", err)
		return nil, &r
	}
	after, r := w.after(ctx, before, "the host's own connection")
	if r != nil {
		return nil, r
	}
	if after.Refusals <= before.Refusals {
		r := errorf("positive control failed: the daemon's log did not record a connection the host closed without a hello, so it cannot witness one the sandbox made")
		return nil, &r
	}
	return w, nil
}

func (w *witness) mark() (c.DaemonLogMark, *c.CheckResult) {
	m, err := w.d.d.DaemonLog()
	if err != nil {
		r := errorf("the daemon's log could not be read: %v", err)
		return m, &r
	}
	return m, nil
}

// after is the mark once the daemon had time to log what it refused: an
// ERROR if the log was moved aside since before, which may have taken a
// refusal with it.
func (w *witness) after(ctx context.Context, before c.DaemonLogMark, what string) (c.DaemonLogMark, *c.CheckResult) {
	if err := w.d.ops.Wait(ctx, witnessWait); err != nil {
		r := errorf("%v", err)
		return before, &r
	}
	m, r := w.mark()
	if r != nil {
		return m, r
	}
	if m.RotatedSince(before) || m.Refusals < before.Refusals {
		r := errorf("the daemon's log was moved aside while %s was tried, so nothing can be told of it", what)
		return m, &r
	}
	return m, nil
}

// holders is which processes hold which sockets in the sandbox, which
// of those processes are caboose-agent, and what each one's executable
// read as.
type holders struct {
	byInode map[string][]int
	agent   map[int]bool
	exe     map[int]string
}

// agentHolds reports whether caboose-agent holds any of the inodes.
func (h *holders) agentHolds(inodes []string) bool {
	for _, ino := range inodes {
		for _, pid := range h.byInode[ino] {
			if h.agent[pid] {
				return true
			}
		}
	}
	return false
}

// agentExes are the paths caboose-agent runs from in a sandbox, as
// /proc/PID/exe reads them: where the layer installs it (a container's
// and a gVisor sandbox's link is `docker exec caboose-agent link`), and
// where a vm guest's init copies itself on the new root, which the
// guest's agent runs from and starts the link from (os.Executable).
var agentExes = []string{"/usr/local/bin/caboose-agent", agentproto.GuestAgent}

// deletedSuffix is what Linux adds to /proc/PID/exe once the file the
// process runs is no longer at its path.
const deletedSuffix = " (deleted)"

// isAgentExe reports whether exe, as /proc/PID/exe read, is one of
// agentExes: exactly, but for a trailing deletedSuffix. Any other path,
// whatever its base name, is not the agent.
func isAgentExe(exe string) bool {
	return slices.Contains(agentExes, strings.TrimSuffix(exe, deletedSuffix))
}

// maxExeText bounds an executable's path in a reason.
const maxExeText = 128

// describe says which processes hold the inodes and what each one's
// executable read as: at most 8, each path cut short and quoted in
// ASCII, since the sandbox wrote it.
func (h *holders) describe(inodes []string) string {
	var pids []int
	for _, ino := range inodes {
		for _, pid := range h.byInode[ino] {
			if !slices.Contains(pids, pid) {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) == 0 {
		return "no process was found holding it"
	}
	slices.Sort(pids)
	var out []string
	for _, pid := range pids {
		exe, ok := h.exe[pid]
		if !ok {
			out = append(out, fmt.Sprintf("pid %d (executable unread)", pid))
			continue
		}
		if len(exe) > maxExeText {
			exe = exe[:maxExeText] + "..."
		}
		out = append(out, fmt.Sprintf("pid %d (%s)", pid, strconv.QuoteToASCII(exe)))
	}
	return "held by " + strings.Join(clipList(out), ", ")
}

// socketHolders maps the sandbox's sockets to the processes holding them
// (as root), and those to their executables. Its control is the link's
// socket, which caboose-agent listens on.
func (d *daemonRun) socketHolders(ctx context.Context, link c.SockPath, listening map[string][]string) (*holders, *c.CheckResult) {
	cmd, err := c.SocketHolders(d.n8)
	res, err := d.sh(ctx, "root", cmd, err)
	if err != nil || res.RC != 0 {
		r := errorf("the sockets' processes could not be listed: %v%s", err, msgOf(res))
		return nil, &r
	}
	h := &holders{byInode: map[string][]int{}, agent: map[int]bool{}, exe: map[int]string{}}
	ls := lines(res.Out)
	exit := ""
	for _, l := range ls {
		if rest, ok := strings.CutPrefix(l, c.ScanComplete(d.n8)+" "); ok {
			exit = rest
		}
	}
	// find exits 1 for a process gone while it looked; timeout's 124 (or
	// a kill) is a list cut short.
	if exit != "0" && exit != "1" {
		r := errorf("the list of the sockets' processes did not go through (find's exit %q)", exit)
		return nil, &r
	}
	for i := 0; i+1 < len(ls); i++ {
		ino, ok1 := strings.CutPrefix(ls[i], "socket:[")
		ino, ok2 := strings.CutSuffix(ino, "]")
		if !ok1 || !ok2 {
			continue
		}
		rest, ok := strings.CutPrefix(ls[i+1], "/proc/")
		pidText, _, ok3 := strings.Cut(rest, "/fd/")
		pid, err := strconv.Atoi(pidText)
		if !ok || !ok3 || err != nil || pid <= 0 {
			continue
		}
		if !slices.Contains(h.byInode[ino], pid) {
			h.byInode[ino] = append(h.byInode[ino], pid)
		}
	}
	pids := map[int]bool{}
	for _, ino := range append(slices.Clone(listening[link.String()]), allInodes(listening)...) {
		for _, pid := range h.byInode[ino] {
			pids[pid] = true
		}
	}
	if len(pids) > maxSockets*4 {
		r := errorf("the sandbox's sockets are held by %d processes, more than the %d the check looks at", len(pids), maxSockets*4)
		return nil, &r
	}
	for _, pid := range slices.Sorted(maps.Keys(pids)) {
		cmd, err := c.ProcExe(pid)
		res, err := d.sh(ctx, "root", cmd, err)
		if err != nil {
			r := errorf("the executable of pid %d could not be read: %v", pid, err)
			return nil, &r
		}
		// A process gone since says nothing: it holds nothing now.
		if out := lines(res.Out); res.RC == 0 && len(out) > 0 {
			h.exe[pid] = out[0]
			if isAgentExe(out[0]) {
				h.agent[pid] = true
			}
		}
	}
	if inodes := listening[link.String()]; !h.agentHolds(inodes) {
		r := errorf("positive control failed: the link's socket %s was not found held by caboose-agent (%s), so a relay of its could not be told: %s",
			link, strings.Join(agentExes, " or "), h.describe(inodes))
		return nil, &r
	}
	return h, nil
}

func allInodes(m map[string][]string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k]...)
	}
	return out
}

func d2() c.Check {
	return daemonCheck("D2", "the daemon's socket is not reachable", "daemon socket unreachable", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			info, err := d.d.Daemon()
			if err != nil || !info.Running || info.Dir == "" {
				return errorf("the throwaway daemon is not running (%v)", err)
			}
			if err := d.d.HostHello(ctx); err != nil {
				return errorf("positive control failed: the daemon does not answer its own user on the host: %v", err)
			}
			for _, m := range d.env.Mounts() {
				if covers(m.Source, info.Dir) || covers(info.Dir, m.Source) {
					return failf("the mount %s of %s holds the daemon's directory, or is inside it", m.Target, m.Source)
				}
			}
			link, err := c.NewSockPath(d.d.LinkSocket())
			if err != nil {
				return errorf("the link's socket: %v", err)
			}
			if err := d.probeInPlace(); err != nil {
				return errorf("%v", err)
			}
			found := map[string]bool{}
			cmd, err := c.ListSockets(d.n8)
			res, err := d.sh(ctx, "root", cmd, err)
			if err != nil {
				return errorf("the sockets could not be listed: %v", err)
			}
			listed := lines(res.Out)
			if res.RC != 0 || !slices.Contains(listed, c.ScanComplete(d.n8)) {
				return errorf("the search for sockets did not go through (exit %d)%s", res.RC, msgOf(res))
			}
			for _, l := range listed {
				if strings.HasPrefix(l, "/") {
					found[l] = true
				}
			}
			// The control is the search's own: /proc/net/unix sees only the
			// sandbox's network namespace, never a socket file mounted in.
			if !found[link.String()] {
				return errorf("positive control failed: the search for sockets did not find the link's socket %s, so it cannot be trusted to find another", link)
			}
			res, err = d.sh(ctx, "root", c.ProcNetUnix(), nil)
			if err != nil || res.RC != 0 {
				return errorf("/proc/net/unix could not be read: %v%s", err, msgOf(res))
			}
			var r *c.CheckResult
			bound := map[string]bool{}         // bound in the sandbox's own network namespace
			listening := map[string][]string{} // a bound path's listening sockets' inodes
			for _, l := range lines(res.Out) {
				if p := procNetUnixPath(l); p != "" {
					found[p] = true
					bound[p] = true
				}
				if p, ino := procNetUnixListener(l); p != "" {
					listening[p] = append(listening[p], ino)
				}
			}
			vm := d.env.Isolation() == "vm"
			var w *witness
			var held *holders
			if vm {
				// The host's witness, before any socket is tried: the
				// daemon logs a connection it takes and refuses without
				// a frame, which a socket that said nothing may have
				// relayed to it.
				if w, r = d.newWitness(ctx); r != nil {
					return *r
				}
			}
			var socks []string
			for p := range found {
				if p != link.String() {
					socks = append(socks, p)
				}
			}
			slices.Sort(socks)
			if len(socks) > maxSockets {
				return errorf("the sandbox has %d sockets, more than the %d the check tries", len(socks), maxSockets)
			}
			var notes []string
			var same func(context.Context, c.SockPath) (string, error)
			for _, p := range socks {
				sp, err := c.NewSockPath(p)
				if err != nil {
					return errorf("a socket the sandbox has cannot be tried: %v", err)
				}
				var before c.DaemonLogMark
				if w != nil {
					if before, r = w.mark(); r != nil {
						return *r
					}
				}
				cmd, err := c.ProbeDaemon(d.n8, d.nonce, link, sp)
				res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
				if err != nil {
					return errorf("the probe could not be run on %s: %v", p, err)
				}
				outs, err := probeOutcomes(res.Out)
				if err != nil {
					return errorf("%s: %v", p, err)
				}
				if len(outs) != 1 || outs[0].Message != probe.DaemonHello {
					return errorf("the probe on %s ended %d without its line%s", p, res.RC, msgOf(res))
				}
				o := outs[0]
				if vm && noDaemonAnswer(o) {
					// Under vm no socket crosses virtio-fs: one the guest
					// binds itself (its dockerd's, its containerd's) is
					// the guest's, not the daemon, if it did not complete
					// the daemon's hello (another protocol, or silence the
					// daemon's log did not see). One caboose-agent listens
					// on is its relay to the host, never the guest's own.
					// One not bound in the guest (a socket mounted in,
					// which could wrap the daemon's) keeps the verdict it
					// has anywhere else.
					if want := map[string]int{probe.Foreign: probe.ExitHeld, probe.Closed: probe.ExitNotHeld}[o.Outcome]; res.RC != want {
						return errorf("the probe could not tell on %s (%s, exit %d): %s", p, o.Outcome, res.RC, o.Detail)
					}
					as := p
					if !bound[p] {
						// /proc/net/unix names a socket by the path it was
						// bound at (dockerd's /var/run/docker.sock), the
						// search by where it is (/run/docker.sock): the
						// same file, by device and inode, or not bound.
						if same == nil {
							if same, r = d.sameSockets(ctx, link, bound); r != nil {
								return *r
							}
						}
						own, err := same(ctx, sp)
						if err != nil {
							return errorf("%v", err)
						}
						as = own
					}
					if as == "" {
						if o.Outcome == probe.Foreign {
							return failf("the socket %s took a connection (%s: %s), and is not one the sandbox binds itself", p, o.Outcome, o.Detail)
						}
						return failf("the socket %s took a connection (%s: %s)", p, o.Outcome, o.Detail)
					}
					if held == nil {
						if held, r = d.socketHolders(ctx, link, listening); r != nil {
							return *r
						}
					}
					relay := held.agentHolds(listening[as])
					what := ""
					if relay {
						if what = d.d.Relays()[as]; what == "" {
							what = "a relay the host does not name"
						}
					}
					witnessed := ""
					if o.Outcome == probe.Closed || relay {
						// The hello said nothing, which the daemon itself
						// does when it refuses, and a relay that drops the
						// daemon's answer does too. A request before any
						// hello is logged by the daemon whenever it gets
						// there: the log tells.
						cmd, err := c.ProbeDaemonNoHello(d.n8, d.nonce, link, sp)
						res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
						if err != nil {
							return errorf("the probe could not be run on %s: %v", p, err)
						}
						outs, err := probeOutcomes(res.Out)
						if err != nil {
							return errorf("%s: %v", p, err)
						}
						if len(outs) != 1 || outs[0].Message != probe.DaemonNoHello {
							return errorf("the probe's request before a hello on %s ended %d without its line%s", p, res.RC, msgOf(res))
						}
						if n := outs[0]; n.Verdict == probe.NotHeld {
							return failf("the socket %s answered a request before a hello in the daemon's protocol (%s: %s)", p, n.Outcome, n.Detail)
						}
						if n := outs[0]; n.Verdict != probe.Held || res.RC != probe.ExitHeld {
							return errorf("the probe's request before a hello could not tell on %s (%s, exit %d): %s", p, n.Outcome, res.RC, n.Detail)
						}
						after, r := w.after(ctx, before, p)
						if r != nil {
							return *r
						}
						if after.Refusals > before.Refusals {
							witnessed = fmt.Sprintf("the daemon's log recorded %d connection(s) it refused meanwhile", after.Refusals-before.Refusals)
						}
					}
					if witnessed != "" {
						return failf("the socket %s took a connection (%s: %s), and %s: it reaches the daemon", p, o.Outcome, o.Detail, witnessed)
					}
					note := p + ": the sandbox's own, not the daemon (" + o.Outcome + ": " + o.Detail + ")"
					if relay {
						note = p + ": caboose's relay to the host (" + what + "), not the daemon (" + o.Outcome + ": " + o.Detail + ")"
					}
					if as != p {
						note += ", bound as " + as
					}
					if o.Outcome == probe.Closed || relay {
						note += "; the daemon's log recorded no connection"
					}
					notes = append(notes, note)
					continue
				}
				switch {
				case o.Verdict == probe.NotHeld:
					return failf("the socket %s took a connection (%s: %s)", p, o.Outcome, o.Detail)
				case o.Outcome == probe.Foreign:
					// Outside vm a foreign answer is a connection: a
					// socket there may be mounted in, and wrap the daemon's.
					return failf("the socket %s took a connection (%s: %s), and is not one the sandbox binds itself", p, o.Outcome, o.Detail)
				case o.Verdict != probe.Held || res.RC != probe.ExitHeld:
					return errorf("the probe could not tell on %s (%s, exit %d): %s", p, o.Outcome, res.RC, o.Detail)
				}
				notes = append(notes, p+": no connection")
			}
			after, err := d.d.Daemon()
			if err != nil || !after.Running || after.PID != info.PID || after.Started != info.Started {
				return errorf("the daemon was not the same process after the check (%v), so nothing is proven", err)
			}
			if err := d.d.HostHello(ctx); err != nil {
				return errorf("the daemon stopped answering its own user during the check: %v", err)
			}
			return pass(append([]string{fmt.Sprintf("tried %d socket(s) beside the link's own at %s, which the sandbox has by design", len(socks), link)}, notes...)...)
		})
}

// uiVariant is one reach to the web UI's port that D3 asks the launcher
// to judge.
type uiVariant struct {
	name string
	v    c.UIVariant
}

func uiVariants() []uiVariant {
	ui := strconv.Itoa(uiport.Release)
	return []uiVariant{
		{"forward_ports = " + ui, c.UIVariant{ForwardPorts: ui}},
		{"forward_ports = " + strconv.Itoa(uiport.Dev), c.UIVariant{ForwardPorts: strconv.Itoa(uiport.Dev)}},
		{"forward_ports = 7000-7200", c.UIVariant{ForwardPorts: "7000-7200"}},
		{"open_urls_allow names " + uiport.Origin(false), c.UIVariant{OpenURLsAllow: []string{uiport.Origin(false)}}},
		{"egress_allow names " + uiport.Addr(false).String(), c.UIVariant{EgressAllow: []string{uiport.Addr(false).String()}}},
		{"run_args publishes onto " + uiport.Addr(false).String(), c.UIVariant{RunArgs: []string{"--publish=" + uiport.Addr(false).String() + ":" + ui}}},
	}
}

func d3() c.Check {
	return daemonCheck("D3", "the web UI's port is not forwardable", "web UI port never forwarded", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			ctl, err := d.d.TryUI(c.UIVariant{ForwardPorts: "3000"})
			if err != nil {
				return errorf("%v", err)
			}
			if ctl.Stage != "" {
				return errorf("positive control failed: forward_ports = 3000 is refused too (%s: %s)", ctl.Stage, ctl.Message)
			}
			// D3 judges the configuration and policy layers; the link's own
			// refusal is pinned by hostlink's unit tests.
			ls := []string{"covers the configuration and policy layers; the link's own refusal is a unit test's (hostlink TestUIPortsNeverForwarded)"}
			for _, x := range uiVariants() {
				r, err := d.d.TryUI(x.v)
				switch {
				case err != nil:
					return errorf("%s: %v", x.name, err)
				case r.Stage != c.UIAtLoad && r.Stage != c.UIAtDecide:
					return failf("%s is not refused (%s)", x.name, stageOf(r))
				case !strings.Contains(r.Message, "web UI"):
					return errorf("%s is refused, but not for the web UI: %s", x.name, r.Message)
				}
				ls = append(ls, x.name+": refused at "+r.Stage)
			}
			// A vm profile takes no run_args at all (and hostile allows
			// only vm), so the sandbox under test cannot be put on the
			// host's network that way: the case does not apply.
			if d.env.Isolation() == "vm" {
				return pass(append(ls, "run_args --network host: does not apply, a vm profile takes no run_args")...)
			}
			// The one spelling the launcher takes: as two words it is
			// refused for its form alone, which says nothing of the host's
			// network.
			r, err := d.d.TryUI(c.UIVariant{RunArgs: []string{"--network=host"}})
			switch {
			case err != nil:
				return errorf("run_args --network host: %v", err)
			case r.Stage == c.UIAtLoad || r.Stage == c.UIAtDecide:
				return pass(append(ls, "run_args --network host: refused at "+r.Stage)...)
			case r.Stage == c.UIAtDoctor:
				return c.CheckResult{Status: c.Gap, Lines: ls,
					Reason: "run_args --network host is allowed under trust = fallible, warned by doctor and caboose ui, not refused (a known gap); the other reaches are refused"}
			}
			return failf("run_args --network host is accepted, and not even doctor reports it")
		})
}

func stageOf(r c.UIDecision) string {
	if r.Stage == "" {
		return "accepted"
	}
	return "only " + r.Stage + " reports it"
}

// curl's exit statuses that mean no connection was made, and those that
// mean one was made but no HTTP answer came.
var (
	curlNoConn   = []int{6, 7, 28}
	curlNoAnswer = []int{52, 56}
	httpStatusRe = regexp.MustCompile(`(?m)^` + c.UIHTTP + `([0-9]{3})$`)
)

func d4() c.Check {
	return daemonCheck("D4", "the web UI's API is refused without the token", "web UI token", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			info, err := d.d.Daemon()
			if err != nil || !info.Running {
				return errorf("the throwaway daemon is not running (%v)", err)
			}
			if info.WebAddr == "" || !info.WebOwned {
				return c.CheckResult{Status: c.Skip, Reason: "the web UI's port is not the throwaway daemon's (another daemon, the person's own included, may hold it): nothing was probed"}
			}
			i := strings.LastIndex(info.WebAddr, ":")
			port, err := strconv.Atoi(info.WebAddr[i+1:])
			if i < 0 || err != nil || !uiport.Is(port) {
				return errorf("the web UI's address %q is not the UI's port", info.WebAddr)
			}
			if st, err := d.d.UIStatic(ctx); err != nil || st != 200 {
				return errorf("positive control failed: the web UI does not serve its page to the host (%d, %v)", st, err)
			}
			addrs, err := d.d.HostAddrs()
			if err != nil {
				return errorf("the host's addresses could not be listed: %v", err)
			}
			targets := append([]string{"127.0.0.1"}, c.UIHostNames...)
			for _, a := range addrs {
				targets = append(targets, a.String())
			}
			var ls []string
			for _, t := range targets {
				line, r := d.tryUI(ctx, t, port)
				if r != nil {
					return *r
				}
				ls = append(ls, line)
			}
			return pass(ls...)
		})
}

// tryUI asks the API at host without the token, each way; a host that no
// connection reaches is asked once.
func (d *daemonRun) tryUI(ctx context.Context, host string, port int) (string, *c.CheckResult) {
	var got []string
	for _, how := range []string{c.UINoToken, c.UIWrongToken, c.UIForeign} {
		cmd, err := c.CurlUI(host, port, how)
		res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
		if err != nil {
			r := errorf("%s: %v", host, err)
			return "", &r
		}
		switch {
		case slices.Contains(curlNoConn, res.RC):
			if how == c.UINoToken {
				return host + ": no connection", nil
			}
			got = append(got, how+" no connection")
			continue
		case slices.Contains(curlNoAnswer, res.RC):
			got = append(got, how+" no HTTP answer")
			continue
		case res.RC != 0:
			r := errorf("%s %s: curl exited %d%s", host, how, res.RC, msgOf(res))
			return "", &r
		}
		m := httpStatusRe.FindStringSubmatch(res.Out)
		if m == nil {
			r := errorf("%s %s: no HTTP status printed", host, how)
			return "", &r
		}
		st, _ := strconv.Atoi(m[1])
		switch {
		case st >= 200 && st < 300:
			r := failf("%s %s: the API answered %d without the token", host, how, st)
			return "", &r
		case st != 401 && st != 403 && st != 421:
			r := errorf("%s %s: the API answered %d, which says nothing of the token", host, how, st)
			return "", &r
		}
		got = append(got, fmt.Sprintf("%s %d", how, st))
	}
	return host + ": " + strings.Join(got, ", "), nil
}

func x2() c.Check {
	return daemonCheck("X2", "the daemon trusts its own user", "daemon same-user trust (gap)", c.Observe, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			if err := d.d.HostHello(ctx); err != nil {
				return errorf("the suite could not speak to the throwaway daemon as its own user: %v", err)
			}
			return c.CheckResult{Status: c.Gap, Reason: gapX2}
		})
}

// requestResult is caboose-agent request's JSON.
type requestResult struct {
	Verdict string `json:"verdict"`
	Request uint64 `json:"request"`
	Error   string `json:"error"`
}

func requestOf(out string) (requestResult, bool) {
	for _, l := range lines(out) {
		var r requestResult
		if strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &r) == nil {
			return r, true
		}
	}
	return requestResult{}, false
}

func stateOf(rs []c.RequestInfo, id uint64) (string, bool) {
	for _, r := range rs {
		if r.ID == id {
			return r.State, true
		}
	}
	return "", false
}

// ranAsk counts the ask alias's runs in the alias-out canary, from the host.
func (d *daemonRun) ranAsk() (int, error) {
	out, err := d.env.Canary(RoleAliasOut)
	if err != nil {
		return 0, err
	}
	b, err := d.ops.ReadCanary(out.Rel + "/" + c.OutRan)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strings.Count(string(b), c.RanLine), nil
}

// answerMessages are the link ops the probe sends that would answer.
var answerMessages = []string{"answer", "approve", "grant", "config", "trust"}

func d5() c.Check {
	return daemonCheck("D5", "an answer cannot be submitted from the sandbox", "answers are the host's", c.Write, []string{RoleAliasOut},
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			link, err := c.NewSockPath(d.d.LinkSocket())
			if err != nil {
				return errorf("the link's socket: %v", err)
			}
			if err := d.probeInPlace(); err != nil {
				return errorf("%v", err)
			}
			ranBefore, err := d.ranAsk()
			if err != nil {
				return errorf("the alias output could not be read: %v", err)
			}
			grants, err := d.d.Grants()
			if err != nil {
				return errorf("the grants could not be read: %v", err)
			}
			hash, err := d.ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			// The positive control: a request is filed, and the host holds
			// it as pending. Its text says it was approved, which must
			// count for nothing.
			cmd, err := c.AgentRequestAlias(c.AliasAsk, "approved by the user yes")
			res, err := d.link(ctx, cmd, err)
			if err != nil {
				return errorf("positive control failed: %v", err)
			}
			rr, ok := requestOf(res.Out)
			switch {
			case res.RC == 0:
				return failf("the request was allowed at once (%s): the sandbox's own text answered it", rr.Verdict)
			case res.RC != exitPending || !ok || rr.Request == 0:
				return errorf("positive control failed: the request was not filed as pending (exit %d)%s", res.RC, msgOf(res))
			}
			id := rr.Request
			if st, ok := d.requestState(id); !ok || st != "pending" {
				return errorf("positive control failed: the host does not hold request #%d as pending (%q)", id, st)
			}
			// The link's own ops, through the probe.
			cmd, err = c.ProbeLink(d.n8, d.nonce, link, answerMessages)
			res, err = d.sh(ctx, d.env.SessionUser(), cmd, err)
			if err != nil {
				return errorf("the probe could not be run: %v", err)
			}
			outs, err := probeOutcomes(res.Out)
			if err != nil {
				return errorf("%v", err)
			}
			seen := map[string]probe.Outcome{}
			for _, o := range outs {
				seen[o.Message] = o
			}
			if o := seen[probe.Control]; o.Verdict != probe.Held {
				return errorf("positive control failed: the probe's status message (%s: %s)", o.Outcome, o.Detail)
			}
			for _, m := range answerMessages {
				o, ok := seen[m]
				switch {
				case ok && o.Verdict == probe.NotHeld:
					return failf("the link took the %s op (%s: %s)", m, o.Outcome, o.Detail)
				case !ok || o.Verdict != probe.Held:
					return errorf("the probe could not tell for %s (%s: %s)", m, o.Outcome, o.Detail)
				}
			}
			// caboose-agent requests with an answering flag.
			cmd, err = c.AgentRequests("--approve", strconv.FormatUint(id, 10))
			res, err = d.sh(ctx, d.env.SessionUser(), cmd, err)
			if err != nil {
				return errorf("requests --approve could not be run: %v", err)
			}
			if res.RC == 0 {
				return failf("caboose-agent requests --approve %d exited 0", id)
			}
			// A proposal asking for it.
			pf, err := c.ProposalFile(d.n8, "answer")
			if err != nil {
				return errorf("%v", err)
			}
			cmd, err = c.AppendLines(pf, "[answer]", fmt.Sprintf("request = %d", id), `scope = "always"`)
			res, err = d.sh(ctx, d.env.SessionUser(), cmd, err)
			if err != nil || res.RC != 0 {
				return errorf("positive control failed: writing %s (%v)%s", pf, err, msgOf(res))
			}
			if err := d.ops.Wait(ctx, daemonSettle); err != nil {
				return errorf("%v", err)
			}
			// The host's word.
			st, ok := d.requestState(id)
			switch {
			case !ok:
				return errorf("request #%d is gone from the host", id)
			case st != "pending" && st != "expired" && st != "denied":
				return failf("request #%d is %s on the host", id, st)
			}
			if now, err := d.d.Grants(); err != nil || now != grants {
				return failf("the host's grants changed (%d to %d, %v)", grants, now, err)
			}
			if now, err := d.ops.ConfigHash(); err != nil || now != hash {
				return failf("the host's config.toml changed")
			}
			if now, err := d.ranAsk(); err != nil || now != ranBefore {
				return failf("the alias %s ran on the host (%v)", c.AliasAsk, err)
			}
			return pass(fmt.Sprintf("request #%d stayed %s", id, st),
				"not tried: writing the request store or decisions.log, which no mount holds (D1)",
				"the web UI without the token is D4")
		})
}

func (d *daemonRun) requestState(id uint64) (string, bool) {
	rs, err := d.d.Requests()
	if err != nil {
		return "", false
	}
	return stateOf(rs, id)
}

// onlyIn is what of a b lacks, at most four, for a message.
func onlyIn(a, b []string) string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	switch {
	case len(out) == 0:
		return "none"
	case len(out) > 4:
		return strings.Join(out[:4], ", ") + fmt.Sprintf(" and %d more", len(out)-4)
	}
	return strings.Join(out, ", ")
}

func d6() c.Check {
	return daemonCheck("D6", "the sandbox cannot change its own trust, roots, aliases or egress", "the sandbox config reaches nothing of the host's", c.Write, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			hash, err := d.ops.ConfigHash()
			if err != nil {
				return errorf("config.toml could not be read: %v", err)
			}
			trust, err := d.ops.EffectiveTrust()
			if err != nil {
				return errorf("the host's trust could not be read: %v", err)
			}
			next, err := d.d.NextMounts()
			if err != nil {
				return errorf("the next launch's mounts could not be read: %v", err)
			}
			if planned, running := c.MountSet(next), c.MountSet(d.env.Mounts()); !slices.Equal(planned, running) {
				return errorf("positive control failed: the next launch's mounts are not the running sandbox's, so a change could not be told (planned only: %s; running only: %s)",
					onlyIn(planned, running), onlyIn(running, planned))
			}
			script, err := d.ops.AliasCommand(c.AliasMarker)
			if err != nil {
				return errorf("%v", err)
			}
			other := c.TrustHostile
			if trust == c.TrustHostile {
				other = c.TrustFallible
			}
			// A root of the suite's own canaries (host-only, under B), so
			// even a product that took it would mount nothing of the person's.
			evil := path.Dir(script)
			widen := []string{
				`trust = "` + other + `"`,
				"[roots.conf-" + d.n8 + "-evil]", `path = "` + evil + `"`, `writable = ["."]`,
				"[link]", "host_exec = true", `forward_ports = "1-65535"`,
				"[vm.conformance]", `egress_allow = ["0.0.0.0/0"]`,
			}
			cmd, err := c.AppendLines(c.SandboxConfigFile, widen...)
			if res, err := d.sh(ctx, d.env.SessionUser(), cmd, err); err != nil || res.RC != 0 {
				return errorf("positive control failed: writing %s (%v)%s", c.SandboxConfigFile, err, msgOf(res))
			}
			pf, err := c.ProposalFile(d.n8, "widen")
			if err != nil {
				return errorf("%v", err)
			}
			cmd, err = c.AppendLines(pf, "[root]", `path = "`+evil+`"`, `writable = ["."]`, `trust = "`+other+`"`)
			if res, err := d.sh(ctx, d.env.SessionUser(), cmd, err); err != nil || res.RC != 0 {
				return errorf("positive control failed: writing %s (%v)%s", pf, err, msgOf(res))
			}
			if err := d.ops.Wait(ctx, daemonSettle); err != nil {
				return errorf("%v", err)
			}
			if now, err := d.ops.ConfigHash(); err != nil || now != hash {
				return failf("the host's config.toml changed")
			}
			if now, err := d.ops.EffectiveTrust(); err != nil || now != trust {
				return failf("the host's trust is %q, not %q", now, trust)
			}
			now, err := d.d.NextMounts()
			if err != nil {
				return errorf("the next launch's mounts could not be read back: %v", err)
			}
			if !slices.Equal(c.MountSet(now), c.MountSet(next)) {
				return failf("the next launch's mounts changed")
			}
			line := "doctor's output does not name sandbox.toml"
			if out, err := d.d.Doctor(ctx); err == nil && strings.Contains(out, "sandbox.toml") {
				line = "doctor names sandbox.toml"
			}
			return pass(line, "egress_allow, host_exec and forward_ports were judged on the next launch's plan and the running trust only")
		})
}

// uiURLs are the spellings of the web UI's address a URL may use.
func uiURLs() []string {
	r, dv := strconv.Itoa(uiport.Release), strconv.Itoa(uiport.Dev)
	return []string{
		"http://127.0.0.1:" + r + "/", "http://127.0.0.1:" + dv + "/", "http://localhost:" + r + "/",
		"http://[::1]:" + r + "/#token=x", "http://127.1:" + r, "http://2130706433:" + r + "/",
		"http://0x7f000001:" + r + "/", "http://0177.0.0.1:" + r + "/", "http://caboose.localhost:" + r + "/",
		"http://localhost:" + dv + "/",
	}
}

// harmlessURL is the positive control's URL: it reaches only the stub.
const harmlessURL = "http://example.invalid/"

func d7() c.Check {
	return daemonCheck("D7", "a URL cannot open the web UI", "URLs to the web UI refused", c.Write, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			trust, err := d.ops.EffectiveTrust()
			if err != nil {
				return errorf("the host's trust could not be read: %v", err)
			}
			if trust != c.TrustFallible {
				return c.CheckResult{Status: c.Skip, Reason: "trust is hostile, which asks before every URL: the positive control would file a real request, and the ask path is not exercised"}
			}
			mode, err := d.d.OpenURLs()
			if err != nil {
				return errorf("open_urls could not be read: %v", err)
			}
			if mode != "allow" {
				return c.CheckResult{Status: c.Skip, Reason: fmt.Sprintf("the throwaway configuration's open_urls is %q, not allow, so the positive control could not reach the stub: nothing was sent", mode)}
			}
			before, err := d.d.StubOpened()
			if err != nil {
				return errorf("the stub opener could not be read: %v", err)
			}
			cmd, err := c.AgentOpen(harmlessURL)
			res, err := d.link(ctx, cmd, err)
			if err != nil || res.RC != 0 {
				return errorf("positive control failed: opening %s (%v)%s", harmlessURL, err, msgOf(res))
			}
			now, err := d.d.StubOpened()
			if err != nil || len(now) != len(before)+1 || now[len(now)-1] != harmlessURL {
				return errorf("positive control failed: the stub opener did not record %s, so a real browser might be asked: nothing more was sent", harmlessURL)
			}
			before = now
			for _, u := range uiURLs() {
				cmd, err := c.AgentOpen(u)
				res, err := d.link(ctx, cmd, err)
				now, serr := d.d.StubOpened()
				switch {
				case serr != nil:
					return errorf("the stub opener could not be read back: %v", serr)
				case len(now) != len(before):
					return failf("%s reached the opener", u)
				case err != nil:
					return errorf("%s: %v", u, err)
				case res.RC == 0:
					return failf("opening %s exited 0", u)
				}
			}
			return pass("not tried: a redirect, which the browser follows after the host has decided the URL it was given; the stub stands in for the browser")
		})
}

// floodCount is the number a flood printed: how many were refused for
// their rate.
func floodCount(res c.ExecResult) (int, error) {
	ls := lines(res.Out)
	if len(ls) == 0 {
		return 0, errors.New("the flood printed nothing")
	}
	return strconv.Atoi(ls[len(ls)-1])
}

func (d *daemonRun) flood(ctx context.Context, what string, one c.Cmd, err error) (int, *c.CheckResult) {
	if err != nil {
		r := errorf("the %s flood could not be built: %v", what, err)
		return 0, &r
	}
	cmd, err := c.Flood(floodN, one)
	res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
	if err != nil {
		r := errorf("the %s flood could not be run: %v", what, err)
		return 0, &r
	}
	n, err := floodCount(res)
	if err != nil {
		r := errorf("the %s flood: %v", what, err)
		return 0, &r
	}
	if n == 0 {
		r := failf("none of %d %s requests sent at once was refused for its rate", floodN, what)
		return 0, &r
	}
	return n, nil
}

func d8() c.Check {
	return daemonCheck("D8", "request flooding is bounded", "link requests rate-limited", c.Write, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			info, err := d.d.Daemon()
			if err != nil || !info.Running {
				return errorf("the throwaway daemon is not running (%v)", err)
			}
			rss, err := d.d.DaemonRSS()
			if err != nil || rss <= 0 {
				return errorf("the daemon's memory could not be read: %v", err)
			}
			notify := func() (c.Cmd, error) { return c.AgentNotify("conf-" + d.n8 + " D8") }
			// The positive control: one notification reaches the stub.
			calls, err := d.ops.StubCalls()
			if err != nil {
				return errorf("the stubs could not be read: %v", err)
			}
			cmd, err := notify()
			if res, err := d.link(ctx, cmd, err); err != nil || res.RC != 0 {
				return errorf("positive control failed: notify did not work (%v)%s", err, msgOf(res))
			}
			if now, err := d.ops.StubCalls(); err != nil || now != calls+1 {
				return errorf("positive control failed: the stub notifier did not record the notification, so a flood could reach the person")
			}
			var ls []string
			calls++
			one, err := notify()
			n, r := d.flood(ctx, "notify", one, err)
			if r != nil {
				return *r
			}
			now, err := d.ops.StubCalls()
			if err != nil {
				return errorf("the stubs could not be read back: %v", err)
			}
			if got := now - calls; got > floodMax {
				return failf("%d of %d notifications sent at once got through", got, floodN)
			}
			ls = append(ls, fmt.Sprintf("notify: %d of %d refused for their rate, %d shown", n, floodN, now-calls))
			if err := d.ops.Wait(ctx, refill); err != nil {
				return errorf("%v", err)
			}
			reqs, err := d.d.Requests()
			if err != nil {
				return errorf("the requests could not be read: %v", err)
			}
			one, err = c.AgentRequestAlias(c.AliasAsk, "conf-"+d.n8+" D8 flood")
			n, r = d.flood(ctx, "request", one, err)
			if r != nil {
				return *r
			}
			after, err := d.d.Requests()
			if err != nil {
				return errorf("the requests could not be read back: %v", err)
			}
			if got := len(after) - len(reqs); got > floodRequestsMax {
				return failf("%d requests sent at once filed %d requests on the host", floodN, got)
			}
			ls = append(ls, fmt.Sprintf("request: %d of %d refused for their rate, %d filed", n, floodN, len(after)-len(reqs)))
			if mode, err := d.d.OpenURLs(); err == nil && mode == "allow" {
				if err := d.ops.Wait(ctx, refill); err != nil {
					return errorf("%v", err)
				}
				opened, err := d.d.StubOpened()
				if err != nil {
					return errorf("the stub opener could not be read: %v", err)
				}
				one, err = c.AgentOpen(harmlessURL)
				n, r = d.flood(ctx, "open", one, err)
				if r != nil {
					return *r
				}
				nowOpened, err := d.d.StubOpened()
				if err != nil {
					return errorf("the stub opener could not be read back: %v", err)
				}
				if got := len(nowOpened) - len(opened); got > floodMax {
					return failf("%d of %d URLs sent at once reached the opener", got, floodN)
				}
				ls = append(ls, fmt.Sprintf("open: %d of %d refused for their rate", n, floodN))
			} else {
				ls = append(ls, "open: not flooded (open_urls is not allow)")
			}
			ls = append(ls, "connect: not flooded (its own bucket, which the egress checks meet)")
			// A later request is answered, and the daemon is the same.
			if err := d.ops.Wait(ctx, refill); err != nil {
				return errorf("%v", err)
			}
			calls, err = d.ops.StubCalls()
			if err != nil {
				return errorf("the stubs could not be read: %v", err)
			}
			cmd, err = notify()
			res, err := d.link(ctx, cmd, err)
			if now, serr := d.ops.StubCalls(); err != nil || serr != nil || res.RC != 0 || now != calls+1 {
				return failf("a request after the flood was not answered (%v)%s", err, msgOf(res))
			}
			later, err := d.d.Daemon()
			if err != nil || !later.Running || later.PID != info.PID || later.Started != info.Started {
				return failf("the daemon did not stay up under the flood (%v)", err)
			}
			rssAfter, err := d.d.DaemonRSS()
			if err != nil {
				return errorf("the daemon's memory could not be read back: %v", err)
			}
			if rssAfter > rssFactor*rss+rssSlack {
				return failf("the daemon's memory grew from %d to %d bytes", rss, rssAfter)
			}
			if err := d.ops.StubsInPlace(); err != nil {
				return errorf("the stubs left their place during the flood: %v", err)
			}
			return pass(append(ls, fmt.Sprintf("memory %d MiB before, %d MiB after", rss>>20, rssAfter>>20))...)
		})
}

func d9() c.Check {
	return daemonCheck("D9", "the sandbox cannot kill or restart the daemon", "daemon out of the sandbox's reach", c.Write, nil,
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			info, err := d.d.Daemon()
			if err != nil || !info.Running || info.PID <= 1 {
				return errorf("the throwaway daemon is not running (%v)", err)
			}
			cmd, err := c.KillZero(1)
			if res, err := d.sh(ctx, "root", cmd, err); err != nil || res.RC != 0 {
				return errorf("positive control failed: the sandbox's root cannot signal its own pid 1 (%v)%s", err, msgOf(res))
			}
			var ls []string
			cmd, err = c.KillZero(info.PID)
			res, err := d.sh(ctx, "root", cmd, err)
			if err != nil {
				return errorf("%v", err)
			}
			switch {
			case res.RC == 0:
				cmd, err := c.ProcCmdline(info.PID)
				res, err := d.sh(ctx, "root", cmd, err)
				if err != nil {
					return errorf("%v", err)
				}
				args := strings.ReplaceAll(res.Out, "\x00", " ")
				if strings.Contains(args, "caboose") && strings.Contains(args, "daemon") {
					return failf("the daemon's process is visible in the sandbox as pid %d", info.PID)
				}
				ls = append(ls, fmt.Sprintf("pid %d in the sandbox is another process; not signalled", info.PID))
			case strings.Contains(res.Out, "No such process"):
				cmd, err := c.KillTerm(info.PID)
				res, err := d.sh(ctx, "root", cmd, err)
				if err != nil {
					return errorf("%v", err)
				}
				if res.RC == 0 {
					return failf("the sandbox's root signalled pid %d, the daemon's", info.PID)
				}
				ls = append(ls, fmt.Sprintf("pid %d: no such process in the sandbox", info.PID))
			default:
				ls = append(ls, fmt.Sprintf("pid %d: not signalled%s", info.PID, msgOf(res)))
			}
			if err := d.ops.Wait(ctx, daemonSettle); err != nil {
				return errorf("%v", err)
			}
			after, err := d.d.Daemon()
			if err != nil || !after.Running || after.PID != info.PID || after.Started != info.Started {
				return failf("the daemon's process changed (pid %d to %d, %v)", info.PID, after.PID, err)
			}
			return pass(append(ls, "not tried: ptrace, which needs a process the sandbox can see")...)
		})
}

func x1() c.Check {
	return daemonCheck("X1", "the sandbox's home holds what Claude Code keeps, readable and writable", "kept home (gap)", c.Write, []string{RoleX1},
		func(ctx context.Context, d *daemonRun) c.CheckResult {
			cn, err := d.env.Canary(RoleX1)
			if err != nil {
				return errorf("canary %s is missing: %v", RoleX1, err)
			}
			hp, err := c.HomePath(cn)
			if err != nil {
				return errorf("%v", err)
			}
			before, err := d.env.Snapshot(cn)
			if err != nil || len(before) == 0 {
				return errorf("canary %s is not on the host", RoleX1)
			}
			cmd, err := c.ReadHome(hp)
			res, err := d.sh(ctx, d.env.SessionUser(), cmd, err)
			if err != nil {
				return errorf("%v", err)
			}
			if res.Class == c.ClassEnoent || res.Class == c.ClassOther && res.RC != 0 {
				return errorf("reading %s ended %s%s, which says nothing", hp, res.Class, msgOf(res))
			}
			readable := res.RC == 0 && strings.Contains(res.Out, "caboose-conformance "+d.nonce)
			cmd, err = c.AppendHome(hp)
			res, err = d.sh(ctx, d.env.SessionUser(), cmd, err)
			if err != nil {
				return errorf("%v", err)
			}
			after, err := d.env.Snapshot(cn)
			if err != nil {
				return errorf("canary %s could not be read back: %v", RoleX1, err)
			}
			writable := res.RC == 0 && changed(before, after)
			if res.RC != 0 && (res.Class == c.ClassEnoent || res.Class == c.ClassOther) {
				return errorf("appending to %s ended %s%s, which says nothing", hp, res.Class, msgOf(res))
			}
			if readable && writable {
				return c.CheckResult{Status: c.Gap, Reason: gapX1Open}
			}
			return c.CheckResult{Status: c.Gap, Reason: gapX1Closed, Lines: []string{fmt.Sprintf("read: %v, written: %v", readable, writable)}}
		})
}
