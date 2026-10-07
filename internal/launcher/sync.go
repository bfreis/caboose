package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/syncagent"
	"github.com/bfreis/caboose/internal/tty"
)

// Sync backs up and syncs the portable part of the sandbox's home --
// memories, settings, skills, agents, commands, MCP servers -- through a
// git remote. See internal/statesync for what syncs and how it merges.
//
// The sync runs in the sandbox, as caboose-agent sync (internal/syncagent):
// it reads and writes the home and the sync repo there, where git sees
// every change as it is made, and its git reaches the remote with what the
// sandbox has -- the forwarded SSH agent, gh's token. The host needs no
// git, and neither its git config, hooks and credential helpers nor
// anything a remote sends come near it. The host holds the lock, says what
// happened, and asks the person at its terminal what the sync needs asked:
// how to settle a conflict, and git's and ssh's own prompts.
//
// It runs only while no session is running: sessions write the very files
// it merges, and Claude Code rewrites .claude.json whole from memory, which
// would undo what a sync wrote into it.
//
//	caboose sync                 sync with the remote already set
//	caboose sync --remote URL    set (or replace) the remote, then sync
//	caboose sync status          what would be sent, and taken (sandbox.go)
//	caboose sync add|rm PATH     a sync rule, added or removed (sandbox.go)
func (a *App) Sync(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return a.SyncStatus(args[1:])
		case "add", "rm":
			return a.SyncEdit(args[0], args[1:])
		}
	}
	var remote string
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "--remote" && args[1] != "":
		remote = args[1]
	case len(args) == 1 && strings.HasPrefix(args[0], "--remote=") && len(args[0]) > len("--remote="):
		remote = strings.TrimPrefix(args[0], "--remote=")
	default:
		return Die("usage: caboose sync [--remote URL] | status | add PATH | rm PATH")
	}
	// Up, as a launch brings it up (never building an image), but with no
	// session in it.
	if err := a.ensureRunning(false); err != nil {
		return err
	}
	// Its git reaches the remote through the outbound proxy, under vm.
	a.awaitProxy()
	if err := a.refuseLiveSessions(); err != nil {
		return err
	}
	mounts, err := a.checkSyncMount()
	if err != nil {
		return err
	}
	if v, err := backend.Output(a.box(), "git", "--version"); err != nil {
		return Die("git does not run in the %s (%v); the image needs git 2.28 or later, see 'caboose check-image'", a.noun(), err)
	} else if v == "" {
		return Die("git in the %s reports no version; the image needs git 2.28 or later", a.noun())
	}

	unlock, err := statesync.Lock(a.Cfg.DataDir)
	if errors.Is(err, statesync.ErrLocked) {
		return Die("%v; try again when it is done", err)
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()

	url := remote
	if url == "" {
		if !statesync.MaybeRemote(a.syncRepo()) {
			return a.noRemote()
		}
		url = statesync.RemoteHint(a.syncRepo())
	}
	req, err := a.syncRequest(mounts)
	if err != nil {
		return Die("%v", err)
	}
	interactive := tty.IsTerminal(os.Stdin.Fd())
	req.Remote, req.Interactive = remote, interactive
	var h syncagent.Handler
	if interactive {
		h = syncagent.Handler{Conflict: a.resolveConflict, Ask: a.askPrompt, Interrupted: a.interrupted}
	}
	var show io.Writer
	if tty.IsTerminal(os.Stderr.Fd()) {
		req.ShowGit, show = true, os.Stderr
	}
	a.Note("syncing %s with %s", a.Cfg.DataDir, or(shown(url), "its remote"))

	res, err := a.runSync(syncagent.OpRun, req, h, show)
	if err != nil {
		return err
	}
	if res.Report != nil {
		a.reportSync(res.Report)
	}
	e := res.Err
	switch {
	case e == nil:
		a.afterSync(res.Report)
		return nil
	case e.Kind == syncagent.KindSecrets:
		return Die("%v", (&statesync.SecretsError{Paths: shownAll(e.Paths)}).Error())
	case e.Kind == syncagent.KindAborted:
		msg := "%s; nothing in %s changed"
		if !interactive {
			msg += " (run it from a terminal to resolve the conflict)"
		}
		return Die(msg, shown(e.Message), a.Cfg.DataDir)
	case e.Kind == syncagent.KindNoRemote:
		return a.noRemote()
	case e.Kind == syncagent.KindSetup || e.Kind == syncagent.KindBusy || e.Kind == syncagent.KindStopped:
		return Die("%s", shown(e.Message))
	}
	return Die("sync failed: %s", shown(e.Message))
}

