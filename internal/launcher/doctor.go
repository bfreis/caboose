package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/statesync"
)

// doctorBudget is how long doctor's fetch may take: shorter than a launch's
// syncBudget, since nothing waits on the answer but the person who asked.
// A variable for the tests.
var doctorBudget = 10 * time.Second

// Exit statuses of caboose doctor: 1 when it found a problem, 2 when it
// could not run at all. Notes alone exit 0.
const (
	doctorProblems = 1
	doctorUsage    = 2
)

// level is how much a finding matters.
type level int

const (
	levelOK        level = iota
	levelNote            // worth knowing; clears up by itself, or is a choice
	levelProblem         // broken now, or about to be, until the user acts
	levelUnchecked       // could not be told, and why
)

// finding is one row of doctor's checklist.
type finding struct {
	label string
	level level
	text  string
	// fix is the command (or, where no command can, the action) that
	// fixes a problem.
	fix string
}

// checkup collects doctor's findings, in the order they print.
type checkup struct {
	rows []finding
	// live, on a terminal, prints each finding as it comes, under a
	// spinner that says what is being checked.
	live *liveReport
}

func (c *checkup) add(l level, label, fix, format string, args ...any) {
	r := finding{label: label, level: l, text: fmt.Sprintf(format, args...), fix: fix}
	c.rows = append(c.rows, r)
	if c.live != nil {
		c.live.row(r)
	}
}

// checking says what is being checked now, on a terminal.
func (c *checkup) checking(what string) {
	if c.live != nil {
		c.live.checking(what)
	}
}

func (c *checkup) ok(label, format string, args ...any) { c.add(levelOK, label, "", format, args...) }
func (c *checkup) note(label, format string, args ...any) {
	c.add(levelNote, label, "", format, args...)
}
func (c *checkup) unchecked(label, format string, args ...any) {
	c.add(levelUnchecked, label, "", format, args...)
}
func (c *checkup) problem(label, fix, format string, args ...any) {
	c.add(levelProblem, label, fix, format, args...)
}

func (c *checkup) count(l level) int {
	n := 0
	for _, r := range c.rows {
		if r.level == l {
			n++
		}
	}
	return n
}

// marks are how each level of finding shows.
var marks = map[level]mark{levelOK: markOK, levelNote: markNote, levelProblem: markProblem, levelUnchecked: markUnchecked}

// render prints the checklist, then every problem's fix. Everything goes
// to u: the report is what was asked for, and `caboose doctor | less`
// should show all of it.
//
// On a terminal (u.width set) the text wraps under its column, a label
// repeated on the next row is shown once, and home is written ~. Anywhere
// else each finding is one line of its own, whole, for grep and sed.
func (c *checkup) render(u *ui, home string) {
	lw := 0
	for _, r := range c.rows {
		lw = max(lw, len(r.label))
	}
	prev := ""
	for _, r := range c.rows {
		renderRow(u, r, lw, prev, home)
		prev = r.label
	}
	c.renderFixes(u, lw, home)
}

// renderRow prints one finding, after prev's.
func renderRow(u *ui, r finding, lw int, prev, home string) {
	label := r.label
	if u.width > 0 && label == prev {
		label = ""
	}
	text := doctorText(u, r.text, home)
	if r.level == levelUnchecked {
		text = u.paint(dim, "not checked: "+text)
	}
	u.row(marks[r.level], label, lw, text)
}

// renderFixes prints the verdict, and every problem's fix.
func (c *checkup) renderFixes(u *ui, lw int, home string) {
	u.blank()
	n := c.count(levelProblem)
	if n == 0 {
		u.ok("No problems found.")
		return
	}
	u.fail("%d %s. To fix:", n, plural(n, "problem", "problems"))
	for _, r := range c.rows {
		if r.level == levelProblem {
			first := "    " + r.label + strings.Repeat(" ", lw-len(r.label)) + "  "
			fmt.Fprint(u.out, u.wrap(u.paint(bold, doctorText(u, or(r.fix, "see above"), home)), first, strings.Repeat(" ", lw+6)))
		}
	}
}

