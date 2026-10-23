package conformance

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// This file is the one place a command for the sandbox's shell is made.
// A command is a Cmd, which only the builders below make, from SPath
// arguments, which only SandboxPath and the methods of an SPath make, and
// which are the literal paths of canaries (the run's nonce and a role):
// never a listing, a glob, a variable or anything the sandbox said.

const (
	// wrapPrefix and wrapSuffix are the fixed wrapper around every command:
	// the C locale, the command's stdout discarded (its errors stay on
	// stderr, whose first line is the message), then a line
	// rc=<status> on stdout. The $? is the one expansion in any command the suite
	// sends, and it is the wrapper's own.
	wrapPrefix = "LC_ALL=C; export LC_ALL; { "
	wrapSuffix = "; } >/dev/null; printf '\\nrc=%s\\n' \"$?\""
)

// SPath is a path inside the sandbox of a canary (or below one, or a
// mount point of a root), validated.
type SPath struct{ p string }

func (p SPath) String() string { return p.p }

var spathRe = regexp.MustCompile(`^/work/conf-[0-9a-f]{8}-(ro|dev)(/part)?(/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]{0,63})*)?$`)

// managedRe is a canary in the managed directory the sandbox has read-only.
var managedRe = regexp.MustCompile(`^/etc/claude-code/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]{0,63})*$`)

// managedRelRe is the host-side path of a canary in the managed directory:
// in the environment's data dir, home/envs/<env>/data (or,
// in a model of B with the data dir at its top, data).
var managedRelRe = regexp.MustCompile(`^(?:home/envs/` + EnvPattern + `/)?data/claude-code/(conf-[0-9a-f]{8}-[a-z0-9-]+)$`)

func pathOK(s string) bool { return spathRe.MatchString(s) || managedRe.MatchString(s) }

// relRe is the host-side path of a canary of the two roots.
var relRe = regexp.MustCompile(`^roots/(conf-[0-9a-f]{8}-(?:ro|dev))/((?:part/)?conf-[0-9a-f]{8}-[a-z0-9-]+)$`)

// SandboxPath is where the sandbox sees canary c: its path under a root.
// A canary the sandbox does not reach (under canary/ or home/) has none.
func SandboxPath(c Canary) (SPath, error) {
	if m := managedRelRe.FindStringSubmatch(c.Rel); m != nil {
		return SPath{"/etc/claude-code/" + m[1]}, nil
	}
	m := relRe.FindStringSubmatch(c.Rel)
	if m == nil {
		return SPath{}, fmt.Errorf("conformance: canary %s is not under a root the sandbox sees", c.Rel)
	}
	return SPath{"/work/" + m[1] + "/" + m[2]}, nil
}

// Child is the path of a named entry inside p.
func (p SPath) Child(name string) (SPath, error) {
	q := SPath{p.p + "/" + name}
	if p.p == "" || !pathOK(q.p) {
		return SPath{}, fmt.Errorf("conformance: %q is not a path the suite may name", q.p)
	}
	return q, nil
}

// MountPoint is the mount point of the writable part that p is in.
func (p SPath) MountPoint() (SPath, error) {
	i := strings.Index(p.p, "/part")
	if i < 0 {
		return SPath{}, fmt.Errorf("conformance: %s is not in a writable part", p.p)
	}
	return SPath{p.p[:i+len("/part")]}, nil
}

func (p SPath) base() string { return p.p[strings.LastIndex(p.p, "/")+1:] }

func (p SPath) valid() bool { return pathOK(p.p) }

// Cmd is a command for the sandbox's shell.
type Cmd struct {
	body        string
	destructive bool
	wipe        bool
}

// Destructive reports whether the command removes, moves or unmounts.
func (c Cmd) Destructive() bool { return c.destructive }

func (c Cmd) valid() bool { return c.body != "" }

// Argv is the command line the sandbox executes.
func (c Cmd) Argv() []string {
	return []string{"/bin/sh", "-c", wrapPrefix + c.body + wrapSuffix}
}

// Body is the command without the wrapper, for tests.
func (c Cmd) Body() string { return c.body }

func build(destructive bool, format string, ps ...SPath) (Cmd, error) {
	args := make([]any, len(ps))
	for i, p := range ps {
		if !p.valid() {
			return Cmd{}, errors.New("conformance: a command needs canary paths made by the suite")
		}
		args[i] = p.p
	}
	return Cmd{body: fmt.Sprintf(format, args...), destructive: destructive}, nil
}

// The builders. Each takes literal canary paths and nothing else.