// noRemote is the error of a sync with no remote to sync with.
func (a *App) noRemote() error {
	return Die("no sync remote yet; set one once with\n" +
		"  caboose sync --remote URL\n" +
		"any git URL the sandbox can push to, such as an empty private repo")
}

// syncRepo is this data dir's sync repo, on the host. The host only ever
// reads it, and only for hints (statesync.MaybeRemote, RemoteHint) and
// what is waiting to be sent while the sandbox is down (localPending).
func (a *App) syncRepo() string { return filepath.Join(a.Cfg.DataDir, statesync.Dir) }

// syncRequest is the request for a sync of this data dir in the sandbox,
// with mounts, the sandbox's: the sandbox config in effect as the host
// loaded it (with its last good copy standing in for a file that does not
// parse), this machine's name and roots, and the keep entries the sandbox
// mounts.
func (a *App) syncRequest(mounts []backend.Mount) (syncagent.Request, error) {
	sb, err := a.sandboxConfig()
	if err != nil {
		return syncagent.Request{}, err
	}
	req := syncagent.Request{
		Host: a.syncHost(), Sandbox: sb.Data, Roots: a.rootPaths(),
		Defaults: sandboxcfg.Default(a.rootPaths()), Mounted: mountedInHome(mounts),
	}
	if sb.Err != nil {
		req.SandboxErr = sb.Err.Error()
	}
	return req, nil
}

// mountedInHome are the home-relative paths of mounts under the sandbox's
// home: the keep entries it has, among caboose's own mounts.
func mountedInHome(mounts []backend.Mount) []string {
	rels := []string{}
	for _, m := range mounts {
		if rel, ok := strings.CutPrefix(path.Clean(m.Target), sandboxcfg.ContainerHome+"/"); ok {
			rels = append(rels, rel)
		}
	}
	return rels
}

// errOldAgent is a sandbox whose caboose-agent cannot run this caboose's
// sync, as one line, to say with what fixes it.
func (a *App) oldAgent() string {
	return fmt.Sprintf("the %s was made from an image of another caboose, whose caboose-agent cannot run this one's sync", a.noun())
}

// runSync runs caboose-agent sync op in the sandbox with req, answering
// it with h, and returns what it ended with. An error is one to return as
// it is: the sync could not be run at all.
func (a *App) runSync(op string, req syncagent.Request, h syncagent.Handler, show io.Writer) (*syncagent.Result, error) {
	cmd := a.box().Command(backend.ExecSpec{Argv: []string{AgentPath, "sync", op}, Stdin: true})
	res, err := syncagent.Run(cmd, req, h, show)
	switch {
	case errors.Is(err, syncagent.ErrOldAgent):
		return nil, Die("%s; run 'caboose restart' to recreate it from this caboose's image\n"+
			"(nothing is lost; it asks before ending sessions)", a.oldAgent())
	case err != nil:
		return nil, Die("%s", shown(err.Error()))
	}
	return res, nil
}

// shown is text the sandbox sent, made safe to print: each line through
// proposal.Printable, without the \r ssh ends lines with, and every line
// after the first indented, so that none can pass for one of caboose's.
func shown(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = proposal.Printable(strings.TrimRight(l, "\r"))
		if i > 0 {
			lines[i] = "  " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// interrupted ends caboose when an interrupt came during a sync, the sync
// was told to stop, and a question on the terminal still holds it up.
func (a *App) interrupted() {
	fmt.Fprintln(a.Stderr)
	a.Note("interrupted; the sync in the %s was told to stop", a.noun())
	os.Exit(130)
}

// shownAll is shown of each of list.
func shownAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = proposal.Printable(s)
	}
	return out
}