// doctorText is s as doctor shows it: printable, and with home as ~ on a
// terminal.
func doctorText(u *ui, s, home string) string {
	s = printable(s)
	if u.width > 0 {
		s = tildeHome(s, home)
	}
	return s
}

// tildeHome writes the home directory in s as ~.
func tildeHome(s, home string) string {
	if home == "" || home == "/" {
		return s
	}
	s = strings.ReplaceAll(s, home+"/", "~/")
	if strings.HasSuffix(s, home) {
		s = strings.TrimSuffix(s, home) + "~"
	}
	return s
}

// printable drops control characters: some of what doctor shows (a remote
// URL, a git error) comes from files the container writes, and must not
// drive the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// Restart's cost, for every fix that is one.
const endsSessions = " (this ends running sessions)"

// Doctor is caboose doctor [--offline]: what, if anything, is wrong with
// this environment, and for each problem the command that fixes it.
//
// It changes nothing: it never builds, creates, starts or stops, runs none
// of the data dir set-up a launch does, and writes nothing but what a git
// fetch into the sync repo writes -- which --offline leaves out, along with
// every other question to the network. With the container not running, it
// says what it could tell from the host and what it could not.
func (a *App) Doctor(args []string) error {
	offline := false
	for _, arg := range args {
		if arg != "--offline" {
			return &ExitError{Code: doctorUsage, Msg: fmt.Sprintf("usage: caboose doctor [--offline] (got %q)", arg)}
		}
		offline = true
	}
	u := newUI(a.Stdout, termWidth(a.Stdout, 100))
	u.banner("caboose doctor", "environment "+a.Cfg.Env)
	u.blank()
	c := &checkup{}
	if u.width > 0 {
		c.live = startLive(u, a.Cfg.Home, doctorLabelWidth)
	}
	c.checking("caboose itself")
	l, text := a.updateSummary()
	c.add(l, "caboose", "", "%s", text)
	c.checking("the configuration")
	rootsOK := a.doctorConfig(c)
	c.checking("the data dir")
	a.doctorDataDir(c)
	c.checking("the Docker engine")
	state, reachable := "", a.doctorDocker(c)
	if reachable {
		c.checking("the image")
		a.doctorImage(c)
		c.checking("the container")
		state = a.doctorContainer(c, rootsOK)
	}
	running := state == "running"
	agentKeys, agentWhy := []string(nil), "the container is not running"
	if running {
		c.checking("inside the container")
		agentKeys, agentWhy = a.doctorInside(c)
	} else {
		why := "the container is not running"
		if !reachable {
			why = "docker did not answer"
		}
		for _, l := range []string{"claude", "sessions", "ssh agent"} {
			c.unchecked(l, "%s", why)
		}
	}
	c.checking("git")
	a.doctorIdentity(c)
	a.doctorSigning(c, agentKeys, agentWhy)
	c.checking("the sync")
	switch {
	case !reachable:
		a.doctorSync(c, "docker did not answer")
	case !running:
		a.doctorSync(c, "the container is not running, and the sync's git runs there")
	case offline:
		a.doctorSync(c, "--offline")
	default:
		a.doctorSync(c, "")
	}
	if c.live != nil {
		c.live.end()
		c.renderFixes(u, doctorLabelWidth, a.Cfg.Home)
	} else {
		c.render(u, a.Cfg.Home)
	}
	if c.count(levelProblem) > 0 {
		return &ExitError{Code: doctorProblems}
	}
	return nil
}

