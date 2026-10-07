package launcher

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/syncagent"
	"github.com/bfreis/caboose/internal/tty"
)

// The sandbox config's own commands: caboose sync add/rm, which edit its
// sync rules, caboose sync status, and caboose sandbox-config update. They
// run on the host, on the data dir's copy of ~/.config/caboose/sandbox.toml;
// a session edits the same file from inside.

// readSandboxFile is the sandbox config's text as the file has it, or the
// defaults when there is none (absent set).
func (a *App) readSandboxFile() (data []byte, absent bool, err error) {
	data, _, err = nofollow.Dir(a.Cfg.DataDir).ReadFile(datadir.SandboxConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return sandboxcfg.Default(a.rootPaths()), true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %v", sandboxcfg.HomePath, err)
	}
	return data, false, nil
}

// writeSandboxFile writes the sandbox config: beside it and renamed over
// it, never through a link (the sandbox writes the directory too).
func (a *App) writeSandboxFile(data []byte) error {
	if err := nofollow.Dir(a.Cfg.DataDir).WriteFile(datadir.SandboxConfig, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %v", sandboxcfg.HomePath, err)
	}
	a.sb = nil
	return nil
}

// SyncEdit is caboose sync add PATH and caboose sync rm PATH: a sync rule
// added to the sandbox config, or removed from it. PATH is a path in the
// sandbox's home, ~/... or /home/agent/...
func (a *App) SyncEdit(op string, args []string) error {
	if len(args) != 1 {
		return Die("usage: caboose sync %s PATH (a path in the sandbox's home: ~/...)", op)
	}
	data, _, err := a.readSandboxFile()
	if err != nil {
		return Die("%v", err)
	}
	edit := sandboxcfg.AddSync
	if op == "rm" {
		edit = sandboxcfg.RemoveSync
	}
	out, msg, err := edit(data, args[0])
	switch {
	case errors.Is(err, sandboxcfg.ErrNotSynced):
		a.Note("%s; nothing to change", proposal.Printable(err.Error()))
		return nil
	case err != nil:
		return Die("%s", proposal.Printable(err.Error()))
	}
	if string(out) != string(data) {
		if err := a.writeSandboxFile(out); err != nil {
			return Die("%v", err)
		}
	}
	a.Note("%s", proposal.Printable(msg))
	if string(out) != string(data) {
		a.Note("the next sync syncs by it; %s", a.syncHow())
	}
	return nil
}

