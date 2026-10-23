package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/nofollow"
)

// The real Host's calls for the host-command, trust, egress and
// checkpoint checks (conformance.HostOps, EgressOps,
// CheckpointOps): each through the launcher's own code, in this process,
// against the suite's environment under B. None starts a session or
// Claude Code, attaches a terminal or reaches anything outside B.

var (
	_ conformance.HostOps       = (*confHost)(nil)
	_ conformance.EgressOps     = (*confHost)(nil)
	_ conformance.CheckpointOps = (*confHost)(nil)
)

// Bounds of what the calls read and how long they wait.
const (
	confConfigMax    = 1 << 20
	confLogMax       = 32 << 20
	confSettleWait   = 60 * time.Second
	confSettlePoll   = 250 * time.Millisecond
	confLinkStateMax = 64 << 10
)

// confConfigPath is the environment's config.toml, relative to B.
func (h *confHost) configRel() string { return confConfigRel(h.G.Env()) }

func (h *confHost) readConfig() ([]byte, error) {
	b, _, err := nofollow.Dir(h.G.Base()).ReadFileMax(h.configRel(), confConfigMax)
	return b, err
}

// app is the suite's App with its configuration read again from
// config.toml, as the next command would read it: what a check changed
// is in it.
func (h *confHost) app() (*App, error) {
	if h.a == nil {
		return nil, errors.New("conformance: the environment's App was not built")
	}
	cfg, err := config.Load(h.a.Cfg.Getenv, config.OSFS{}, h.a.Cfg.Env)
	if err != nil {
		return nil, err
	}
	return h.appWith(cfg)
}

func (h *confHost) appWith(cfg *config.Config) (*App, error) {
	if cfg.Env != h.G.Env() || cfg.CabooseHome != h.G.CabooseHome() || cfg.DataDir != h.G.DataDir() {
		return nil, errors.New("conformance: the configuration read is not the suite's environment's")
	}
	if err := cfg.ResolveRoots(); err != nil {
		return nil, err
	}
	return &App{Cfg: cfg, Docker: h.a.Docker, Backend: h.a.Backend, Checkout: h.a.Checkout, Stdout: io.Discard, Stderr: io.Discard,
		Spawn: h.a.Spawn, DaemonDial: h.a.DaemonDial, Executable: h.a.Executable}, nil
}

// readLinkState is link.json, written by the running link, with what it
// decides by; an error when no link runs.
func (h *confHost) readLinkState() (linkState, error) {
	dataDir := h.G.DataDir()
	if held, err := linkRunning(dataDir); err != nil || !held {
		return linkState{}, fmt.Errorf("no link is running for the environment (%v)", err)
	}
	b, _, err := nofollow.Dir(dataDir).ReadFileMax(linkStateFile, confLinkStateMax)
	if err != nil {
		return linkState{}, err
	}
	var st linkState
	if err := json.Unmarshal(b, &st); err != nil {
		return linkState{}, fmt.Errorf("%s: %v", linkStateFile, err)
	}
	return st, nil
}

// refusal is a launcher decision's error as a LaunchResult.
func refusal(err error) conformance.LaunchResult {
	if err == nil {
		return conformance.LaunchResult{}
	}
	msg := err.Error()
	var ee *ExitError
	if errors.As(err, &ee) {
		msg = ee.Msg
	}
	return conformance.LaunchResult{Refused: true, Message: msg}
}

// --- HostOps

func (h *confHost) StubsInPlace() error {
	if err := h.G.CheckStubs(); err != nil {
		return err
	}
	if h.daemon == nil {
		return errors.New("the suite has no daemon of its own")
	}
	if err := h.daemon.stubsFirst(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonStatusBudget)
	defer cancel()
	return h.daemon.check(ctx, h.a)
}

func (h *confHost) StubCalls() (int, error) { return h.G.StubCalls() }

