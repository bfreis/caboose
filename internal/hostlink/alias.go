package hostlink

// Aliases: the host's config.toml names commands ([alias.NAME]) a session
// may have run here, `caboose-agent run NAME [KEY=VALUE...]`. What the
// sandbox sends is a name and enum arguments, never a command: the host
// looks the alias up, substitutes the arguments into the argv the alias
// fixes, resolves its command, directory and absolute arguments on this
// disk at every run, and asks the link's Decider, which refuses a command
// within (or reached through) anything a sandbox can write. What runs is
// the resolved path that was decided, with the argv the alias gives,
// through no shell, with no terminal, as the user the link runs as, with
// only PATH, HOME and LANG, in the alias's directory or a fresh empty one
// that is removed afterwards. It lives only while its client does, and
// its timeout; whatever it leaves running in its group goes with it.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/saferm"
)

const (
	// aliasEvery and aliasBurst bound how often the sandbox may ask to
	// run an alias: each is looked up, resolved and decided.
	aliasEvery = 250 * time.Millisecond
	aliasBurst = 20
	// maxSymlinkHops is the most symlinks a path's resolution follows,
	// as the kernel's own limit.
	maxSymlinkHops = 40
)

// aliasState is a Host's bucket for alias runs.
type aliasState struct {
	tokens float64
	last   time.Time
}

// errNoAliases is a request for an alias on a link that runs none.
var errNoAliases = refuse(agentproto.AliasUnavailable, "this host runs no aliases: its link has no broker to decide them")

// aliasesOn reports whether the link offers aliases: it needs a broker to
// decide them and a place for their fresh directories.
func (h *Host) aliasesOn() bool { return h.cfg.Decide != nil && h.cfg.AliasTmp != "" }

// takeAlias takes a token from the alias bucket.
func (h *Host) takeAlias() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &h.alias
	now := time.Now()
	if g.last.IsZero() {
		g.tokens = aliasBurst
	} else {
		g.tokens = min(aliasBurst, g.tokens+now.Sub(g.last).Seconds()/aliasEvery.Seconds())
	}
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// aliases serves OpAliases into r: the aliases' names, descriptions and
// parameters, never their commands.
func (h *Host) aliases(r *agentproto.Message) error {
	if !h.aliasesOn() {
		return errNoAliases
	}
	if !h.takeList() {
		return errors.New("too many requests; try again shortly")
	}
	list, err := h.cfg.Decide.Aliases()
	if err != nil {
		return refuse(agentproto.AliasUnavailable, "the host cannot list its aliases now: %s", clip(err.Error(), 200))
	}
	for _, a := range list {
		if len(r.Aliases) == agentproto.MaxAliases {
			break
		}
		params := map[string][]string{}
		for k, v := range a.Params {
			params[k] = slices.Clone(v)
		}
		if len(params) == 0 {
			params = nil
		}
		r.Aliases = append(r.Aliases, agentproto.AliasInfo{Name: a.Name, Description: clip(proposal.Printable(a.Description), 300),
			Params: params, Asks: a.Asks})
	}
	return nil
}

