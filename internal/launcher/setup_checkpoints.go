package launcher

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// ckCandidate is a path setup offers to take checkpoints of: a container
// path (or ~/REL) and the host directory it is.
type ckCandidate struct {
	Path string
	Host string
}

// checkpointCandidates are the paths worth offering, from the roots as
// config.toml has them: each writable part of a root, each wholly writable
// root, and ~/.claude. Only what the sandbox can write is copied, so
// nothing else is offered.
func (a *App) checkpointCandidates() []ckCandidate {
	var out []ckCandidate
	add := func(p, host string) {
		if !slices.ContainsFunc(out, func(c ckCandidate) bool { return c.Path == p }) {
			out = append(out, ckCandidate{p, host})
		}
	}
	for _, r := range a.fileRoots() {
		host := a.hostPath(r.Path)
		for _, w := range r.Writable {
			if w == "." {
				add(r.container(), host)
			} else {
				add(path.Join(r.container(), w), filepath.Join(host, filepath.FromSlash(w)))
			}
		}
	}
	add("~/.claude", filepath.Join(a.Cfg.DataDir, datadir.HomeDir, ".claude"))
	return out
}

// setupCheckpoints asks which paths of the sandbox to take checkpoints of,
// and writes [checkpoints] paths. It says where they are kept, how often
// and for how long, and how each candidate would be copied on this
// machine.
func (a *App) setupCheckpoints(p *prompter) error {
	c := a.Cfg
	store := checkpoint.StoreDir(c.EnvDir)
	p.heading("Checkpoints", "Copies of what the sandbox may write, kept on this machine to restore from. "+
		"Optional: none are taken until you choose paths.")
	p.note("Kept in %s, which no sandbox mounts (deleting the environment's directory deletes them). "+
		"One every 10 minutes while the sandbox runs, and one when it stops; kept every 10 minutes for 2 hours, "+
		"hourly for 48 hours, daily for 7 days. Taken with a clone where the filesystem has one, else by copying.", a.short(store))
	cands := a.checkpointCandidates()
	rows := make([][2]string, len(cands))
	for i, cd := range cands {
		m := hostCkFS.method(cd.Host, store)
		rows[i] = [2]string{strconv.Itoa(i+1) + " " + cd.Path, "← " + a.short(cd.Host) + " (" + m.Name + ")"}
	}
	p.table(rows)
	for _, cd := range cands {
		if m := hostCkFS.method(cd.Host, store); m.Slow {
			p.warn("%s: %s", cd.Path, m.Why)
		}
	}
	def := strings.Join(c.Checkpoints, " ")
	question := "Take checkpoints of which? Numbers or paths separated by spaces; empty for none"
	if def != "" {
		question = "Take checkpoints of which? Numbers or paths separated by spaces; - for none"
	}
	var paths []string
	for {
		answer, err := p.ask(question, def)
		if err != nil {
			return err
		}
		list, why := parseCheckpointAnswer(answer, cands)
		if why == "" {
			paths, err = config.CheckCheckpoints(list)
			if err != nil {
				why = capFirst(err.Error())
			}
		}
		if why != "" {
			p.fail("%s", why)
			continue
		}
		break
	}
	if slices.Equal(paths, c.Checkpoints) || len(paths) == 0 && len(c.Checkpoints) == 0 {
		p.same("Nothing changed")
		return nil
	}
	e := config.Edit{Set: map[string]any{"checkpoints.paths": paths}}
	if len(paths) == 0 {
		e = config.Edit{Unset: []string{"checkpoints.paths"}}
	}
	if _, err := a.writeConfig(e); err != nil {
		return err
	}
	if len(paths) == 0 {
		p.ok("No checkpoints are taken now; the ones taken stay in %s", a.short(store))
	} else {
		p.ok("Wrote %d %s to %s", len(paths), plural(len(paths), "path", "paths"), a.short(filepath.Join(c.EnvDir, config.FileName)))
	}
	p.note("No restart is needed: the daemon reads the list again. caboose checkpoint list shows them; caboose doctor checks them.")
	return nil
}

// parseCheckpointAnswer turns an answer into paths: a number is the
// candidate of that number, anything else a path as written; "-" and
// nothing are none. why is what is wrong with it, or "".
func parseCheckpointAnswer(answer string, cands []ckCandidate) (list []string, why string) {
	if answer == "-" {
		return nil, ""
	}
	for _, f := range strings.Fields(answer) {
		if n, err := strconv.Atoi(f); err == nil {
			if n < 1 || n > len(cands) {
				return nil, fmt.Sprintf("There is no candidate %d: 1 to %d", n, len(cands))
			}
			f = cands[n-1].Path
		}
		list = append(list, f)
	}
	return list, ""
}