func (h *confHost) ConfigHash() (string, error) {
	b, err := h.readConfig()
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func (h *confHost) AliasCommand(name string) (string, error) {
	for _, al := range confSettingsFor(h.G).Aliases {
		if al.Name == name && len(al.Command) > 0 {
			return al.Command[0], nil
		}
	}
	return "", fmt.Errorf("conformance: no alias %q in the suite", name)
}

func (h *confHost) EnvCanary() string { return confEnvCanary(h.G) }

func (h *confHost) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// EffectiveTrust is the trust the running link decides by, as it wrote it
// into link.json when it started or last reread config.toml.
func (h *confHost) EffectiveTrust() (string, error) {
	st, err := h.readLinkState()
	if err != nil {
		return "", err
	}
	if st.Trust == "" {
		return "", errors.New("the running link's link.json names no trust")
	}
	return st.Trust, nil
}

// confPorts is egress_ports as the suite writes it through
// config.EditFile, whose read-back check takes a string list alone: each
// port as the one-port range "N-N", which the launcher reads as port N.
func confPorts(ports []int) []string {
	out := make([]string, len(ports))
	for i, n := range ports {
		out[i] = strconv.Itoa(n) + "-" + strconv.Itoa(n)
	}
	return out
}

// confOverlay is the filesystem with one file's contents replaced: a copy
// of config.toml, read by the launcher's own loader, written nowhere.
type confOverlay struct {
	config.OSFS
	path string
	data []byte
}

func (o confOverlay) ReadFile(p string) ([]byte, error) {
	if filepath.Clean(p) == o.path {
		return bytes.Clone(o.data), nil
	}
	return o.OSFS.ReadFile(p)
}

// loadEdited loads a copy of config.toml with e applied, as config.EditFile
// applies it and config.CheckEdit confirms it, with the launcher's loader.
func (h *confHost) loadEdited(e config.Edit) (*config.Config, error) {
	orig, err := h.readConfig()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(h.G.Base(), filepath.FromSlash(h.configRel()))
	edited := config.EditFile(orig, e)
	if err := config.CheckEdit(p, orig, edited, e); err != nil {
		return nil, err
	}
	return config.Load(h.a.Cfg.Getenv, confOverlay{path: p, data: edited}, h.a.Cfg.Env)
}

// variantEdit is the change ConfigVariant v makes.
func variantEdit(v conformance.ConfigVariant) (config.Edit, error) {
	if !slices.Contains(config.Kinds, v.Isolation) || (v.Trust != config.TrustFallible && v.Trust != config.TrustHostile) {
		return config.Edit{}, fmt.Errorf("conformance: no variant %+v", v)
	}
	prof := v.Isolation + "." + confProfile
	e := config.Edit{Tables: []string{prof}, Set: map[string]any{
		"trust": v.Trust, "isolation": prof, "link.host_exec": v.HostExec,
	}}
	if v.Egress != nil {
		if v.Isolation != config.KindVM {
			return config.Edit{}, errors.New("conformance: egress is a vm profile's key")
		}
		e.Set[prof+".egress"] = *v.Egress
	}
	if v.Isolation == config.KindVM && v.Trust == config.TrustHostile {
		// A hostile launch refuses until egress_allow names Claude
		// Code's hosts: the variant is one that would launch.
		e.Set[prof+".egress_allow"] = slices.Clone(claudeHosts)
	}
	return e, nil
}

func (h *confHost) TryConfig(v conformance.ConfigVariant) error {
	e, err := variantEdit(v)
	if err != nil {
		return err
	}
	_, err = h.loadEdited(e)
	return err
}

// HostExecOffered is what the link's hello would offer under v: the
// settings a link reads (settingsOf) parsed as it parses them (check).
func (h *confHost) HostExecOffered(v conformance.ConfigVariant) (bool, string, error) {
	e, err := variantEdit(v)
	if err != nil {
		return false, "", err
	}
	cfg, err := h.loadEdited(e)
	if err != nil {
		return false, "", err
	}
	lc, err := settingsOf(cfg).check()
	if err != nil {
		return false, "", err
	}
	switch {
	case lc.hostExec:
		return true, "", nil
	case !cfg.HostExec:
		return false, "host_exec is off", nil
	default:
		return false, fmt.Sprintf("trust is %s, under which the hello offers no host exec", cfg.Trust), nil
	}
}

// SSHAgentCarried is the agent a link under v, with ssh_agent unset,
// would carry: agentFrom on the settings a link reads (settingsOf), on a
// copy of config.toml, with the host's $SSH_AUTH_SOCK standIn and an ssh
// that names no IdentityAgent. No ssh runs, no ssh config of this
// machine's is read, and nothing is dialled.
func (h *confHost) SSHAgentCarried(v conformance.ConfigVariant, standIn string) (string, string, error) {
	e, err := variantEdit(v)
	if err != nil {
		return "", "", err
	}
	e.Unset = append(e.Unset, "link.ssh_agent")
	cfg, err := h.loadEdited(e)
	if err != nil {
		return "", "", err
	}
	if cfg.SSHAgent != "" {
		return "", "", fmt.Errorf("conformance: ssh_agent is still %q in the copy", cfg.SSHAgent)
	}
	cfg.Getenv = func(k string) string {
		if k == "SSH_AUTH_SOCK" {
			return standIn
		}
		return ""
	}
	a := &App{Cfg: cfg, SSH: &SSHLookup{
		G:    func(string) ([]byte, error) { return []byte("user conformance\n"), nil },
		Home: func() (string, error) { return "", errors.New("conformance: no home") },
		FS:   emptyFS{},
	}}
	st := settingsOf(cfg)
	sock, from := a.agentFrom(st.SSHAgent, st.SSHAgentFrom, st.Trust)
	return sock, from, nil
}

// emptyFS holds no file.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Create is the launch's decision to create the sandbox (checkTrust, as
// createContainer makes it before anything is built or created). The
// suite never creates the sandbox it decides on: when the decision lets
// it through, the result says so, unrefused and not created, and no
// sandbox or entrypoint ever starts.
func (h *confHost) Create(ctx context.Context) (conformance.LaunchResult, error) {
	a, err := h.app()
	if err != nil {
		return conformance.LaunchResult{}, err
	}
	if err := a.checkTrust(); err != nil {
		return refusal(err), nil
	}
	return conformance.LaunchResult{Message: "the launcher's decision let the creation through; the suite created nothing"}, nil
}

// Attach is the launch's decision on the existing sandbox
// (checkTrustExisting, as checkExisting makes it), with no tmux, no
// session and nothing started.
func (h *confHost) Attach(ctx context.Context) (conformance.LaunchResult, error) {
	a, err := h.app()
	if err != nil {
		return conformance.LaunchResult{}, err
	}
	return refusal(a.checkTrustExisting()), nil
}

// SetTrust rewrites trust in config.toml as setup does (writeConfig), and
// returns once the running link, if any, decides by it.
func (h *confHost) SetTrust(trust string) error {
	if trust != config.TrustFallible && trust != config.TrustHostile {
		return fmt.Errorf("conformance: no trust %q", trust)
	}
	a, err := h.app()
	if err != nil {
		return err
	}
	if _, err := a.writeConfig(config.Edit{Set: map[string]any{"trust": trust}}); err != nil {
		return err
	}
	if err := h.G.NoteConfigWritten(); err != nil {
		return err
	}
	if held, err := linkRunning(h.G.DataDir()); err != nil || !held {
		return err
	}
	return h.settle(context.Background(), func(st linkState) bool { return st.Trust == trust })
}

// settle waits until link.json says what ok wants.
func (h *confHost) settle(ctx context.Context, ok func(linkState) bool) error {
	deadline := time.Now().Add(confSettleWait)
	for {
		st, err := h.readLinkState()
		if err == nil && ok(st) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("conformance: the running link did not take the new settings within %v (%v)", confSettleWait, err)
		}
		if err := h.Wait(ctx, confSettlePoll); err != nil {
			return err
		}
	}
}