// doctorConfig checks the environment's configuration, and reports
// whether the roots resolved.
func (a *App) doctorConfig(c *checkup) bool {
	cfg := a.Cfg
	switch {
	case cfg.File != nil:
		c.ok("env", "%s (config %s)", cfg.Env, cfg.File.Path)
	case cfg.EnvDir != "":
		c.note("env", "%s is not set up (no %s/%s), and runs on defaults; '%s' asks for its settings",
			cfg.Env, cfg.EnvDir, config.FileName, SetupCommand(cfg.Env, ""))
	default:
		c.ok("env", "%s", cfg.Env)
	}
	switch err := cfg.CheckImages(); {
	case errors.Is(err, config.ErrTwoBases):
		c.problem("image", "remove base_image from "+cfg.EnvDir+"/"+config.FileName+" (or unset CABOOSE_BASE_IMAGE), or move "+cfg.ImageDir+" away",
			"the environment has %s, and CABOOSE_BASE_IMAGE names '%s': which to build on would be a guess", cfg.ImageDir, cfg.BaseImage)
	case err != nil:
		c.problem("image", "unset CABOOSE_IMAGE (the default is caboose), or give it a tag of its own",
			"CABOOSE_BASE_IMAGE names the same image as CABOOSE_IMAGE ('%s'): the build would build over its own base", cfg.Image)
	}
	if err := cfg.ResolveRoots(); err != nil {
		c.problem("roots", "point the roots at directories that exist ("+cfg.RootsOrigin()+")", "%s", firstLine(err.Error()))
		return false
	}
	c.ok("roots", "%s", mountList(cfg.Roots))
	if len(cfg.Persist) > 0 {
		c.ok("persist", "%s", config.DescribePersist(cfg.Persist))
	}
	return true
}

// sandboxWrites are the files in the data dir every launch writes, which
// it refuses to write through a link, with where the sandbox sees them.
var sandboxWrites = []struct{ rel, inside string }{
	{".claude/CLAUDE.md", "~/.claude/CLAUDE.md"},
	{datadir.GitConfig, "~/.config/git/config"},
}

// doctorDataDir checks the data dir from the host: where it is, and links
// where a launch writes.
func (a *App) doctorDataDir(c *checkup) {
	cfg := a.Cfg
	c.ok("data dir", "%s", cfg.DataDir)
	d := nofollow.Dir(cfg.DataDir)
	for _, f := range sandboxWrites {
		if _, _, err := d.ReadFile(f.rel); errors.Is(err, nofollow.ErrNotPlain) {
			c.problem("data dir", "remove it from inside: caboose shell, then rm "+f.inside+"; the next launch writes it again",
				"%s is a symlink or a hard link, or is reached through one; a launch never writes through those, so it is not kept up to date", f.inside)
		}
	}
	// Keys belong on the host, reaching the sandbox through the agent.
	if keys, err := datadir.PrivateKeysIn(cfg.DataDir); err == nil && len(keys) > 0 {
		c.note("ssh", "the sandbox's ~/.ssh holds %s (%s), kept in %s: keys are meant to stay on the host and reach "+
			"the sandbox through the forwarded agent; to remove %s: caboose shell, then rm %s",
			plural(len(keys), "a private key file", "private key files"), strings.Join(keys, ", "),
			filepath.Join(cfg.DataDir, datadir.SSHDir), plural(len(keys), "it", "them"), "~/.ssh/"+strings.Join(keys, " ~/.ssh/"))
	}
	if extra := a.unconfiguredPersist(); len(extra) > 0 {
		c.note("persist", "%s in %s %s no longer in [persist], so not mounted; %s kept, with what %s, until you delete %s",
			strings.Join(extra, ", "), filepath.Join(cfg.DataDir, datadir.PersistRoot), plural(len(extra), "is", "are"),
			plural(len(extra), "it is", "they are"), plural(len(extra), "it holds", "they hold"), plural(len(extra), "it", "them"))
	}
	// The login is only looked at: its presence, never its contents.
	switch in, err := datadir.LoggedIn(cfg.DataDir); {
	case err != nil:
		c.unchecked("login", "%v", err)
	case in:
		c.ok("login", "Claude, in %s", filepath.Join(cfg.DataDir, datadir.Credentials))
	default:
		c.note("login", "not logged in to Claude yet; the first session asks (run caboose in a project)")
	}
}

// doctorDocker reports whether the engine answers.
func (a *App) doctorDocker(c *checkup) bool {
	if _, err := exec.LookPath(a.Docker.Path); err != nil {
		c.problem("docker", "install Docker (OrbStack, Docker Desktop, or the engine), with docker on PATH", "no '%s' on PATH", a.Docker.Path)
		return false
	}
	if err := a.Docker.Quiet("version"); err != nil {
		c.problem("docker", "start the Docker engine (OrbStack, Docker Desktop, ...), then run 'caboose doctor' again", "the engine does not answer")
		return false
	}
	c.ok("docker", "the engine answers")
	return true
}