func Append(p SPath) (Cmd, error)   { return build(false, "echo x >> %s", p) }
func Truncate(p SPath) (Cmd, error) { return build(false, "truncate -s 0 %s", p) }
func Touch(p SPath) (Cmd, error)    { return build(false, "touch %s", p) }
func Chmod(p SPath) (Cmd, error)    { return build(false, "chmod 600 %s", p) }
func Read(p SPath) (Cmd, error)     { return build(false, "cat %s", p) }
func List(p SPath) (Cmd, error)     { return build(false, "find %s", p) }

func Remove(p SPath) (Cmd, error)     { return build(true, "rm %s", p) }
func RemoveTree(p SPath) (Cmd, error) { return build(true, "rm -r %s", p) }
func Umount(mp SPath) (Cmd, error)    { return build(true, "umount %s", mp) }
func RemountRW(mp SPath) (Cmd, error) { return build(true, "mount -o remount,rw %s", mp) }

// MkdirIn makes a directory inside the directory canary p.
func MkdirIn(p SPath) (Cmd, error) {
	c, err := p.Child("new-dir")
	if err != nil {
		return Cmd{}, err
	}
	return build(false, "mkdir %s", c)
}

// SymlinkIn makes a symbolic link inside the directory canary p.
func SymlinkIn(p SPath) (Cmd, error) {
	t, err := p.Child("target")
	if err != nil {
		return Cmd{}, err
	}
	l, err := p.Child("new-link")
	if err != nil {
		return Cmd{}, err
	}
	return build(false, "ln -s %s %s", t, l)
}

// HardlinkIn makes a hard link, inside the tree canary p, to its first file.
func HardlinkIn(p SPath) (Cmd, error) {
	t, err := p.Child(p.base() + "-1")
	if err != nil {
		return Cmd{}, err
	}
	l, err := p.Child("new-hardlink")
	if err != nil {
		return Cmd{}, err
	}
	return build(false, "ln %s %s", t, l)
}

// MoveIn renames, inside the tree canary p, its first file.
func MoveIn(p SPath) (Cmd, error) {
	t, err := p.Child(p.base() + "-1")
	if err != nil {
		return Cmd{}, err
	}
	d, err := p.Child("moved")
	if err != nil {
		return Cmd{}, err
	}
	return build(true, "mv %s %s", t, d)
}

// AppendThrough links target from inside the directory canary scratch and
// appends through the link.
func AppendThrough(target, scratch SPath) (Cmd, error) {
	l, err := scratch.Child(target.base() + "-lnk")
	if err != nil {
		return Cmd{}, err
	}
	return build(false, "ln -sf %s %s && echo x >> %s", target, l, l)
}

// UmountThenAppend unmounts mp and then appends to p.
func UmountThenAppend(mp, p SPath) (Cmd, error) {
	return build(true, "umount %s && echo x >> %s", mp, p)
}

// ShellOf runs another command through sh -c, to exercise the nested form.
func ShellOf(c Cmd) (Cmd, error) {
	if !c.valid() || c.wipe || strings.Contains(c.body, "'") {
		return Cmd{}, errors.New("conformance: nothing to nest")
	}
	return Cmd{body: "sh -c '" + c.body + "'", destructive: c.destructive}, nil
}

// ErrNoEnvelope is what a check gets when it runs outside the runner.
var ErrNoEnvelope = errors.New("conformance: a check runs only inside the runner's envelope")

// RunCmd runs a command in the sandbox as user and classifies the result.
// Only a destructive check may run a destructive command.
func (e Env) RunCmd(ctx context.Context, user string, c Cmd) (ExecResult, error) {
	if e.seal == nil || e.sandbox == nil {
		return ExecResult{}, ErrNoEnvelope
	}
	if !c.valid() {
		return ExecResult{}, errors.New("conformance: empty command")
	}
	if c.wipe {
		return ExecResult{}, fmt.Errorf("conformance: check %s may run the wipe only through Wipe", e.seal.id)
	}
	if c.destructive && e.seal.class != Destructive {
		return ExecResult{}, fmt.Errorf("conformance: check %s is not destructive and may not run %q", e.seal.id, firstWord(c.body))
	}
	if user == "" {
		if err := e.SessionUserError(); err != nil {
			return ExecResult{}, fmt.Errorf("conformance: %w", err)
		}
	}
	if user != "agent" && user != "root" {
		return ExecResult{}, fmt.Errorf("conformance: unknown sandbox user %q", user)
	}
	return e.exec(ctx, user, c)
}

// GatedSandbox is a Sandbox that runs a destructive command its own way:
// the real one re-pins its target and runs the command behind the
// sandbox-side gate. Every destructive command goes to it when the
// Sandbox is one.
type GatedSandbox interface {
	ExecDestructive(ctx context.Context, user string, argv []string, stdin []byte) (rc int, stdout, stderr string, err error)
}