// aliasRun serves OpAliasRun id: the alias x names decided, with the facts
// the host resolves for it, and a stream for its client when it may run;
// or why it does not, as a refusal whose reason is one of the Alias*
// codes.
func (h *Host) aliasRun(id uint64, x *agentproto.ExecRequest) error {
	if !h.aliasesOn() {
		return errNoAliases
	}
	if !h.takeAlias() {
		return refuse(agentproto.AliasUnavailable, "too many alias runs; try again shortly")
	}
	if x == nil {
		return refuse(agentproto.AliasDenied, "the request names no alias")
	}
	name, args, err := agentproto.ParseAliasArgv(x.Argv)
	if err != nil {
		return refuse(agentproto.AliasDenied, "%s", err)
	}
	what := "alias " + name
	a, ok, err := h.cfg.Decide.Alias(name)
	if err != nil {
		h.execLogf("%s: refused: %s", what, clip(err.Error(), 300))
		return refuse(agentproto.AliasUnavailable, "the host cannot run aliases now: %s", clip(err.Error(), 200))
	}
	if !ok {
		// Decided all the same, for the decisions log; the policy has
		// no such alias, and refuses.
		o, _ := h.cfg.Decide.Decide(policy.Request{Kind: policy.KindAlias, Alias: name, Args: args})
		h.execLogf("%s: refused: no such alias", what)
		return refuse(agentproto.AliasUnknown, "%s", o.Reason)
	}
	run, req, err := resolveAlias(a, args)
	if err != nil {
		// Decided all the same, for the decisions log: the facts the
		// policy needs are missing, and it refuses.
		_, _ = h.cfg.Decide.Decide(req)
		h.execLogf("%s: refused: %s", what, clip(proposal.Printable(err.Error()), 300))
		return err
	}
	o, err := h.cfg.Decide.Decide(req)
	switch {
	case err != nil && o.Verdict == "":
		return refuse(agentproto.AliasUnavailable, "%s", clip(err.Error(), 300))
	case o.Verdict == broker.Pending:
		h.execLogf("%s: pending #%d", what, o.Request)
		return &refusal{reason: agentproto.AliasPending, request: o.Request,
			msg: fmt.Sprintf("pending #%d: %s; it runs once a person on the host allows it, when asked again", o.Request, o.Reason)}
	case o.Verdict != broker.Allowed:
		h.execLogf("%s: refused: %s", what, clip(o.Reason, 300))
		return refuse(agentproto.AliasDenied, "%s", o.Reason)
	}
	asked := slices.Clone(x.Argv)
	return h.openExec(id, what, func(s *agentproto.Session, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (int, string, error) {
		return h.runAlias(s, run, asked, req, streams)
	})
}

// aliasCmd is an alias as it is to run: decided, every path resolved.
type aliasCmd struct {
	// path is the command's resolved path, what is executed; argv its
	// arguments, argv[0] the alias's command[0] as configured.
	path string
	argv []string
	// dir is the resolved directory it runs in, "" for a fresh one.
	dir     string
	timeout time.Duration
}

// resolveAlias is what alias a runs as with args, and the request that
// decides it: every path the policy compares resolved on this disk now,
// with each symlink followed on the way and the identity of each path and
// every directory above it. An error is a refusal saying why it cannot
// run (an argument the alias does not take, a command missing here).
func resolveAlias(a policy.Alias, args map[string]string) (aliasCmd, policy.Request, error) {
	req := policy.Request{Kind: policy.KindAlias, Alias: a.Name, Args: args}
	argv, err := policy.Substitute(a, args)
	if err != nil {
		return aliasCmd{}, req, refuse(agentproto.AliasDenied, "alias %q: %s", a.Name, err)
	}
	if a.Policy == policy.AliasDeny {
		// Refused whatever its command: no need to look for it.
		return aliasCmd{}, req, refuse(agentproto.AliasDenied, `alias %q is refused: policy = "deny" in [alias.%s] of config.toml on the host`, a.Name, a.Name)
	}
	cmd, via, err := ResolvePath(argv[0], false)
	if err != nil {
		return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its command %s is missing on the host, or cannot be looked at (%s)",
			a.Name, clip(proposal.Printable(argv[0]), 200), clip(proposal.Printable(errText(err)), 200))
	}
	if fi, err := os.Stat(cmd); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its command %s is no executable file on the host", a.Name, clip(proposal.Printable(cmd), 200))
	}
	// A script runs its #! interpreter: that is decided as the command
	// is, symlinks and all.
	if interp := Interpreter(cmd); interp != "" {
		ipath, ivia, err := ResolvePath(interp, false)
		if err != nil {
			return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its command's interpreter %s is missing on the host", a.Name, clip(proposal.Printable(interp), 200))
		}
		via = append(append(via, ipath), ivia...)
	}
	req.Command, req.CommandVia = cmd, via
	req.CommandIDs = idChain(cmd)
	for _, v := range via {
		req.CommandIDs = append(req.CommandIDs, idChain(v)...)
	}
	run := aliasCmd{path: cmd, argv: argv, timeout: a.Timeout}
	if a.Cwd != "" {
		dir, via, err := ResolvePath(a.Cwd, false)
		if err != nil {
			return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its cwd %s is missing on the host (%s)",
				a.Name, clip(proposal.Printable(a.Cwd), 200), clip(proposal.Printable(errText(err)), 200))
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its cwd %s is no directory on the host", a.Name, clip(proposal.Printable(dir), 200))
		}
		req.Cwd, run.dir = dir, dir
		req.CwdIDs = idChain(dir)
		for _, v := range via {
			req.CwdIDs = append(req.CwdIDs, idChain(v)...)
		}
	}
	for _, e := range argv[1:] {
		if !strings.HasPrefix(e, "/") {
			continue
		}
		p, via, err := ResolvePath(e, true)
		if err != nil {
			return aliasCmd{}, req, refuse(agentproto.AliasUnavailable, "alias %q: its argument %s cannot be looked at on the host (%s)",
				a.Name, clip(proposal.Printable(e), 200), clip(proposal.Printable(errText(err)), 200))
		}
		req.ArgPaths = append(req.ArgPaths, p)
		req.ArgIDs = append(req.ArgIDs, idChain(p)...)
		for _, v := range via {
			req.ArgIDs = append(req.ArgIDs, idChain(v)...)
		}
	}
	return run, req, nil
}

