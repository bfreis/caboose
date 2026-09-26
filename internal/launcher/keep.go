package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// sandboxProblems is where the host tells sessions what is wrong with the
// sandbox config, in proposals/current: the one place the host writes and
// sessions read.
const sandboxProblems = "sandbox-config.txt"

// rootNames are the names of the configured roots, none for a single one:
// what a sandbox config written now expects.
func (a *App) rootNames() []string {
	var names []string
	for _, r := range a.Cfg.Roots {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	return names
}

// sandboxConfig is the sandbox config in effect (datadir.LoadSandboxConfig),
// read once per run.
func (a *App) sandboxConfig() (*datadir.Sandbox, error) {
	if a.sb == nil {
		sb, err := datadir.LoadSandboxConfig(a.Cfg.DataDir, a.rootNames())
		if err != nil {
			return nil, fmt.Errorf("reading the sandbox config: %v", err)
		}
		a.sb = sb
	}
	return a.sb, nil
}

// writeSandboxDefaults writes the defaults as the sandbox config, when
// there is none, unless a sync could still bring one: with a remote set,
// that is the sync's to do (statesync.Syncer.Defaults), or two machines
// would each write their own and conflict on their first sync.
func (a *App) writeSandboxDefaults() error {
	sb, err := a.sandboxConfig()
	if err != nil || !sb.Absent || a.newSyncer(nil).MaybeRemote() {
		return err
	}
	return a.writeDefaultsNow(sb)
}

// writeDefaultsNow writes sb's defaults as the sandbox config. Quietly: it
// is what was in effect already, now where it can be read and changed.
func (a *App) writeDefaultsNow(sb *datadir.Sandbox) error {
	if err := os.MkdirAll(a.Cfg.DataDir, 0o777); err != nil {
		return err
	}
	if err := nofollow.Dir(a.Cfg.DataDir).WriteFile(datadir.SandboxConfig, sb.Data, 0o644); err != nil {
		return fmt.Errorf("writing the sandbox config: %v", err)
	}
	sb.Absent = false
	return nil
}

// noteSandboxConfig says, in a line or two, when the sandbox config could
// not be used, or had entries skipped, and tells sessions the same in
// proposals/current (best-effort: a session without it still has doctor's
// word, through the user).
func (a *App) noteSandboxConfig(sb *datadir.Sandbox) {
	lines := sandboxProblemLines(sb)
	d := nofollow.Dir(a.Cfg.DataDir)
	rel := path.Join(proposal.Dir, proposal.CurrentDir, sandboxProblems)
	if len(lines) == 0 {
		if err := d.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.Note("not telling sessions about the sandbox config: %v", err)
		}
		return
	}
	text := "# Written by caboose on the host, at every launch: what is wrong with\n" +
		"# " + sandboxcfg.HomePath + ". Changing this file changes nothing; fix that one.\n\n" +
		strings.Join(lines, "\n") + "\n"
	if err := d.WriteFile(rel, []byte(text), 0o644); err != nil {
		a.Note("not telling sessions about the sandbox config: %v", err)
	}
	if sb.Err != nil {
		a.Note("%s", lines[0])
	}
	if n := len(sb.Problems); n > 0 {
		a.Note("the sandbox config has %s; 'caboose doctor' lists %s", plural(n, "a problem", fmt.Sprintf("%d problems", n)), plural(n, "it", "them"))
	}
}

// sandboxProblemLines are sb's problems, one line each: first why the file
// could not be used at all, when it could not.
func sandboxProblemLines(sb *datadir.Sandbox) []string {
	var lines []string
	if sb.Err != nil {
		instead := "the defaults"
		if sb.LastGood {
			instead = "the last copy of it that could be read"
		}
		lines = append(lines, fmt.Sprintf("%s cannot be used: %v; %s stands in, and nothing syncs until it is fixed%s",
			sandboxcfg.HomePath, sb.Err, instead, map[bool]string{true: " ('caboose update' reads a newer format)"}[errors.Is(sb.Err, sandboxcfg.ErrNewerFormat)]))
	}
	for _, p := range sb.Problems {
		lines = append(lines, proposal.Printable(p))
	}
	return lines
}

// keepMounts are the docker run arguments that mount sb's keep entries.
func keepMounts(dataDir string, sb *datadir.Sandbox) []string {
	var args []string
	for _, k := range sb.Keep {
		args = append(args, "-v", filepath.Join(dataDir, datadir.HomeDir, filepath.FromSlash(k.Rel))+":"+config.ContainerHome+"/"+k.Rel)
	}
	return args
}

// mountedKeeps are the home-relative paths the container has mounted from
// the data dir's home/: the keep entries it was created with. ok is false
// when the container cannot be asked (it does not exist, or docker does not
// answer): then the sandbox config is about to be the truth.
func (a *App) mountedKeeps() (rels []string, ok bool) {
	mounts, err := a.Docker.Mounts(a.Cfg.Container)
	if err != nil || len(mounts) == 0 {
		return nil, false
	}
	// Docker records a source as it was given, which may be the data dir's
	// physical path rather than the configured one.
	homes := []string{filepath.Join(a.Cfg.DataDir, datadir.HomeDir)}
	if p, err := config.Physical(a.Cfg.DataDir); err == nil && p != a.Cfg.DataDir {
		homes = append(homes, filepath.Join(p, datadir.HomeDir))
	}
	for _, m := range mounts {
		for _, h := range homes {
			if rel, ok := strings.CutPrefix(filepath.Clean(m.Source), h+"/"); ok {
				rels = append(rels, filepath.ToSlash(rel))
				break
			}
		}
	}
	slices.Sort(rels)
	return rels, true
}

// keepDrift says how the running container's kept paths differ from the
// sandbox config's, as what follows "the container", or "" when they do
// not (or it cannot say).
func (a *App) keepDrift() string {
	mounted, ok := a.mountedKeeps()
	sb, err := a.sandboxConfig()
	if !ok || err != nil {
		return ""
	}
	var want []string
	for _, k := range sb.Keep {
		want = append(want, k.Rel)
	}
	slices.Sort(want)
	if slices.Equal(mounted, want) {
		return ""
	}
	var added, gone []string
	for _, w := range want {
		if !slices.Contains(mounted, w) {
			added = append(added, "~/"+w)
		}
	}
	for _, m := range mounted {
		if !slices.Contains(want, m) {
			gone = append(gone, "~/"+m)
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "does not keep "+strings.Join(added, ", ")+", which the sandbox config now does")
	}
	if len(gone) > 0 {
		parts = append(parts, "keeps "+strings.Join(gone, ", ")+", which the sandbox config no longer does")
	}
	return strings.Join(parts, ", and ")
}

// warnIfKeepDrifted says when the running container keeps other paths than
// the sandbox config now says. Only a warning, as for the roots: the fix, a
// restart, kills sessions.
func (a *App) warnIfKeepDrifted() {
	if d := a.keepDrift(); d != "" {
		a.Note("the container %s.", d)
		a.Note("run 'caboose restart' to remount (this kills running sessions).")
	}
}