func (e Env) exec(ctx context.Context, user string, c Cmd) (ExecResult, error) {
	run := e.sandbox.Exec
	if g, ok := e.sandbox.(GatedSandbox); ok && c.destructive {
		run = g.ExecDestructive
	}
	rc, stdout, stderr, err := run(ctx, user, c.Argv(), nil)
	if err != nil {
		return ExecResult{RC: -1, Class: ClassOther, Msg: err.Error()}, err
	}
	res := ParseWrapped(stdout, stderr)
	if res.RC < 0 && rc != 0 {
		res.Msg = fmt.Sprintf("the shell exited %d without running the wrapper", rc)
	}
	return res, nil
}

// The wipe: one recursive removal, as the sandbox's most privileged
// user, of everything of the suite's the sandbox sees, each by its literal
// path: the suite's two roots under /work and the suite's canaries in the
// managed directory. Never "/", the home, /work itself, a glob or a
// variable: the gate checks just before it runs that /work holds exactly
// the two roots, so naming /work would remove nothing more of the host's,
// and only a path that is the run's own is named.

// WipeUser is who the wipe runs as: root, in every isolation.
const WipeUser = "root"

var (
	wipeManagedRe = regexp.MustCompile(`^/etc/claude-code/conf-[0-9a-f]{8}-[a-z0-9-]+$`)
)

// wipeCmd is the wipe of the run whose nonce begins with n8: rm -rf of
// its two roots, then of each of managed, which must be the run's
// canaries in the managed directory, each named once.
func wipeCmd(n8 string, managed []SPath) (Cmd, error) {
	if !n8Re.MatchString(n8) {
		return Cmd{}, fmt.Errorf("conformance: %q is no run's nonce", n8)
	}
	args := []string{"/work/conf-" + n8 + "-ro", "/work/conf-" + n8 + "-dev"}
	// The roots are built from n8 alone; each managed path is checked
	// against the one shape it may have, and named once.
	for _, p := range managed {
		if !wipeManagedRe.MatchString(p.p) || !strings.HasPrefix(p.p, "/etc/claude-code/conf-"+n8+"-") || slices.Contains(args, p.p) {
			return Cmd{}, fmt.Errorf("conformance: the wipe cannot name %q", p.p)
		}
		args = append(args, p.p)
	}
	return Cmd{body: "rm -rf " + strings.Join(args, " "), destructive: true, wipe: true}, nil
}

// WipeCommand is the wipe this check's Env would run: built from the
// run's nonce and the host's list of canaries, never from the sandbox.
func (e Env) WipeCommand() (Cmd, error) {
	if e.seal == nil || e.sandbox == nil {
		return Cmd{}, ErrNoEnvelope
	}
	if !e.seal.wipe || e.seal.class != Destructive {
		return Cmd{}, fmt.Errorf("conformance: check %s may not wipe", e.seal.id)
	}
	nonce, err := e.Nonce()
	if err != nil {
		return Cmd{}, err
	}
	cans, err := e.Canaries()
	if err != nil {
		return Cmd{}, err
	}
	var managed []SPath
	for _, cn := range cans {
		if p, err := SandboxPath(cn); err == nil && strings.HasPrefix(p.p, "/etc/claude-code/") {
			managed = append(managed, p)
		}
	}
	return wipeCmd(nonce[:8], managed)
}

// Wipe runs the wipe (WipeCommand) as WipeUser: it exists only in the Env
// of a check that is both Destructive and Wipe, which only Steps makes,
// and goes the way of every destructive command, behind the gate.
func (e Env) Wipe(ctx context.Context) (ExecResult, error) {
	c, err := e.WipeCommand()
	if err != nil {
		return ExecResult{}, err
	}
	return e.exec(ctx, WipeUser, c)
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}

// Unwrap is the command inside a wrapped argv, for a model of the shell.
func Unwrap(argv []string) (string, bool) {
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" {
		return "", false
	}
	b, ok := strings.CutPrefix(argv[2], wrapPrefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(b, wrapSuffix)
}

// GatedArgv is argv run behind the gate script.
func GatedArgv(script string, argv []string) ([]string, error) {
	if script == "" || len(argv) == 0 {
		return nil, errors.New("conformance: nothing to gate")
	}
	return append([]string{"/bin/sh", "-c", script, gateArg0}, argv...), nil
}

// scriptHeader is the first line of the suite's alias scripts (hostops.go),
// which the host runs: this file is the one place the shell is named.
const scriptHeader = "#!/bin/sh\n"
