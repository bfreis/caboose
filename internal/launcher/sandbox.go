package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
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
		return sandboxcfg.Default(a.rootNames()), true, nil
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
// the container runs (the sync's git runs there), what the remote has for
// it to take. It changes nothing but what a fetch writes into the sync repo
// (and, from a terminal, a host key accepted when ssh asks, as caboose sync
// would).
func (a *App) SyncStatus(args []string) error {
	if len(args) > 0 {
		return Die("usage: caboose sync status")
	}
	s := a.newSyncer(nil)
	if !s.MaybeRemote() {
		return Die("no sync remote yet; set one once with\n  caboose sync --remote URL")
	}
	unlock, err := s.Lock()
	if errors.Is(err, statesync.ErrLocked) {
		return Die("%v; try again when it is done", err)
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()
	out := a.Stdout
	fmt.Fprintf(out, "remote  : %s\n", proposal.Printable(or(s.RemoteHint(), "set")))

	p, err := s.Pending()
	if err != nil {
		return Die("%v", err)
	}
	list := func(title string, paths []string, mark string) {
		fmt.Fprintf(out, "%-8s: %s\n", title, plural(len(paths), "1 file", fmt.Sprintf("%d files", len(paths))))
		for _, x := range paths {
			fmt.Fprintf(out, "  %s %s\n", mark, proposal.Printable(x))
		}
	}
	list("to send", p.Changed, "M")
	for _, x := range p.Deleted {
		fmt.Fprintf(out, "  D %s\n", proposal.Printable(x))
	}
	if len(p.Export.Refused) > 0 {
		fmt.Fprintf(out, "refused : %s (symlinks or hard links: never followed)\n", strings.Join(p.Export.Refused, ", "))
	}
	var se *statesync.SecretsError
	if err := p.Export.ScanSecrets(); errors.As(err, &se) {
		fmt.Fprintf(out, "secrets : %s (every sync refuses until they are gone)\n", strings.Join(se.Paths, ", "))
	}

	if a.state() != "running" {
		fmt.Fprintf(out, "to take : not checked: the %s is not running, and the sync's git runs there\n", a.noun())
		return nil
	}
	if err := a.checkSyncMount(); err != nil {
		return err
	}
	// Under vm the fetch's ssh goes through the outbound proxy, which the
	// link serves: started and waited for as caboose sync does, since a VM
	// some other command brought up need not have its link yet.
	if a.isVM() {
		a.startLink()
		a.awaitProxy()
	}
	// From a terminal the fetch is caboose sync's: it may ask to accept the
	// remote's host key, or for credentials, and is not cut short. Only a
	// status nobody can answer (a script, a pipe) fails instead of asking.
	interactive := tty.IsTerminal(os.Stdin.Fd()) && tty.IsTerminal(os.Stderr.Fd())
	g := a.newSyncGit(!interactive)
	if interactive {
		s.Stderr = os.Stderr
	} else {
		g.deadline = time.Now().Add(doctorBudget)
	}
	s.Git = g.command
	if err := s.Fetch(); err != nil {
		if interactive {
			// git said why on the terminal, above.
			fmt.Fprintf(out, "to take : not checked: the fetch failed, as git says above\n")
			return nil
		}
		what, fix := fetchFailure(err)
		fmt.Fprintf(out, "to take : not checked: %s; %s\n", proposal.Printable(what), fix)
		return nil
	}
	taken, others, err := s.Incoming()
	if err != nil {
		return Die("%v", err)
	}
	list("to take", taken, "<")
	if len(others) > 0 {
		fmt.Fprintf(out, "left    : %s the remote has that this machine's rules do not sync (left in the repo)\n",
			plural(len(others), "1 file", fmt.Sprintf("%d files", len(others))))
	}
	return nil
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