// errText is err's own words, without the path an *fs.PathError adds.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// ResolvePath is p, an absolute path, with every symlink in it followed
// as the kernel follows them, component by component: the physical path,
// and each symlink met on the way, as the path it was met at. With
// missing, a path whose end does not exist resolves as far as it does,
// the rest joined on as it is. Nothing is cleaned before it is walked: a
// ".." is taken from where the walk is, as the kernel takes it.
func ResolvePath(p string, missing bool) (string, []string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", nil, fmt.Errorf("%q is not an absolute path", p)
	}
	var via []string
	done := "/"
	rest := strings.Split(p, "/")
	hops := 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			done = filepath.Dir(done)
			continue
		}
		next := filepath.Join(done, c)
		fi, err := os.Lstat(next)
		if err != nil {
			if missing && errors.Is(err, fs.ErrNotExist) {
				return filepath.Join(append([]string{next}, rest...)...), via, nil
			}
			return "", via, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if hops++; hops > maxSymlinkHops {
				return "", via, &fs.PathError{Op: "resolve", Path: p, Err: syscall.ELOOP}
			}
			t, err := os.Readlink(next)
			if err != nil {
				return "", via, err
			}
			via = append(via, next)
			if strings.HasPrefix(t, "/") {
				done = "/"
			}
			rest = append(strings.Split(t, "/"), rest...)
			continue
		}
		if !fi.IsDir() && slices.ContainsFunc(rest, func(s string) bool { return s != "" && s != "." }) {
			return "", via, &fs.PathError{Op: "resolve", Path: next, Err: syscall.ENOTDIR}
		}
		done = next
	}
	return done, via, nil
}

// idChain is the identity of p and of every directory above it, as each
// is now, not following a symlink at p itself; what cannot be looked at
// is left out.
func idChain(p string) []policy.FileID {
	var ids []policy.FileID
	for {
		if fi, err := os.Lstat(p); err == nil {
			if id, ok := policy.IDOf(fi); ok {
				ids = append(ids, id)
			}
		}
		up := filepath.Dir(p)
		if up == p {
			return ids
		}
		p = up
	}
}

// runAlias runs run for the client of session s, which asked for asked
// (the argv of the request decided) and now sends req on the exec
// protocol: the two must be the one request. It runs in run's directory,
// or a fresh one made under the link's AliasTmp and removed once the
// command and what it left are gone.
func (h *Host) runAlias(s *agentproto.Session, run aliasCmd, asked []string, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (int, string, error) {
	closeAll := func() {
		for _, st := range streams {
			_ = st.CloseWrite()
		}
	}
	switch {
	case !slices.Equal(req.Argv, asked):
		closeAll()
		return agentproto.ExitCannotRun, "", errors.New("the alias run asks for another alias than the one decided")
	case req.TTY:
		closeAll()
		return agentproto.ExitCannotRun, "", errors.New("an alias gets no terminal")
	case req.User != "":
		closeAll()
		return agentproto.ExitCannotRun, "", errors.New("an alias runs as the user the link runs as, and takes no other")
	case len(req.Env) > 0:
		closeAll()
		return agentproto.ExitCannotRun, "", errors.New("an alias gets only PATH, HOME and LANG, and takes no variables")
	}
	dir := run.dir
	if dir == "" {
		tmp, err := h.aliasDir()
		if err != nil {
			closeAll()
			return agentproto.ExitCannotRun, "", fmt.Errorf("no fresh directory to run it in on the host: %v", err)
		}
		defer func() {
			if err := saferm.RemoveTree(h.cfg.AliasTmp, tmp); err != nil && h.cfg.Log != nil {
				h.cfg.Log.Printf("alias: cannot remove its directory %s: %v", tmp, err)
			}
		}()
		dir = tmp
	}
	cmd := &exec.Cmd{Path: run.path, Args: run.argv, Dir: dir, Env: HostExecEnv(os.Getenv),
		// Its own group, so a kill reaches what it started too.
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	code, err := runCmd(s, cmd, streams, h.cfg.Execs, runLimits{timeout: run.timeout, noRemains: true})
	return code, dir, err
}

// aliasDir makes a fresh, empty directory for one alias run, under the
// link's AliasTmp (made, 0700, when missing).
func (h *Host) aliasDir() (string, error) {
	parent := h.cfg.AliasTmp
	if !filepath.IsAbs(parent) {
		return "", fmt.Errorf("%q is not an absolute path", parent)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "run-")
}

// Interpreter is the absolute interpreter path of path's #! line, when it
// is a script with one; "" otherwise. A relative one is not run by the
// kernel, so there is none to decide.
func Interpreter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line, ok := strings.CutPrefix(string(buf[:n]), "#!")
	if !ok {
		return ""
	}
	line, _, _ = strings.Cut(line, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	return fields[0]
}