// checkSyncMount makes sure the sandbox mounts this data dir's sync repo
// where the sync looks for it -- one created on another data dir mounts
// that one's, and the mounts are fixed at creation -- and returns the
// sandbox's mounts.
func (a *App) checkSyncMount() ([]backend.Mount, error) {
	mounts, err := a.box().Mounts()
	if err != nil {
		return nil, a.boxFailed(err)
	}
	want := a.syncRepo()
	for _, m := range mounts {
		if m.Target != statesync.ContainerDir {
			continue
		}
		if m.Source != want {
			return nil, Die("the %s mounts %s at %s, but this data dir's sync repo is %s; run 'caboose restart' to recreate it on this data dir",
				a.noun(), m.Source, statesync.ContainerDir, want)
		}
		return mounts, nil
	}
	return nil, Die("the %s does not mount %s at %s, where the sync runs;\n"+
		"run 'caboose restart' to recreate it (nothing is lost; it asks before ending sessions)", a.noun(), want, statesync.ContainerDir)
}

// live is what in the container could be writing the data dir.
type live struct {
	// sessions are the tmux sessions.
	sessions []string
	// others counts the Claude Code processes in none of them: a
	// session with tmux = false or a `caboose claude -p` run (plain docker execs),
	// or a background supervisor.
	others int
}

func (l live) none() bool { return len(l.sessions) == 0 && l.others == 0 }

// describe lists l, indented, one line each.
func (l live) describe() string {
	var b strings.Builder
	for _, s := range l.sessions {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	if l.others > 0 {
		fmt.Fprintf(&b, "  %d Claude Code %s outside tmux (tmux = false, caboose claude -p, background agents)\n",
			l.others, plural(l.others, "process", "processes"))
	}
	return b.String()
}

// isClaude reports whether a process's argv[0] runs Claude Code: the
// launcher symlink, or a version binary it points at.
func isClaude(arg0 string) bool {
	return path.Base(arg0) == "claude" || strings.Contains(arg0, "/.local/share/claude/versions/")
}

// processesScript prints "PID PPID ARGV0" for every process in the
// container, from /proc, with bash builtins alone (bash is an image
// requirement; ps is not). Not `docker top`: that runs ps where the engine
// runs -- in OrbStack's or Docker Desktop's VM -- whose columns vary with
// that ps. A process gone mid-loop is skipped; the ppid is the field after
// the ")" that ends the command name, which may itself hold spaces. The
// stat fields are numbers and letters, so splitting them unquoted is safe.
const processesScript = `for d in /proc/[0-9]*; do
  a0=
  IFS= read -r -d '' a0 2>/dev/null < "$d/cmdline"
  [ -n "$a0" ] || continue
  st=
  read -r st 2>/dev/null < "$d/stat" || continue
  st=${st##*) }
  set -- $st
  printf '%s %s %s\n' "${d#/proc/}" "$2" "$a0"
done`

// liveWork is what runs in the container that could be writing the data
// dir; nothing when it is not running. tmux sessions alone would miss the
// Claude Code runs that are not in one, so the processes are listed too.
func (a *App) liveWork() (live, error) {
	var l live
	if a.state() != "running" {
		return l, nil
	}
	if out, _ := backend.Output(a.box(), "tmux", "list-sessions", "-F", "#{session_name}"); out != "" {
		l.sessions = strings.Split(out, "\n")
	}
	out, err := backend.Output(a.box(), "bash", "-c", processesScript)
	if err != nil || out == "" {
		return l, fmt.Errorf("cannot list what runs in the %s: %v", a.noun(), or(firstLine(out), fmt.Sprint(err)))
	}
	arg0, ppid := map[string]string{}, map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, " ", 3)
		if len(f) == 3 {
			ppid[f[0]], arg0[f[0]] = f[1], f[2]
		}
	}
	for pid, a0 := range arg0 {
		// A session's Claude Code is a tmux pane's; its own children
		// (subagents, a supervisor it started) are counted with it.
		parent := arg0[ppid[pid]]
		if isClaude(a0) && path.Base(parent) != "tmux" && !isClaude(parent) {
			l.others++
		}
	}
	return l, nil
}