// ClaudeProcesses counts the Claude Code processes in the environment's
// sandbox: none when there is no sandbox. A container's are listed from
// the engine (docker top, by the pinned ID), a VM's from the guest's
// /proc. The suite starts none on the host, and never counts the person's.
func (h *confHost) ClaudeProcesses() (int, error) {
	a := h.a
	if a == nil {
		return 0, errors.New("conformance: the environment's App was not built")
	}
	if st := a.box().State(); st == "absent" {
		return 0, nil
	}
	if h.box == nil {
		return 0, errors.New("conformance: the sandbox is not reachable")
	}
	pins, _, ok := h.G.Pinned()
	if !ok {
		return 0, errors.New("conformance: the sandbox exists, but was never pinned")
	}
	if !a.isVM() {
		out, err := a.Docker.Output("top", pins.ContainerID)
		if err != nil {
			return 0, fmt.Errorf("docker top: %v", err)
		}
		return countClaudeTop(out), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), confExecTimeout)
	defer cancel()
	rc, out, _, err := h.box.Exec(ctx, "root", []string{"/bin/sh", "-c", `for p in /proc/[0-9]*; do cat "$p/cmdline" 2>/dev/null; echo; done`}, nil)
	if err != nil || rc != 0 {
		return 0, fmt.Errorf("listing the guest's processes: exit %d, %v", rc, err)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if claudeArgv(strings.Split(line, "\x00")) {
			n++
		}
	}
	return n, nil
}

