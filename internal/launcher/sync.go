package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/tty"
)

// Sync backs up and syncs the portable part of the data dir -- memories,
// settings, skills, agents, commands, MCP servers -- through a git remote.
// See internal/statesync for what syncs and how it merges.
//
// The files are read and written here, on the host, but every git command
// runs in the container, through `docker exec`, on the sync repo the
// container mounts at statesync.ContainerDir: the host needs no git, and
// neither its git config, hooks and credential helpers nor anything a
// remote sends come near it. Pushes go out with what the sandbox has -- the
// forwarded SSH agent, gh's token, its own git config.
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
	if err := a.checkSyncMount(); err != nil {
		return err
	}
	if v, err := backend.Output(a.box(), "git", "--version"); err != nil {
		return Die("git does not run in the %s (%v); the image needs git 2.28 or later, see 'caboose check-image'", a.noun(), err)
	} else if v == "" {
		return Die("git in the %s reports no version; the image needs git 2.28 or later", a.noun())
	}

	s := a.newSyncer(a.newSyncGit(false))
	interactive := tty.IsTerminal(os.Stdin.Fd())
	if interactive {
		s.Resolve = a.resolveConflict
	}
	if tty.IsTerminal(os.Stderr.Fd()) {
		s.Stderr = os.Stderr
	}

	unlock, err := s.Lock()
	if errors.Is(err, statesync.ErrLocked) {
		return Die("%v; try again when it is done", err)
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()

	if err := s.Init(); err != nil {
		return Die("setting up %s: %v", s.RepoDir(), err)
	}
	if remote != "" {
		if err := s.SetRemote(remote); err != nil {
			return Die("setting the sync remote: %v", err)
		}
	}
	url := s.Remote()
	if url == "" {
		return Die("no sync remote yet; set one once with\n" +
			"  caboose sync --remote URL\n" +
			"any git URL the host can push to, such as an empty private repo")
	}
	a.Note("syncing %s with %s", a.Cfg.DataDir, url)

	r, err := s.Sync()
	if r != nil {
		a.reportSync(r)
	}
	var se *statesync.SecretsError
	switch {
	case err == nil:
		a.afterSync(r)
		return nil
	case errors.As(err, &se):
		return Die("%v", err)
	case errors.Is(err, statesync.ErrAborted):
		msg := "%v; nothing in %s changed"
		if !interactive {
			msg += " (run it from a terminal to resolve the conflict)"
		}
		return Die(msg, err, a.Cfg.DataDir)
	}
	return Die("sync failed: %v", err)
}

// newSyncer is a Syncer for this data dir, running git with g. A nil g is
// for a Syncer that only locks, or that is given its git later: never the
// host's git, which statesync would otherwise default to.
func (a *App) newSyncer(g *syncGit) *statesync.Syncer {
	host, _ := os.Hostname()
	return &statesync.Syncer{
		DataDir:  a.Cfg.DataDir,
		Host:     host,
		Defaults: sandboxcfg.Default(a.rootNames()),
		Git:      g.command, // on a nil g, a call panics rather than run host git
		GitDir:   statesync.ContainerDir,
	}
}

// checkSyncMount makes sure the container mounts this data dir's sync repo
// where the container's git will look for it: one created on another data
// dir mounts that one's, and the mounts are fixed at creation.
func (a *App) checkSyncMount() error {
	mounts, err := a.box().Mounts()
	if err != nil {
		return a.boxFailed(err)
	}
	want := filepath.Join(a.Cfg.DataDir, statesync.Dir)
	for _, m := range mounts {
		if m.Target != statesync.ContainerDir {
			continue
		}
		if m.Source != want {
			return Die("the %s mounts %s at %s, but this data dir's sync repo is %s; run 'caboose restart' to recreate it on this data dir",
				a.noun(), m.Source, statesync.ContainerDir, want)
		}
		return nil
	}
	return Die("the %s does not mount %s at %s, where the sync's git runs;\n"+
		"run 'caboose restart' to recreate it (nothing is lost; it asks before ending sessions)", a.noun(), want, statesync.ContainerDir)
}