// doctorImage places the local image against this launcher and the base in
// use, as a launch and caboose version do.
func (a *App) doctorImage(c *checkup) {
	cfg := a.Cfg
	if errors.Is(cfg.CheckImages(), config.ErrTwoBases) {
		return // no base to judge it against; the configuration's row says why
	}
	labels, exists, err := a.Docker.ImageLabels(cfg.Image)
	if err != nil && !exists {
		c.problem("image", "check CABOOSE_IMAGE (or image in config.toml)", "cannot inspect '%s': %v", cfg.Image, err)
		return
	}
	built := or(labels[assets.LabelVersion], "unknown")
	switch st := a.imageStatus(labels, exists); st.state {
	case imageMissing:
		if cfg.NoAutoBuild != "" {
			c.problem("image", "caboose build", "'%s' is not built, and CABOOSE_NO_AUTO_BUILD keeps a launch from building it", cfg.Image)
			return
		}
		c.note("image", "'%s' is not built yet; the first launch builds it (or 'caboose build' now)", cfg.Image)
	case imageCurrent:
		c.ok("image", "%s, matching this launcher (built by %s)", cfg.Image, built)
	case imageStale:
		fix := "caboose build, then caboose restart" + endsSessions
		switch {
		case a.rebuildsAtCreation(st) && st.switched():
			fix = "caboose restart, which rebuilds it on the configured base" + endsSessions
		case a.rebuildsAtCreation(st):
			fix = "caboose restart, which rebuilds it" + endsSessions
		}
		c.problem("image", fix, "%s is out of date: built by %s, %s", cfg.Image, built, st.reason)
	case imageUnlabelled:
		c.note("image", "%s was not built by caboose build (it has none of its labels), so it cannot be judged; 'caboose build' builds it", cfg.Image)
	}
}

// doctorContainer checks the container against the image and the roots,
// and returns its state.
func (a *App) doctorContainer(c *checkup, rootsOK bool) string {
	cfg := a.Cfg
	state := a.state()
	switch state {
	case "absent":
		c.note("container", "%s does not exist; a launch creates it", cfg.Container)
		return state
	case "running":
	default:
		c.note("container", "%s is %s; a launch starts it", cfg.Container, state)
	}
	running, current := a.Docker.ContainerImage(cfg.Container), a.Docker.ImageID(cfg.Image)
	switch p := a.compatProblem(a.containerCompat()); {
	case p != "":
		c.problem("container", "caboose restart"+endsSessions, "%s", p)
	case running != "" && current != "" && running != current:
		c.problem("container", "caboose restart"+endsSessions, "%s was created from an older image than %s", cfg.Container, cfg.Image)
	case state == "running":
		c.ok("container", "%s, running", cfg.Container)
	}
	if mounted := a.mountedRoots(); rootsOK && len(mounted) > 0 && !config.SameRoots(mounted, cfg.Roots) {
		c.problem("roots", "caboose restart"+endsSessions, "the container mounts %s; the configuration says %s",
			mountList(mounted), mountList(cfg.Roots))
	}
	if d := a.persistDrift(); d != "" {
		c.problem("persist", "caboose restart"+endsSessions, "the container %s", d)
	}
	return state
}