// refuseLiveSessions stops a sync while anything in the container could be
// writing the files it merges. CABOOSE_FORCE=1 overrides, as it does for caboose
// restart.
func (a *App) refuseLiveSessions() error {
	l, err := a.liveWork()
	if err != nil && !a.force() {
		return Die("%v; refusing to sync (set CABOOSE_FORCE=1 to sync anyway)", err)
	}
	if l.none() {
		return nil
	}
	if a.force() {
		a.Note("CABOOSE_FORCE=1 set: syncing while these run:")
		fmt.Fprint(a.Stderr, l.describe())
		return nil
	}
	a.Note("sessions are running, and they write what a sync merges:")
	fmt.Fprint(a.Stderr, l.describe())
	return Die("refusing to sync; end them first (or set CABOOSE_FORCE=1, and restart them after)")
}

func (a *App) reportSync(r *statesync.Report) {
	if r.Committed {
		a.Note("sent this machine's changes")
	}
	if r.Merged {
		a.Note("took %d %s from the remote", len(r.Applied), plural(len(r.Applied), "change", "changes"))
		for _, p := range r.Applied {
			fmt.Fprintf(a.Stderr, "  ~/%s\n", proposal.Printable(p))
		}
	}
	if !r.Committed && !r.Merged {
		a.Note("already in sync")
	} else if r.Pushed {
		a.Note("pushed")
	}
	if len(r.Shadowed) > 0 {
		a.Note("sync rules that never decide anything, earlier ones taking all they match: %s", strings.Join(shownAll(r.Shadowed), ", "))
	}
	if r.SandboxConfig {
		a.Note("the sync changed the sandbox config; a change to what it keeps takes effect at the next 'caboose restart'")
	}
	if len(r.Unmounted) > 0 {
		a.Note("%s, which the %s does not mount yet: they sync after 'caboose restart'", homeList(r.Unmounted), a.noun())
	}
	if len(r.Refused) > 0 {
		a.Note("not synced, being symlinks or hard links (never followed): %s", homeList(r.Refused))
	}
	if len(r.Ignored) > 0 {
		a.Note("left in the sync repo, not synced by this caboose: %s", strings.Join(shownAll(r.Ignored), ", "))
	}
}

// homeList is home-relative paths as ~/... , printable, in one line.
func homeList(rels []string) string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = "~/" + proposal.Printable(r)
	}
	return strings.Join(out, ", ")
}

// resolveConflict asks, on the terminal, how to settle one conflict.
func (a *App) resolveConflict(c *syncagent.Conflict) syncagent.Reply {
	abort := syncagent.Reply{Abort: true}
	t, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return abort
	}
	defer t.Close()
	in := bufio.NewReader(t)

	a.Note("both machines changed %s since the last sync", proposal.Printable(c.Path))
	switch {
	case len(c.Keys) > 0:
		fmt.Fprintf(a.Stderr, "  keys in conflict: %s (the rest merged)\n", strings.Join(shownAll(c.Keys), ", "))
	case c.Note != "":
		fmt.Fprintf(a.Stderr, "  %s\n", proposal.Printable(c.Note))
	}
	if c.OursDeleted {
		fmt.Fprintln(a.Stderr, "  this machine deleted it; the remote changed it")
	}
	if c.TheirsDeleted {
		fmt.Fprintln(a.Stderr, "  the remote deleted it; this machine changed it")
	}
	if c.MarkersErr != "" {
		fmt.Fprintf(a.Stderr, "  no [e]dit: %s\n", shown(c.MarkersErr))
	}
	for {
		fmt.Fprint(a.Stderr, "caboose: [o] keep this machine's, [t] take the remote's, [e] edit, [a] abort? ")
		reply, err := in.ReadString('\n')
		if err != nil && reply == "" {
			return abort
		}
		switch strings.ToLower(strings.TrimSpace(reply)) {
		case "o":
			return syncagent.Reply{Take: syncagent.TakeOurs}
		case "t":
			return syncagent.Reply{Take: syncagent.TakeTheirs}
		case "e":
			if res, ok := a.editConflict(c, t); ok {
				return syncagent.Reply{Resolution: &res}
			}
		case "a":
			return abort
		}
	}
}

// conflictExt is a file name extension safe to give the temp file an
// editor opens: it picks the editor's syntax, and the path is the
// sandbox's to name.
var conflictExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

