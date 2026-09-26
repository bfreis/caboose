// Package imagecheck decides whether an image can be a caboose base: it runs
// the embedded probe (imagecheck.sh) in a throwaway container of the image
// and holds the requirements the probe's findings are measured against.
//
// caboose never installs packages into a user's image (see docs/images.md,
// "Requirements for your own image"), so everything the sandbox needs has to
// be there already. Checking up front turns what would otherwise be an
// obscure build error, a boot loop or a claude binary that cannot exec into
// a list of what is missing, with why each thing is needed.
//
// The probe reports facts and this package owns the verdict: which checks
// are required, which only on musl, which optional. That keeps the shell
// side dumb enough to be pure POSIX sh, and the policy in one place that is
// unit-tested against canned probe output.
package imagecheck

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/docker"
)

// ProbeVersion is the output format Parse understands, as the probe's first
// line ("probe 1") states it. The two ship together in one binary, so a
// mismatch means the output did not come from this probe at all.
const ProbeVersion = "1"

// Check is one "ok" or "missing" line of the probe's output.
type Check struct {
	// Name is the probe's name for it: bash, cacerts, tool:find, libgcc...
	Name string
	OK   bool
	// Detail is where the probe found it, or how it fell short; may be "".
	Detail string
}

// Report is what the probe found in an image.
type Report struct {
	Image string
	// NoShell means the image has no /bin/sh, so the probe could not run
	// and nothing else is known. Every other field is then zero.
	NoShell bool
	// Checks are in the order the probe printed them.
	Checks []Check
	// Libc is "glibc", "musl", or "" when the image has neither.
	Libc string
	// Arch is `uname -m` in the image ("" if it printed nothing).
	Arch string
	// Platform is the name Claude Code's installer would pick for the image
	// (linux-x64, linux-arm64-musl, ...), and so the ~/.local directory its
	// binaries belong in; "" when the architecture is unsupported.
	Platform string
	// UID and GID are the host IDs the probe was asked about, and UIDOwner
	// and GIDOwner the user and group in the image holding them, "" when
	// free. Taken is not a failure: the derived layer takes the entry over
	// for the agent user, and uses that group as agent's as it is (see
	// layer-user.sh).
	UID, GID           int
	UIDOwner, GIDOwner string
	// UIDHome and UIDShell are the home and login shell of UIDOwner, as
	// its passwd entry has them: what tells a system account (Debian's _apt,
	// a nologin shell) from a login user such as ubuntu or node.
	UIDHome, UIDShell string
	// uidKnown and gidKnown are set once the probe (or Run) has said which
	// IDs it was asked about, so a report without them does not read as 0.
	uidKnown, gidKnown bool
	// AgentUID and AgentGID are the IDs of a user and group already named
	// agent in the image, "" when there is none.
	AgentUID, AgentGID string
	// Warnings are the probe's "warn" lines, "NAME DETAIL": reachability of
	// claude.ai from the image, never a reason to refuse it.
	Warnings []string
}