// SyncStatus is caboose sync status: what this machine would send, and, if
// the sandbox runs (the sync runs there), what the remote has for it to
// take. It changes nothing but what a fetch writes into the sync repo
// (and, from a terminal, a host key accepted when ssh asks, as caboose sync
// would).
func (a *App) SyncStatus(args []string) error {
	if len(args) > 0 {
		return Die("usage: caboose sync status")
	}
	if !statesync.MaybeRemote(a.syncRepo()) {
		return Die("no sync remote yet; set one once with\n  caboose sync --remote URL")
	}
	unlock, err := statesync.Lock(a.Cfg.DataDir)
	if errors.Is(err, statesync.ErrLocked) {
		return Die("%v; try again when it is done", err)
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()
	out := a.Stdout
	fmt.Fprintf(out, "remote  : %s\n", proposal.Printable(or(statesync.RemoteHint(a.syncRepo()), "set")))
	list := func(title string, paths []string, mark string) {
		fmt.Fprintf(out, "%-8s: %s\n", title, plural(len(paths), "1 file", fmt.Sprintf("%d files", len(paths))))
		for _, x := range paths {
			fmt.Fprintf(out, "  %s %s\n", mark, proposal.Printable(x))
		}
	}
	pending := func(st *syncagent.Status) {
		list("to send", st.Changed, "M")
		for _, x := range st.Deleted {
			fmt.Fprintf(out, "  D %s\n", proposal.Printable(x))
		}
		if len(st.Refused) > 0 {
			fmt.Fprintf(out, "refused : %s (symlinks or hard links: never followed)\n", homeList(st.Refused))
		}
		if len(st.Secrets) > 0 {
			fmt.Fprintf(out, "secrets : %s (every sync refuses until they are gone)\n", strings.Join(shownAll(st.Secrets), ", "))
		}
		if len(st.Unmounted) > 0 {
			fmt.Fprintf(out, "later   : %s (not mounted until 'caboose restart', and not synced until then)\n", homeList(st.Unmounted))
		}
	}

	if a.state() != "running" {
		st, err := a.localPending()
		if err != nil {
			return Die("%v", err)
		}
		pending(st)
		fmt.Fprintf(out, "to take : not checked: the %s is not running, and the sync runs there\n", a.noun())
		return nil
	}
	mounts, err := a.checkSyncMount()
	if err != nil {
		return err
	}
	// Under vm the fetch's ssh goes through the outbound proxy, which the
	// link serves: started and waited for as caboose sync does, since a VM
	// some other command brought up need not have its link yet.
	if a.isVM() {
		a.startLink()
		a.awaitProxy()
	}
	req, err := a.syncRequest(mounts)
	if err != nil {
		return Die("%v", err)
	}
	// From a terminal the fetch is caboose sync's: it may ask to accept the
	// remote's host key, or for credentials, and is not cut short. Only a
	// status nobody can answer (a script, a pipe) fails instead of asking.
	interactive := tty.IsTerminal(os.Stdin.Fd()) && tty.IsTerminal(os.Stderr.Fd())
	var h syncagent.Handler
	var show io.Writer
	if interactive {
		req.Interactive, req.ShowGit = true, true
		h.Ask, show = a.askPrompt, os.Stderr
		h.Interrupted = a.interrupted
	} else {
		req.Auto, req.Budget = true, budgetSecs(doctorBudget)
		h.Timeout = doctorBudget + syncMargin
	}
	res, err := a.runSync(syncagent.OpStatus, req, h, show)
	if err != nil {
		return err
	}
	if res.Err != nil {
		if res.Err.Kind == syncagent.KindNoRemote {
			return Die("no sync remote yet; set one once with\n  caboose sync --remote URL")
		}
		return Die("%s", shown(res.Err.Message))
	}
	st := res.Status
	if st == nil {
		return Die("the sync in the %s said nothing of its state", a.noun())
	}
	pending(st)
	// What a sync committed here and never pushed is no change of the
	// files any more, so "to send" does not count it: say it apart.
	if u := st.Unsent; len(u.Paths) > 0 {
		list(fmt.Sprintf("unsent (%s never pushed)", plural(u.Commits, "1 commit", fmt.Sprintf("%d commits", u.Commits))), u.Paths, "M")
		fmt.Fprintf(out, "          'caboose sync' sends %s\n", plural(len(u.Paths), "it", "them"))
	}
	if st.FetchErr != "" {
		if interactive {
			// git said why on the terminal, above.
			fmt.Fprintf(out, "to take : not checked: the fetch failed, as git says above\n")
			return nil
		}
		what, fix := fetchFailure(errors.New(st.FetchErr))
		fmt.Fprintf(out, "to take : not checked: %s; %s\n", shown(what), fix)
		return nil
	}
	list("to take", st.Taken, "<")
	if len(st.Others) > 0 {
		fmt.Fprintf(out, "left    : %s the remote has that this machine's rules do not sync (left in the repo)\n",
			plural(len(st.Others), "1 file", fmt.Sprintf("%d files", len(st.Others))))
	}
	return nil
}

// localPending is what this machine has not sent, read on the host, from
// the data dir: for when the sandbox is not up to ask. It only reads.
func (a *App) localPending() (*syncagent.Status, error) {
	sb, err := a.sandboxConfig()
	if err != nil {
		return nil, err
	}
	s := &statesync.Syncer{
		Home: filepath.Join(a.Cfg.DataDir, datadir.HomeDir), Repo: a.syncRepo(),
		Sandbox: sb, Roots: a.rootPaths(),
	}
	return syncagent.PendingStatus(s)
}

// SandboxConfig is caboose sandbox-config update: the sandbox config brought
// up to this caboose -- its format, then each default added since it was
// written, offered one by one -- or written, when there is none.
func (a *App) SandboxConfig(args []string) error {
	if len(args) != 1 || args[0] != "update" {
		return Die("usage: caboose sandbox-config update")
	}
	data, absent, err := a.readSandboxFile()
	if err != nil {
		return Die("%v", err)
	}
	if absent {
		if err := a.writeSandboxFile(data); err != nil {
			return Die("%v", err)
		}
		a.Note("wrote the sandbox config, %s, from this caboose's defaults", sandboxcfg.HomePath)
		return nil
	}
	c, err := sandboxcfg.Parse(data)
	switch {
	case errors.Is(err, sandboxcfg.ErrNewerFormat):
		return Die("%s: %v; 'caboose update' installs a caboose that reads it", sandboxcfg.HomePath, err)
	case err != nil:
		return Die("%s: %v; fix it first", sandboxcfg.HomePath, err)
	}
	out := data
	if c.Format < sandboxcfg.Format {
		if out, err = sandboxcfg.Migrate(out, c.Format); err != nil {
			return Die("%s: %v", sandboxcfg.HomePath, err)
		}
		a.Note("brought the sandbox config from format %d to %d", c.Format, sandboxcfg.Format)
	}
	if pending := sandboxcfg.Pending(c); len(pending) > 0 {
		term, err := a.openTerminal()
		if err != nil {
			return Die("there %s to offer, and no terminal to ask on (%v); run it in one", plural(len(pending), "is a new default", fmt.Sprintf("are %d new defaults", len(pending))), err)
		}
		defer term.Close()
		p := a.newSetupPrompter(term)
		for _, add := range pending {
			p.say("%s", add.Summary)
			p.say("%s", add.Text)
			yes, err := p.yesNo("Add it?", true)
			if err != nil {
				return err
			}
			if yes {
				out = sandboxcfg.Append(out, add)
			}
		}
	}
	out = sandboxcfg.Stamp(out, sandboxcfg.Format, sandboxcfg.DefaultsVersion)
	if string(out) == string(data) {
		a.Note("the sandbox config is up to date")
		return nil
	}
	if _, err := sandboxcfg.Parse(out); err != nil {
		return Die("the updated sandbox config would not parse (%v); nothing was written", err)
	}
	if err := a.writeSandboxFile(out); err != nil {
		return Die("%v", err)
	}
	a.Note("updated %s; a new keep entry takes effect at the next 'caboose restart'", sandboxcfg.HomePath)
	return nil
}

// afterSync is what follows a sync that went through: when it changed the
// sandbox config, what is wrong with the new one, and a word when what
// the sandbox keeps changed with it.
func (a *App) afterSync(r *statesync.Report) {
	if r == nil || !r.SandboxConfig {
		return
	}
	before := keptRels(a.sb)
	a.sb = nil
	sb, err := a.sandboxConfig()
	if err != nil {
		return
	}
	a.noteSandboxConfig(sb)
	if before != nil && !slices.Equal(before, keptRels(sb)) {
		a.warnIfKeepDrifted()
	}
}

// keptRels are sb's keep entries, home-relative; nil for no sb.
func keptRels(sb *datadir.Sandbox) []string {
	if sb == nil {
		return nil
	}
	rels := []string{}
	for _, k := range sb.Keep {
		rels = append(rels, k.Rel)
	}
	return rels
}