// countClaudeTop counts the rows of docker top's table whose command is
// Claude Code's.
func countClaudeTop(out string) int {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		return 0
	}
	head := strings.Fields(lines[0])
	col := slices.IndexFunc(head, func(s string) bool { return s == "CMD" || s == "COMMAND" })
	if col < 0 {
		return 0
	}
	n := 0
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > col && claudeArgv(f[col:]) {
			n++
		}
	}
	return n
}

// claudeArgv reports whether argv is Claude Code's: the native build
// (claude, or a file of its versions directory), or node running its
// package.
func claudeArgv(argv []string) bool {
	if len(argv) == 0 || argv[0] == "" {
		return false
	}
	if path.Base(argv[0]) == "claude" || strings.Contains(argv[0], "/claude/versions/") {
		return true
	}
	return len(argv) > 1 && path.Base(argv[0]) == "node" && strings.Contains(argv[1], "claude-code")
}

// --- EgressOps

func (h *confHost) StartListener(ctx context.Context, scope conformance.Scope) (conformance.SuiteListener, error) {
	own, err := conformance.OwnAddrs()
	if err != nil {
		return nil, err
	}
	l, err := conformance.ListenScope(ctx, scope, own, h.G.Nonce())
	if err != nil {
		return nil, err
	}
	return l, nil
}

const confVMProfile = config.KindVM + "." + confProfile

// EgressKeys are [vm.conformance]'s egress keys as config.toml holds them:
// none when it has no such table (a container run's).
func (h *confHost) EgressKeys() (conformance.EgressPolicy, error) {
	b, err := h.readConfig()
	if err != nil {
		return conformance.EgressPolicy{}, err
	}
	f, err := config.ParseFile(filepath.Join(h.G.Base(), filepath.FromSlash(h.configRel())), b)
	if err != nil {
		return conformance.EgressPolicy{}, err
	}
	var p conformance.EgressPolicy
	strs := func(k string) ([]string, error) {
		v, ok := f.Vals[confVMProfile+"."+k]
		if !ok {
			return nil, nil
		}
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is not a list", k)
		}
		out := []string{}
		for _, e := range list {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s holds a non-string", k)
			}
			out = append(out, s)
		}
		return out, nil
	}
	if p.Allow, err = strs("egress_allow"); err != nil {
		return p, err
	}
	if p.Deny, err = strs("egress_deny"); err != nil {
		return p, err
	}
	if v, ok := f.Vals[confVMProfile+".egress_ports"]; ok {
		list, ok := v.([]any)
		if !ok {
			return p, errors.New("egress_ports is not a list")
		}
		p.Ports = []int{}
		for _, e := range list {
			n, err := confPort(e)
			if err != nil {
				return p, err
			}
			p.Ports = append(p.Ports, n)
		}
	}
	return p, nil
}