// editConflict opens c, with conflict markers, in the user's editor, and
// takes the result once no marker is left (and, for JSON, once it parses).
func (a *App) editConflict(c *syncagent.Conflict, term *os.File) (statesync.Resolution, bool) {
	if c.MarkersErr != "" {
		a.Note("could not mark up the conflict: %s", shown(c.MarkersErr))
		return statesync.Resolution{}, false
	}
	ext := filepath.Ext(c.Path)
	if !conflictExt.MatchString(ext) {
		ext = ""
	}
	f, err := os.CreateTemp("", "caboose-conflict-*"+ext)
	if err != nil {
		a.Note("%v", err)
		return statesync.Resolution{}, false
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(c.Markers); err != nil {
		f.Close()
		a.Note("%v", err)
		return statesync.Resolution{}, false
	}
	f.Close()

	editor := or(a.getenv("VISUAL"), or(a.getenv("EDITOR"), "vi"))
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "editor", f.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = term, term, term
	if err := cmd.Run(); err != nil {
		a.Note("%s: %v", editor, err)
		return statesync.Resolution{}, false
	}
	out, err := os.ReadFile(f.Name())
	if err != nil {
		a.Note("%v", err)
		return statesync.Resolution{}, false
	}
	if statesync.HasMarkers(out) {
		a.Note("conflict markers are still in it; edit again, or pick a side")
		return statesync.Resolution{}, false
	}
	if c.JSON {
		if err := statesync.ValidJSON(out); err != nil {
			a.Note("that is not valid JSON (%v); edit again, or pick a side", err)
			return statesync.Resolution{}, false
		}
	}
	return statesync.Resolution{Content: out}, true
}

// askPrompt asks, on the terminal, what git or ssh in the sandbox asks: a
// host key to accept, a username, a password or a passphrase. What is
// typed is not shown unless the host knows the question for one whose
// answer is no secret (askText). Nothing to ask on refuses it.
func (a *App) askPrompt(q *syncagent.Ask) syncagent.Reply {
	refuse := syncagent.Reply{Abort: true}
	t, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return refuse
	}
	defer t.Close()
	text, echo := askText(a.noun(), q.Prompt)
	fmt.Fprint(t, text)
	answer, ok := readAnswer(t, echo)
	if !ok {
		return refuse
	}
	return syncagent.Reply{Answer: &answer}
}

var (
	// hostKeyQuestion is the last line of ssh's question about a host key
	// it has not seen; usernameQuestion git's for a username. Their
	// answers are no secret, and are shown as they are typed.
	hostKeyQuestion  = regexp.MustCompile(`^Are you sure you want to continue connecting \(yes/no(/\[fingerprint\])?\)\? ?$`)
	usernameQuestion = regexp.MustCompile(`^Username for '[^']*': ?$`)
)

// askText is the prompt the sandbox sent as caboose shows it -- labelled
// as the sandbox's, each of its lines behind a mark of their own, so none
// passes for caboose's -- and whether what is typed is shown.
func askText(noun, prompt string) (text string, echo bool) {
	lines := strings.Split(strings.TrimRight(prompt, "\n"), "\n")
	last := strings.TrimRight(lines[len(lines)-1], "\r")
	echo = hostKeyQuestion.MatchString(last) || len(lines) == 1 && usernameQuestion.MatchString(last)
	var b strings.Builder
	fmt.Fprintf(&b, "caboose: the sync in the %s asks:\n", noun)
	for _, l := range lines {
		fmt.Fprintf(&b, "  | %s\n", proposal.Printable(strings.TrimRight(l, "\r")))
	}
	if echo {
		b.WriteString("  answer: ")
	} else {
		b.WriteString("  answer (not shown): ")
	}
	return b.String(), echo
}

// readAnswer reads one line from the terminal t; with echo off, without
// showing it. False when the line was not finished (end of input, ^C).
func readAnswer(t *os.File, echo bool) (string, bool) {
	if echo {
		line, err := bufio.NewReader(t).ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}
	restore, err := tty.MakeRaw(t.Fd())
	if err != nil {
		return "", false
	}
	defer func() {
		restore()
		fmt.Fprint(t, "\n")
	}()
	var line []byte
	b := make([]byte, 1)
	for {
		if n, err := t.Read(b); err != nil || n == 0 {
			return "", false
		}
		switch b[0] {
		case '\r', '\n':
			return string(line), true
		case 3, 4: // ^C, ^D
			return "", false
		case 127, 8:
			if len(line) > 0 {
				line = line[:len(line)-1]
			}
		default:
			line = append(line, b[0])
		}
	}
}