// unconfiguredPersist are the directories in the data dir's persist/ that
// no [persist] entry names, sorted.
func (a *App) unconfiguredPersist() []string {
	entries, err := os.ReadDir(filepath.Join(a.Cfg.DataDir, datadir.PersistRoot))
	if err != nil {
		return nil
	}
	named := map[string]bool{}
	for _, p := range a.Cfg.Persist {
		named[p.Name] = true
	}
	var extra []string
	for _, e := range entries {
		if e.IsDir() && !named[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

// doctorInsideScript prints the container's TZ and CABOOSE_KEEP_VERSIONS,
// NUL-terminated, then "sock" when the host's docker socket is mounted: one
// exec, bash builtins only.
const doctorInsideScript = `printf '%s\0%s\0' "${TZ-}" "${CABOOSE_KEEP_VERSIONS-}"; [ -S /var/run/docker.sock ] && printf sock; true`

// doctorInside checks what runs in the container, and returns the public
// keys the forwarded agent holds, or why they are not known.
func (a *App) doctorInside(c *checkup) (agentKeys []string, why string) {
	cfg := a.Cfg
	if v, err := a.Docker.Output("exec", cfg.Container, "claude", "--version"); err != nil {
		c.problem("claude", "caboose logs, to see why", "Claude Code does not run in the container: %s", firstLine(err.Error()))
	} else {
		c.ok("claude", "%s", firstLine(v))
	}

	if l, err := a.liveWork(); err != nil {
		c.note("sessions", "%v", err)
	} else if l.none() {
		c.ok("sessions", "none running")
	} else {
		c.ok("sessions", "%d in tmux, %d outside it", len(l.sessions), l.others)
	}

	if out, err := a.Docker.RawOutput("exec", cfg.Container, "bash", "-c", doctorInsideScript); err == nil {
		f := strings.SplitN(out, "\x00", 3)
		if len(f) == 3 {
			tz, keep, sock := f[0], f[1], f[2]
			if host := a.hostTimezone(); host != "" && tz != host {
				c.note("timezone", "the container has %s, the host %s now; new sessions get %s anyway, and 'caboose restart' realigns it",
					or(tz, "UTC"), host, host)
			}
			if keep != "" && keep != cfg.KeepVersions {
				c.note("versions", "the container keeps %s Claude Code versions, the configuration %s; 'caboose restart' picks that up",
					keep, cfg.KeepVersions)
			}
			if sock == "sock" {
				c.note("docker", "the host's docker socket is mounted: root-equivalent access to this host (unset CABOOSE_DOCKER_SOCK and 'caboose restart' to undo)")
			}
		}
	}

	status, usable := a.agentStatus()
	switch {
	case strings.HasPrefix(status, "not forwarded"):
		if a.sshAgentArgs() != nil {
			c.problem("ssh agent", "caboose restart, to forward it"+endsSessions,
				"not forwarded, though the host has an agent now")
			return nil, "no agent is forwarded"
		}
		c.note("ssh agent", "not forwarded: the host has no agent, so git over SSH and commit signing in the sandbox have none")
		return nil, "no agent is forwarded"
	case !usable:
		c.problem("ssh agent", a.agentFix(status), "%s", status)
		return nil, "the agent is not usable"
	}
	c.ok("ssh agent", "%s", status)
	out, err := a.Docker.Output("exec", cfg.Container, "ssh-add", "-L")
	if err != nil {
		return nil, "the agent's keys cannot be listed"
	}
	return strings.Split(out, "\n"), ""
}

// agentFix is what makes an unusable forwarded agent usable.
func (a *App) agentFix(status string) string {
	engine := or(a.macEngine(), "the Docker engine")
	switch {
	case goos == "darwin":
		return "make SSH_AUTH_SOCK point at your agent for apps started outside a terminal (1Password: " +
			"\"Configure SSH_AUTH_SOCK globally\"), then restart " + engine
	case strings.Contains(status, "no keys"):
		return "add a key to the host's agent (ssh-add)"
	}
	return "caboose restart, to forward the agent the host has now" + endsSessions
}

// doctorIdentity checks the sandbox has a git identity to commit with.
// None is a problem: every commit in the sandbox fails ("Author identity
// unknown"), and nothing but the user fixes it, now that a launch no
// longer copies the host's.
func (a *App) doctorIdentity(c *checkup) {
	git := datadir.FindGit()
	if git == nil {
		c.unchecked("git", "no git on the host to read the sandbox's config with")
		return
	}
	g, err := datadir.ReadSandboxGit(a.Cfg.DataDir, git)
	switch {
	case errors.Is(err, nofollow.ErrNotPlain):
		return // the data dir's row says so
	case err != nil:
		c.unchecked("git", "cannot read the sandbox's git config: %v", err)
		return
	case g.Complete():
		c.ok("git", "%s <%s>", g.Name, g.Email)
		return
	}
	var missing []string
	if g.Name == "" {
		missing = append(missing, "user.name")
	}
	if g.Email == "" {
		missing = append(missing, "user.email")
	}
	c.problem("git", SetupCommand(a.Cfg.Env, "git"), "no git identity in the sandbox (%s unset): commits in it fail",
		strings.Join(missing, " and "))
}

// doctorSigning checks commit signing: whether the sandbox signs, and with
// a key the forwarded agent holds; and when it does not, whether the host
// does and why that was not carried over. agentKeys are the agent's public
// keys, nil with why when they are not known.
func (a *App) doctorSigning(c *checkup, agentKeys []string, why string) {
	git := datadir.FindGit()
	if git == nil {
		c.unchecked("signing", "no git on the host to read the configurations with")
		return
	}
	hostKey, skipped := datadir.HostSigningKey(git)
	sb, err := datadir.ReadSandboxSigning(a.Cfg.DataDir, git)
	switch {
	case errors.Is(err, nofollow.ErrNotPlain):
		return // the data dir's row says so
	case err != nil:
		c.unchecked("signing", "cannot read the sandbox's git config: %v", err)
		return
	case sb.Key == "" && skipped != "":
		c.note("signing", "the host signs commits with SSH, but the sandbox will not: %s", skipped)
		return
	case sb.Key == "" && hostKey != "":
		c.note("signing", "the host signs commits with SSH, the sandbox does not; '%s' sets it up", SetupCommand(a.Cfg.Env, "git"))
		return
	case sb.Key == "":
		c.ok("signing", "not set up; commits in the sandbox are not signed")
		return
	case !strings.EqualFold(sb.Format, "ssh"):
		c.unchecked("signing", "the sandbox signs with gpg.format '%s', which doctor does not check", or(sb.Format, "openpgp"))
		return
	}
	pub := sb.PublicKey()
	if pub == "" {
		c.unchecked("signing", "the sandbox signs with the key file %s, which only the sandbox can read", sb.Key)
		return
	}
	name := keyName(pub)
	if why != "" {
		c.unchecked("signing", "whether the agent holds %s: %s", name, why)
		return
	}
	if holds(agentKeys, pub) {
		c.ok("signing", "SSH, with %s, which the agent holds", name)
		return
	}
	if sb.Signs() {
		c.problem("signing", "add that key to the host's agent, or change user.signingkey in the sandbox's ~/.config/git/config",
			"the sandbox signs every commit with %s, which the forwarded agent does not hold: commits fail", name)
		return
	}
	c.note("signing", "user.signingkey %s is not in the forwarded agent: a signed commit (git commit -S) fails", name)
}

// keyName shortens a public key line for a person: its type and comment.
func keyName(pub string) string {
	f := strings.Fields(pub)
	switch {
	case len(f) >= 3:
		return f[0] + " " + strings.Join(f[2:], " ")
	case len(f) == 2 && len(f[1]) > 12:
		return f[0] + " ..." + f[1][len(f[1])-12:]
	}
	return pub
}

// holds reports whether the agent's listing (ssh-add -L) has key pub, by
// its type and blob: the comment is the agent's to choose.
func holds(agentKeys []string, pub string) bool {
	want := strings.Fields(pub)
	if len(want) < 2 {
		return false
	}
	for _, k := range agentKeys {
		if f := strings.Fields(k); len(f) >= 2 && f[0] == want[0] && f[1] == want[1] {
			return true
		}
	}
	return false
}

// doctorSync checks the sync: whether a remote is set, what this machine
// has not sent (from the host), and, unless remoteWhy says why not, what
// the remote has that this machine has not taken (a fetch, through the
// same prompt-free, budgeted git as a launch's sync). The sync lock is
// held throughout, so no sync changes the repo underneath; a sync already
// running is said, not waited for.
func (a *App) doctorSync(c *checkup, remoteWhy string) {
	s := a.newSyncer(nil)
	if !s.MaybeRemote() {
		if a.Cfg.AutoSync != "" {
			c.note("sync", "auto_sync is on, but no remote is set; 'caboose sync --remote URL' sets one")
			return
		}
		c.ok("sync", "not set up ('caboose sync --remote URL' starts it)")
		return
	}
	unlock, err := s.Lock()
	if errors.Is(err, statesync.ErrLocked) {
		c.note("sync", "a sync is running; not checked")
		return
	}
	if err != nil {
		c.unchecked("sync", "%v", err)
		return
	}
	defer unlock()

	auto := "auto_sync off"
	if a.Cfg.AutoSync != "" {
		auto = "auto_sync on"
	}
	c.ok("sync", "%s, %s", or(s.RemoteHint(), "a remote is set"), auto)

	p, err := s.Pending()
	switch {
	case err != nil:
		c.problem("sync", "'caboose sync' says the same, in full", "cannot read what syncs: %v", err)
	default:
		a.doctorPending(c, p)
	}
	if remoteWhy != "" {
		c.unchecked("sync", "the remote: %s", remoteWhy)
		return
	}
	if err := a.checkSyncMount(); err != nil {
		c.problem("sync", "caboose restart"+endsSessions, "the container has no sync mount for this data dir")
		return
	}
	g := a.newSyncGit(true)
	g.deadline = time.Now().Add(doctorBudget)
	s.Git = g.command
	if dirty, err := s.Dirty(); err == nil && dirty {
		c.note("sync", "an earlier sync stopped halfway; the next one finishes it (what is waiting to be sent may be miscounted)")
	}
	c.checking("fetching from the sync remote")
	if err := s.Fetch(); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "Host key verification failed"):
			c.problem("sync", "caboose sync, from a terminal: it asks once to accept the key", "the remote's SSH host key is not accepted yet")
		case strings.Contains(msg, "no answer from the remote"):
			c.problem("sync", "check the network (or the remote), then 'caboose doctor' again", "no answer from the remote in %s", doctorBudget)
		default:
			c.problem("sync", "'caboose sync' shows the whole error", "cannot fetch from the remote: %s", strings.TrimPrefix(firstLine(msg), "git fetch -q origin: "))
		}
		return
	}
	d, err := s.Divergence()
	switch {
	case err != nil:
		c.unchecked("sync", "the remote: %v", err)
	case !d.Remote:
		c.note("sync", "the remote is empty; 'caboose sync' sends this machine's state")
	default:
		if d.Ahead > 0 {
			c.note("sync", "%s synced here never reached the remote (a push that failed); 'caboose sync' sends %s",
				plural(d.Ahead, "1 commit", fmt.Sprintf("%d commits", d.Ahead)), plural(d.Ahead, "it", "them"))
		}
		if d.Behind > 0 {
			c.note("sync", "the remote has changes not taken yet; %s", a.syncHow())
		} else {
			c.ok("sync", "nothing new on the remote")
		}
	}
}