// confPort is one egress_ports entry as a port: a whole number, or a
// one-port range "N-N" (confPorts).
func confPort(e any) (int, error) {
	switch v := e.(type) {
	case int64:
		return int(v), nil
	case string:
		a, b, ok := strings.Cut(v, "-")
		if n, err := strconv.Atoi(a); ok && err == nil && a == b {
			return n, nil
		}
	}
	return 0, fmt.Errorf("egress_ports holds %v, which the suite never writes", e)
}

// egressEdit is the change to [vm.conformance] that p says.
func egressEdit(p conformance.EgressPolicy) config.Edit {
	e := config.Edit{Tables: []string{confVMProfile}, Set: map[string]any{
		confVMProfile + ".egress_allow": append([]string{}, p.Allow...),
		confVMProfile + ".egress_deny":  append([]string{}, p.Deny...),
	}}
	if p.Ports == nil {
		e.Unset = []string{confVMProfile + ".egress_ports"}
	} else {
		e.Set[confVMProfile+".egress_ports"] = confPorts(p.Ports)
	}
	return e
}

// egressOf is what of the link's settings the egress keys decide.
func egressOf(s linkSettings) [4]string {
	return [4]string{strconv.FormatBool(s.Egress), s.EgressPorts, s.EgressAllow, s.EgressDeny}
}

// SetEgress rewrites [vm.conformance]'s egress keys as setup does
// (writeConfig), and returns once the running link decides by them: its
// link.json says them, and, when they changed what it decides by, the
// session it ended for them has been made again (a "linked to" in
// link.log after its "config.toml changed").
func (h *confHost) SetEgress(p conformance.EgressPolicy) error {
	e := egressEdit(p)
	want, err := h.loadEdited(e)
	if err != nil {
		return err
	}
	a, err := h.app()
	if err != nil {
		return err
	}
	logRel := linkLogFile
	before, beforeErr := h.readLinkState()
	off := confLogSize(h.G.DataDir(), logRel)
	if _, err := a.writeConfig(e); err != nil {
		return err
	}
	if err := h.G.NoteConfigWritten(); err != nil {
		return err
	}
	if beforeErr != nil {
		// No link runs (a run with no sandbox): nothing decides by them.
		return nil
	}
	target := egressOf(settingsOf(want))
	if egressOf(before.linkSettings) == target {
		return nil
	}
	if err := h.settle(context.Background(), func(st linkState) bool { return egressOf(st.linkSettings) == target }); err != nil {
		return err
	}
	return h.relinked(context.Background(), logRel, off, a.Cfg.Container)
}