// Check returns the named check and whether the probe reported it.
func (r *Report) Check(name string) (Check, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// requirement is one row of the requirements table in docs/images.md.
type requirement struct {
	name  string // the probe's check name
	label string // what a person reads: in the checklist and in Problems
	why   string
	// musl: required only when the image is musl-based.
	musl bool
	// optional: shown, never a problem.
	optional bool
	// tool: grouped into the checklist's single "tools" line.
	tool bool
	// setup: grouped into the checklist's "user" line: what the layer's
	// user setup edits and creates.
	setup bool
}

// requirements is the order the checklist and Problems follow.
var requirements = []requirement{
	{name: "sh", label: "/bin/sh", why: "the probe and the derived layer run with plain sh"},
	{name: "bash", label: "bash", why: "entrypoint.sh and Claude Code's installer are bash scripts, and the launcher runs `docker exec ... bash`"},
	{name: "curl", label: "curl", why: "the entrypoint downloads Claude Code's installer with it, and the installer the binary"},
	{name: "cacerts", label: "CA certificates", why: "curl verifies https://claude.ai against them"},
	{name: "tmux", label: "tmux", why: "every session runs in tmux"},
	{name: "git", label: "git", why: "`caboose sync` runs every git command in the container; 2.28 or later, for `git init -b`"},
	{name: "libc", label: "glibc or musl", why: "Claude Code ships builds for those two only"},
	{name: "arch", label: "x86_64 or aarch64", why: "Claude Code ships builds for those two only"},
	{name: "libgcc", label: "libgcc", musl: true, why: "the musl build of Claude Code links libgcc_s.so.1"},
	{name: "libstdc++", label: "libstdc++", musl: true, why: "the musl build of Claude Code links libstdc++.so.6"},
	{name: "ripgrep", label: "ripgrep", musl: true, why: "the musl build of Claude Code uses the system rg"},
	{name: "tool:env", label: "/usr/bin/env", tool: true, why: "entrypoint.sh's #! line"},
	{name: "tool:readlink", label: "readlink -f", tool: true, why: "entrypoint.sh finds the active version with it"},
	{name: "tool:mktemp", label: "mktemp", tool: true, why: "entrypoint.sh downloads the installer to a temp file"},
	{name: "tool:find", label: "find -delete", tool: true, why: "entrypoint.sh prunes old versions with it"},
	{name: "tool:rm", label: "rm", tool: true, why: "entrypoint.sh prunes old versions with it"},
	{name: "tool:rmdir", label: "rmdir", tool: true, why: "entrypoint.sh prunes old versions with it"},
	{name: "tool:sleep", label: "sleep", tool: true, why: "entrypoint.sh idles on it as PID 1"},
	{name: "tool:test", label: "test", tool: true, why: "the launcher's readiness check execs it"},
	{name: "tool:uname", label: "uname", tool: true, why: "Claude Code's installer picks the build with it"},
	{name: "tool:mkdir", label: "mkdir", tool: true, why: "Claude Code's installer"},
	{name: "tool:chmod", label: "chmod", tool: true, why: "Claude Code's installer"},
	{name: "tool:cut", label: "cut", tool: true, why: "Claude Code's installer"},
	{name: "tool:sed", label: "sed", tool: true, why: "Claude Code's installer"},
	{name: "tool:tr", label: "tr", tool: true, why: "Claude Code's installer"},
	{name: "tool:grep", label: "grep", tool: true, why: "Claude Code's installer"},
	{name: "tool:head", label: "head", tool: true, why: "Claude Code's installer"},
	{name: "tool:sha256sum", label: "sha256sum", tool: true, why: "Claude Code's installer verifies the download with it"},
	{name: "tool:chown", label: "chown", tool: true, why: "the derived layer gives the agent user its home with it"},
	{name: "etc:passwd", label: "/etc/passwd", setup: true, why: "the derived layer adds the agent user to it, in place"},
	{name: "etc:group", label: "/etc/group", setup: true, why: "the derived layer adds agent's group to it, in place"},
	{name: "home", label: "/home/agent", setup: true, why: "the derived layer makes it agent's home, with the bind mounts' mountpoints in it"},
	{name: "tic", label: "tic", optional: true, why: "compiles the host terminal's terminfo when the image lacks it; without it TERM falls back"},
}

// applies reports whether req is required of this image at all.
func (r *Report) applies(req requirement) bool {
	return !req.optional && (!req.musl || r.Libc == "musl")
}

// unmet is req's check when the image fails it: missing, or not reported
// at all (which a complete probe run never does, but a check the probe
// skipped must not pass by default).
func (r *Report) unmet(req requirement) (Check, bool) {
	c, ok := r.Check(req.name)
	if !ok {
		return Check{Name: req.name, Detail: "not reported by the probe"}, true
	}
	return c, !c.OK
}

// OK reports whether the image meets every requirement.
func (r *Report) OK() bool { return len(r.Problems()) == 0 }

// Problems lists the unmet requirements in human terms, one per line, each
// saying what is missing and why caboose needs it. Empty when OK.
func (r *Report) Problems() []string {
	ps := r.rootProblems()
	if r.NoShell {
		return append(ps, "/bin/sh: missing -- "+requirements[0].why+"; nothing else could be checked")
	}
	for _, req := range requirements {
		if !r.applies(req) {
			continue
		}
		c, bad := r.unmet(req)
		if !bad {
			continue
		}
		state := "missing"
		if c.Detail != "" {
			state += " (" + c.Detail + ")"
		}
		ps = append(ps, fmt.Sprintf("%s: %s -- %s", req.label, state, req.why))
	}
	return ps
}

// rootProblems refuses a host user that is root, or in group root: the
// layer gives agent the host's IDs, so it would take over the image's root
// user (renamed agent, home and shell changed) or make agent's group root,
// and a sandbox whose user is root is no sandbox. It is about the host, so
// it holds whatever the image.
func (r *Report) rootProblems() []string {
	var ps []string
	if r.uidKnown && r.UID == 0 {
		ps = append(ps, "uid: 0 -- caboose must not run as root: the layer would take over the image's root user; run it as a regular user")
	}
	if r.gidKnown && r.GID == 0 {
		ps = append(ps, "gid: 0 -- caboose must not run with group root (gid 0): the layer would make root agent's group; run it as a regular user, in a group of its own")
	}
	return ps
}

// SystemAccount reports whether the user holding the host's UID is a system
// account rather than a login user: its shell is nologin or false. (An
// empty shell field means /bin/sh, a login shell.) Taking one over is allowed (see Notes), but worth saying.
func (r *Report) SystemAccount() bool {
	if r.UIDOwner == "" || r.UIDOwner == "agent" {
		return false
	}
	switch r.UIDShell[strings.LastIndex(r.UIDShell, "/")+1:] {
	case "nologin", "false":
		return true
	}
	return false
}

// Notes are what is worth knowing about the image that is not a problem:
// for now, that the layer will take over a system account. That is allowed,
// because the alternative is no sandbox at all for a host user whose UID an
// image happens to use for one -- the agent user has to have the host UID,
// or files it writes into the bind mounts are not the host user's. What it
// costs is that whatever the image ran as that account now runs as agent.
func (r *Report) Notes() []string {
	if !r.SystemAccount() {
		return nil
	}
	return []string{fmt.Sprintf("the host's uid %d is the image's system account '%s' (home %s, shell %s); "+
		"the layer takes it over as agent, so whatever the image runs as '%s' will run as agent",
		r.UID, r.UIDOwner, or(r.UIDHome, "none"), or(r.UIDShell, "none"), r.UIDOwner)}
}

// Row is one line of the checklist: a label, what the image has there,
// and how that stands.
type Row struct {
	Label, Value string
	Level        Level
}

// Level is how a row of the checklist stands.
type Level int

const (
	Met   Level = iota // as caboose needs it
	Noted              // not a problem, but worth knowing
	Unmet              // a requirement the image does not meet
)

// Checklist is the report as caboose check-image prints it: one row per
// requirement that applies, with the entrypoint's and the installer's small
// tools folded into one, then the facts the derived layer depends on, then
// reachability.
func (r *Report) Checklist() []Row {
	if r.NoShell {
		return []Row{{"/bin/sh", "missing", Unmet}}
	}
	rows := []Row{r.row("sh"), r.row("bash"), r.row("curl"), r.row("cacerts"), r.row("tmux"), r.row("git")}
	if r.Libc == "" {
		rows = append(rows, Row{"libc", "unsupported (neither glibc nor musl)", Unmet})
	} else {
		rows = append(rows, Row{"libc", r.Libc, Met})
	}
	if c, bad := r.unmet(byName("arch")); bad {
		rows = append(rows, Row{"platform", "unsupported (arch " + or(c.Detail, "unknown") + ")", Unmet})
	} else {
		rows = append(rows, Row{"platform", r.Platform, Met})
	}
	if r.Libc == "musl" {
		rows = append(rows, r.row("libgcc"), r.row("libstdc++"), r.row("ripgrep"))
	}
	var tools []string
	for _, req := range requirements {
		if c, bad := r.unmet(req); req.tool && bad {
			tools = append(tools, req.label+detail(c))
		}
	}
	if len(tools) == 0 {
		rows = append(rows, Row{"tools", "ok", Met})
	} else {
		rows = append(rows, Row{"tools", "missing " + strings.Join(tools, ", "), Unmet})
	}
	var setup []string
	for _, req := range requirements {
		if c, bad := r.unmet(req); req.setup && bad {
			setup = append(setup, req.label+detail(c))
		}
	}
	if len(setup) == 0 {
		rows = append(rows, Row{"user", "ok", Met})
	} else {
		rows = append(rows, Row{"user", "unusable: " + strings.Join(setup, ", "), Unmet})
	}
	if c, bad := r.unmet(byName("tic")); bad {
		rows = append(rows, Row{"tic", "missing (optional)", Noted})
	} else {
		rows = append(rows, Row{"tic", "ok" + detail(c), Met})
	}

	uid := Row{"uid", idRow(r.UID, r.UIDOwner, "user"), Met}
	switch {
	case r.uidKnown && r.UID == 0:
		uid.Value, uid.Level = "0 (root: caboose must not run as root)", Unmet
	case r.SystemAccount():
		uid.Value = fmt.Sprintf("%d (held by system account '%s', home %s, shell %s; the layer takes it over as agent)",
			r.UID, r.UIDOwner, or(r.UIDHome, "none"), or(r.UIDShell, "none"))
		uid.Level = Noted
	}
	gid := Row{"gid", idRow(r.GID, r.GIDOwner, "group"), Met}
	if r.gidKnown && r.GID == 0 {
		gid.Value, gid.Level = "0 (root: caboose must not run with group root)", Unmet
	}
	rows = append(rows, uid, gid)
	var agent []string
	if r.AgentUID != "" {
		agent = append(agent, "user agent (uid "+r.AgentUID+")")
	}
	if r.AgentGID != "" {
		agent = append(agent, "group agent (gid "+r.AgentGID+")")
	}
	if len(agent) > 0 {
		rows = append(rows, Row{"agent", "the image already has " + strings.Join(agent, " and "), Noted})
	}
	net := Row{"network", r.network(), Met}
	if strings.HasPrefix(net.Value, "warning: ") {
		net.Level = Noted
	}
	return append(rows, net)
}

// row is the checklist row of a plain ok/missing requirement.
func (r *Report) row(name string) Row {
	req := byName(name)
	label := req.label
	switch name {
	case "sh":
		// The probe ran, so there is nothing to say about where.
		return Row{label, "ok", Met}
	case "cacerts":
		label = "ca-certs"
	}
	c, bad := r.unmet(req)
	if bad {
		return Row{label, "missing" + detail(c), Unmet}
	}
	return Row{label, "ok" + detail(c), Met}
}

func byName(name string) requirement {
	for _, req := range requirements {
		if req.name == name {
			return req
		}
	}
	panic("imagecheck: no requirement " + name)
}

func detail(c Check) string {
	if c.Detail == "" {
		return ""
	}
	return " (" + c.Detail + ")"
}

func idRow(id int, owner, kind string) string {
	switch owner {
	case "":
		return strconv.Itoa(id) + " (free)"
	case "agent":
		return fmt.Sprintf("%d (already %s 'agent')", id, kind)
	}
	if kind == "group" {
		return fmt.Sprintf("%d (held by group '%s'; the layer makes it agent's group, as it is)", id, owner)
	}
	return fmt.Sprintf("%d (held by %s '%s'; the layer takes it over as agent)", id, kind, owner)
}

// Unreachable reports whether the probe tried https://claude.ai (or the
// release host) from the image and got no answer, as opposed to not being
// able to try, for want of curl -- which is already a problem of its own.
func (r *Report) Unreachable() bool {
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "net ") && !strings.Contains(w, " not checked") {
			return true
		}
	}
	return false
}