// doctorPending reports what this machine has not sent.
func (a *App) doctorPending(c *checkup, p *statesync.Pending) {
	var se *statesync.SecretsError
	if err := p.Export.ScanSecrets(); errors.As(err, &se) {
		c.problem("sync", "take the secrets out of those files; every sync refuses until then", "%v", err)
	} else if err != nil {
		c.unchecked("sync", "the secrets scan: %v", err)
	}
	if len(p.Export.Refused) > 0 {
		c.note("sync", "not synced, being symlinks or hard links (never followed): %s", strings.Join(p.Export.Refused, ", "))
	}
	if len(p.Export.Unsynced) > 0 {
		c.note("sync", "not synced, being outside /work: %s", strings.Join(p.Export.Unsynced, ", "))
	}
	if !p.Any() {
		c.ok("sync", "nothing here waiting to be sent")
		return
	}
	paths := append(append([]string{}, p.Changed...), p.Deleted...)
	const shown = 3
	list := strings.Join(paths[:min(len(paths), shown)], ", ")
	if len(paths) > shown {
		list += fmt.Sprintf(", and %d more", len(paths)-shown)
	}
	n := len(paths)
	c.note("sync", "%s here not sent yet (%s); %s", plural(n, "1 change", fmt.Sprintf("%d changes", n)), list, a.syncHow())
}

// syncHow says what will sync, for a note about something waiting to.
func (a *App) syncHow() string {
	if a.Cfg.AutoSync != "" {
		return "the next launch with nothing running syncs, or 'caboose sync'"
	}
	return "'caboose sync' syncs"
}