// confLogSize is the size of the data dir's file rel, 0 when there is none.
func confLogSize(dataDir, rel string) int64 {
	fi, err := nofollow.Dir(dataDir).Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

// relinked waits until link.log, from off on, says the link reread
// config.toml and then linked to container again.
func (h *confHost) relinked(ctx context.Context, rel string, off int64, container string) error {
	deadline := time.Now().Add(confSettleWait)
	for {
		b, _, err := nofollow.Dir(h.G.DataDir()).ReadFileMax(rel, confLogMax)
		if err == nil {
			if int64(len(b)) < off {
				off = 0 // the capped log started again
			}
			tail := string(b[off:])
			if i := strings.Index(tail, "config.toml changed"); i >= 0 && strings.Contains(tail[i:], "linked to "+container) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("conformance: the link did not link again within %v of the change", confSettleWait)
		}
		if err := h.Wait(ctx, confSettlePoll); err != nil {
			return err
		}
	}
}

// LaunchDecision is the launch's decisions on a copy of config.toml with
// the egress keys p says: checkTrust, as a creation makes it, and, for the
// sandbox that exists, checkTrustExisting, as an attach makes it. It
// writes, creates and starts nothing.
func (h *confHost) LaunchDecision(ctx context.Context, p conformance.EgressPolicy) (conformance.LaunchResult, error) {
	cfg, err := h.loadEdited(egressEdit(p))
	if err != nil {
		return conformance.LaunchResult{}, err
	}
	a, err := h.appWith(cfg)
	if err != nil {
		return conformance.LaunchResult{}, err
	}
	if err := a.checkTrust(); err != nil {
		return refusal(err), nil
	}
	if a.box().State() != "absent" {
		return refusal(a.checkTrustExisting()), nil
	}
	return conformance.LaunchResult{}, nil
}

// --- CheckpointOps

func (h *confHost) StorePath() (string, error) {
	return filepath.Join(h.G.Base(), filepath.FromSlash(h.G.StoreRel())), nil
}

// otherEnv is checkpointOpts.apps for the suite. The launcher asks only
// environments with a directory under CABOOSE_HOME (envsWithDir), so the
// default environment, which the throwaway CABOOSE_HOME does not have, is
// never asked, and its container on the engine, the person's own, never
// inspected. Any other environment there is not the suite's, and refuses.
func (h *confHost) otherEnv(env string) (*App, error) {
	if env == h.G.Env() {
		return h.app()
	}
	return nil, fmt.Errorf("conformance: environment %s exists in the throwaway CABOOSE_HOME", env)
}

// checkpointRun runs `caboose checkpoint ARGS` in this process, as the
// command does, and returns what it printed.
func (h *confHost) checkpointRun(o checkpointOpts, args ...string) (string, error) {
	a, err := h.app()
	if err != nil {
		return "", err
	}
	o.apps = h.otherEnv
	var out bytes.Buffer
	a.Stdout = &out
	err = a.checkpointCmd(args, o)
	return out.String(), err
}

var (
	confTookRe   = regexp.MustCompile(`: took (\S+) \(`)
	confShrunkRe = regexp.MustCompile(`half or less of what (\S+) held`)
)

func (h *confHost) TakeCheckpoint(ctx context.Context, p string) (conformance.CheckpointTaken, error) {
	out, err := h.checkpointRun(checkpointOpts{}, "create", p)
	if err != nil {
		return conformance.CheckpointTaken{}, fmt.Errorf("%v: %s", err, firstLine(out))
	}
	return parseTaken(out)
}

func parseTaken(out string) (conformance.CheckpointTaken, error) {
	m := confTookRe.FindStringSubmatch(out)
	if m == nil {
		return conformance.CheckpointTaken{}, fmt.Errorf("conformance: the create did not say what it took: %s", firstLine(out))
	}
	t := conformance.CheckpointTaken{ID: m[1]}
	if s := confShrunkRe.FindStringSubmatch(out); s != nil {
		t.Shrunk = s[1]
	}
	return t, nil
}

func (h *confHost) List(ctx context.Context) ([]conformance.CheckpointPath, error) {
	out, err := h.checkpointRun(checkpointOpts{}, "list", "--json")
	if err != nil {
		return nil, err
	}
	return parseListing([]byte(out))
}

func parseListing(b []byte) ([]conformance.CheckpointPath, error) {
	var res daemonproto.CheckpointsResult
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("conformance: the listing: %v", err)
	}
	var out []conformance.CheckpointPath
	for _, l := range res.Paths {
		cp := conformance.CheckpointPath{Path: l.Path, Key: l.Key, Shrunk: l.Shrunk}
		for _, v := range l.Checkpoints {
			cp.Checkpoints = append(cp.Checkpoints, conformance.CheckpointInfo{ID: v.ID, Reason: v.Reason, Shrunk: v.Shrunk, Entries: v.Entries, Bytes: v.Bytes, Pinned: v.Pinned})
		}
		out = append(out, cp)
	}
	return out, nil
}