// network summarises the "net" checks: ok when every URL answered.
func (r *Report) network() string {
	var bad []string
	for _, w := range r.Warnings {
		if rest, ok := strings.CutPrefix(w, "net "); ok {
			bad = append(bad, rest)
		}
	}
	if len(bad) == 0 {
		return "ok (https://claude.ai reachable)"
	}
	return "warning: " + strings.Join(bad, "; ")
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Parse reads the probe's output into a Report. It is strict: the output
// has to start with the probe's header and end with its trailer, and every
// line in between has to be one the probe prints, since anything else means
// the output is not (all) the probe's and a verdict drawn from it would be
// a guess.
func Parse(out string) (*Report, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "probe "+ProbeVersion {
		return nil, fmt.Errorf("not the probe's output (first line %q)", first(lines))
	}
	if lines[len(lines)-1] != "end" {
		return nil, errors.New("the probe's output is truncated (no end line)")
	}
	r := &Report{}
	for _, line := range lines[1 : len(lines)-1] {
		status, rest, _ := strings.Cut(line, " ")
		name, det, _ := strings.Cut(rest, " ")
		if name == "" {
			return nil, fmt.Errorf("malformed probe line %q", line)
		}
		switch status {
		case "ok", "missing":
			r.Checks = append(r.Checks, Check{Name: name, OK: status == "ok", Detail: det})
		case "warn":
			r.Warnings = append(r.Warnings, rest)
		case "info":
			if err := r.info(name, det); err != nil {
				return nil, fmt.Errorf("probe line %q: %w", line, err)
			}
		default:
			return nil, fmt.Errorf("malformed probe line %q", line)
		}
	}
	return r, nil
}

func first(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// info records one "info KEY VALUE..." line.
func (r *Report) info(key, val string) error {
	switch key {
	case "libc":
		if val != "unknown" {
			r.Libc = val
		}
	case "arch":
		if val != "unknown" {
			r.Arch = val
		}
	case "platform":
		r.Platform = val
	case "uid", "gid":
		idStr, owner, _ := strings.Cut(val, " ")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return fmt.Errorf("bad %s %q", key, idStr)
		}
		if key == "uid" {
			r.UID, r.UIDOwner, r.uidKnown = id, owner, true
		} else {
			r.GID, r.GIDOwner, r.gidKnown = id, owner, true
		}
	case "uid-home":
		r.UIDHome = val
	case "uid-shell":
		r.UIDShell = val
	case "user":
		if name, id, _ := strings.Cut(val, " "); name == "agent" {
			r.AgentUID = id
		}
	case "group":
		if name, id, _ := strings.Cut(val, " "); name == "agent" {
			r.AgentGID = id
		}
	}
	// Unknown keys are facts this launcher has no use for.
	return nil
}

// Args is the `docker run` command line that runs the probe script in a
// throwaway container of image, asking about the host uid and gid; platform,
// when not "", picks that variant of the image, as a build's --platform does.
//
// It runs as root, as the derived layer's build steps do, so what it can
// read and create is what they can; the entrypoint is /bin/sh, whatever the
// image's own; and CABOOSE_PROBE_ROOT is cleared so no image can point the
// probe at a fake root. --init puts docker's init in front of the sh, which
// as PID 1 would ignore the ^C that `docker run` forwards: a PID 1 gets no
// default signal handling, and sh installs none for SIGINT. The probe is
// then as interruptible as any command.
func Args(image, platform string, script []byte, uid, gid int) []string {
	argv := []string{"run", "--rm", "--init"}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	return append(argv, "--user", "0:0", "-e", "CABOOSE_PROBE_ROOT=",
		"--entrypoint", "/bin/sh", image,
		"-c", string(script), "caboose-probe", strconv.Itoa(uid), strconv.Itoa(gid))
}

// Run checks image by running the probe in a throwaway container: its
// platform variant, when platform is not "".
//
// The image should already be local: `docker run` pulls one that is not,
// and with its output captured here that is a long silence. Callers pull
// first, with docker's progress shown (the CLI's caboose check-image does).
//
// An image with no /bin/sh is an answer, not an error: a Report with NoShell
// set. The error is for when there is no answer: docker unreachable
// (docker.IsUnreachable is true of it), the container failing to start for
// another reason, or output that is not the probe's.
func Run(d *docker.CLI, image, platform string, uid, gid int) (*Report, error) {
	script, err := assets.ProbeScript()
	if err != nil {
		return nil, err
	}
	out, runErr := d.RawOutput(Args(image, platform, script, uid, gid)...)
	if runErr == nil {
		r, err := Parse(out)
		if err != nil {
			return nil, fmt.Errorf("checking image '%s': %w", image, err)
		}
		r.Image = image
		return r, nil
	}
	if noShell(runErr, out) {
		return &Report{Image: image, NoShell: true, UID: uid, GID: gid, uidKnown: true, gidKnown: true}, nil
	}
	return nil, classify(d, image, runErr)
}

// noShell tells an image without /bin/sh from any other failure to run. The
// probe itself always exits 0 and prints its header first, so a failure
// with no header is docker's. docker exits 127 when the entrypoint does not
// exist (126 when it exists but cannot be executed) and names the missing
// path on stderr: "exec: \"/bin/sh\": stat /bin/sh: no such file or
// directory" from runc, "executable file `/bin/sh` not found" from podman.
func noShell(err error, out string) bool {
	if strings.HasPrefix(out, "probe ") {
		return false
	}
	code := docker.ExitCode(err)
	var f *docker.Failure
	stderr := ""
	if errors.As(err, &f) {
		stderr = f.Stderr
	}
	if !strings.Contains(stderr, "/bin/sh") {
		return false
	}
	return code == 126 || code == 127 ||
		strings.Contains(stderr, "no such file or directory") || strings.Contains(stderr, "not found")
}

// classify turns a docker run failure into the error Run returns: marked
// unreachable when `docker version` fails too, as docker.ImageLabels does.
func classify(d *docker.CLI, image string, err error) error {
	err = fmt.Errorf("docker run %s: %w", image, err)
	if d.Quiet("version") != nil {
		return docker.Unreachable(err)
	}
	return err
}