// live is what in the container could be writing the data dir.
type live struct {
	// sessions are the tmux sessions.
	sessions []string
	// others counts the Claude Code processes in none of them: a
	// CABOOSE_NO_TMUX session or a `caboose claude -p` run (plain docker execs),
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
		fmt.Fprintf(&b, "  %d Claude Code %s outside tmux (CABOOSE_NO_TMUX, caboose claude -p, background agents)\n",
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
// writing the files it merges. FORCE=1 overrides, as it does for caboose
// restart.
func (a *App) refuseLiveSessions() error {
	l, err := a.liveWork()
	if err != nil && a.getenv("FORCE") == "" {
		return Die("%v; refusing to sync (set FORCE=1 to sync anyway)", err)
	}
	if l.none() {
		return nil
	}
	if a.getenv("FORCE") != "" {
		a.Note("FORCE=1 set: syncing while these run:")
		fmt.Fprint(a.Stderr, l.describe())
		return nil
	}
	a.Note("sessions are running, and they write what a sync merges:")
	fmt.Fprint(a.Stderr, l.describe())
	return Die("refusing to sync; end them first (or set FORCE=1, and restart them after)")
}

func (a *App) reportSync(r *statesync.Report) {
	if r.Committed {
		a.Note("sent this machine's changes")
	}
	if r.Merged {
		a.Note("took %d %s from the remote", len(r.Applied), plural(len(r.Applied), "change", "changes"))
		for _, p := range r.Applied {
			fmt.Fprintf(a.Stderr, "  %s\n", p)
		}
	}
	if !r.Committed && !r.Merged {
		a.Note("already in sync")
	} else if r.Pushed {
		a.Note("pushed")
	}
	if len(r.Shadowed) > 0 {
		a.Note("sync rules that never decide anything, earlier ones taking all they match: %s", strings.Join(r.Shadowed, ", "))
	}
	if r.SandboxConfig {
		a.Note("the sync changed the sandbox config; a change to what it keeps takes effect at the next 'caboose restart'")
	}
	if len(r.Refused) > 0 {
		a.Note("not synced, being symlinks or hard links (never followed): %s", strings.Join(r.Refused, ", "))
	}
	if len(r.Ignored) > 0 {
		a.Note("left in the sync repo, not synced by this caboose: %s", strings.Join(r.Ignored, ", "))
	}
}

// resolveConflict asks, on the terminal, how to settle one conflict.
func (a *App) resolveConflict(c statesync.Conflict) (statesync.Resolution, error) {
	t, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return statesync.Resolution{}, statesync.ErrAborted
	}
	defer t.Close()
	in := bufio.NewReader(t)

	a.Note("both machines changed %s since the last sync", c.Path)
	switch {
	case len(c.Keys) > 0:
		fmt.Fprintf(a.Stderr, "  keys in conflict: %s (the rest merged)\n", strings.Join(c.Keys, ", "))
	case c.Note != "":
		fmt.Fprintf(a.Stderr, "  %s\n", c.Note)
	}
	if c.Ours == nil {
		fmt.Fprintln(a.Stderr, "  this machine deleted it; the remote changed it")
	}
	if c.Theirs == nil {
		fmt.Fprintln(a.Stderr, "  the remote deleted it; this machine changed it")
	}
	for {
		fmt.Fprint(a.Stderr, "caboose: [o] keep this machine's, [t] take the remote's, [e] edit, [a] abort? ")
		reply, err := in.ReadString('\n')
		if err != nil && reply == "" {
			return statesync.Resolution{}, statesync.ErrAborted
		}
		switch strings.ToLower(strings.TrimSpace(reply)) {
		case "o":
			return c.Take(statesync.Ours), nil
		case "t":
			return c.Take(statesync.Theirs), nil
		case "e":
			res, ok := a.editConflict(c, t)
			if ok {
				return res, nil
			}
		case "a":
			return statesync.Resolution{}, statesync.ErrAborted
		}
	}
}

// editConflict opens c, with conflict markers, in the user's editor, and
// takes the result once no marker is left (and, for JSON, once it parses).
func (a *App) editConflict(c statesync.Conflict, term *os.File) (statesync.Resolution, bool) {
	data, err := c.Markers()
	if err != nil {
		a.Note("could not mark up the conflict: %v", err)
		return statesync.Resolution{}, false
	}
	f, err := os.CreateTemp("", "caboose-conflict-*"+filepath.Ext(c.Path))
	if err != nil {
		a.Note("%v", err)
		return statesync.Resolution{}, false
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
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