// Tree is checkpoint id's copy, read from the store without following a
// link: store/<key>/<stamp>/tree.
func (h *confHost) Tree(id string) (map[string]conformance.Identity, error) {
	cid, err := checkpoint.ParseID(id)
	if err != nil {
		return nil, err
	}
	return h.G.StoreTree(h.G.StoreRel() + "/" + cid.Key + "/" + checkpoint.FormatStamp(cid.Stamp) + "/" + checkpoint.TreeName)
}

// Restore is `caboose checkpoint restore ID --to DIR`, DIR the target's
// host path as the guard gives it (RestorePath) and nothing else. The
// product's refusals of the target are outcomes; any other failure is an
// error.
func (h *confHost) Restore(ctx context.Context, id string, to conformance.RestoreTarget) (conformance.RestoreOutcome, error) {
	dir, err := h.G.RestorePath(to)
	if err != nil {
		return conformance.RestoreOutcome{}, err
	}
	var res *checkpoint.Restored
	var rerr error
	o := checkpointOpts{restored: func(r checkpoint.Restored, err error) { res, rerr = &r, err }}
	out, err := h.checkpointRun(o, "restore", id, "--to", dir)
	return restoreOutcome(res, rerr, err, out)
}

// restoreOutcome maps what checkpoint.Restore returned (res and rerr, nil
// res when it was never called) and what the command returned (err).
func restoreOutcome(res *checkpoint.Restored, rerr, err error, out string) (conformance.RestoreOutcome, error) {
	if res == nil {
		if err == nil {
			err = errors.New("the restore never ran")
		}
		return conformance.RestoreOutcome{}, fmt.Errorf("%v: %s", err, firstLine(out))
	}
	o := conformance.RestoreOutcome{Warnings: slices.Clone(res.Warnings)}
	switch {
	case rerr == nil && err == nil:
		return o, nil
	case errors.Is(rerr, checkpoint.ErrTargetExists):
		o.Refused, o.Message = conformance.RefusedExists, rerr.Error()
		return o, nil
	case errors.Is(rerr, checkpoint.ErrTargetRefused):
		o.Refused, o.Message = conformance.RefusedUnsafe, rerr.Error()
		return o, nil
	case rerr != nil:
		return o, rerr
	}
	return o, err
}

// Discard removes what a restore wrote at to, through the guard
// (DiscardRestore: saferm, with B as the root).
func (h *confHost) Discard(to conformance.RestoreTarget) error { return h.G.DiscardRestore(to) }

// Prune is the daemon's prune round for the environment's store: under
// the store's lock, every key by the retention rules with ages from at
// (not from the key's newest, so that a run need not wait days for a
// checkpoint to be due), the newest, the store's pins and pinned kept.
func (h *confHost) Prune(ctx context.Context, at time.Time, pinned []string) ([]string, error) {
	var pins []checkpoint.ID
	for _, p := range pinned {
		id, err := checkpoint.ParseID(p)
		if err != nil {
			return nil, err
		}
		pins = append(pins, id)
	}
	a, err := h.app()
	if err != nil {
		return nil, err
	}
	s, err := a.openCheckpointStore(h.otherEnv)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	l, err := s.Lock(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	keys, err := s.Keys()
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, k := range keys {
		ids, err := s.Prune(l, k.Key, checkpoint.Rules{KeepNewest: true, From: at}, pins)
		for _, id := range ids {
			dropped = append(dropped, id.String())
		}
		if err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}
