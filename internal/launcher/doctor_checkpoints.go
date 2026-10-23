package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// ckLabel is the label of every checkpoint row.
const ckLabel = "checkpoints"

// Fixes shared by several rows.
const (
	ckFixPaths = "caboose setup checkpoints (or edit [checkpoints] paths in config.toml): a path must be a writable part of a root (caboose setup roots) or a kept path of the home"
	ckFixReach = "narrow the root that holds CABOOSE_HOME (caboose setup roots) or move CABOOSE_HOME out of every directory a sandbox mounts; a sandbox mounted before that keeps its mounts until caboose restart" + endsSessions
)

// doctorCheckpoints is doctor's checkpoints rows for the environment, only
// when [checkpoints] paths is set (a store with nothing configured gets
// one note). It reads: the store is never made, nothing is taken, pruned
// or removed.
func (a *App) doctorCheckpoints(c *checkup, running bool) {
	var sched *daemonproto.CheckpointStatus
	if len(a.Cfg.Checkpoints) > 0 {
		if lv := a.linkView(); lv.Env != nil {
			sched = lv.Env.Checkpoints
		}
	}
	a.doctorCheckpointsWith(c, running, sched, defaultCkSeams())
}

func (a *App) doctorCheckpointsWith(c *checkup, running bool, sched *daemonproto.CheckpointStatus, s ckSeams) {
	if a.Cfg.CabooseHome == "" || a.Cfg.EnvDir == "" {
		if len(a.Cfg.Checkpoints) > 0 {
			c.unchecked(ckLabel, "neither CABOOSE_HOME nor HOME is set")
		}
		return
	}
	store := checkpoint.StoreDir(a.Cfg.EnvDir)
	_, lerr := os.Lstat(store)
	exists := lerr == nil
	if len(a.Cfg.Checkpoints) == 0 && !exists {
		return
	}
	h, err := a.checkpointHealth(sched, s.now())
	if err != nil {
		c.problem(ckLabel, "caboose checkpoint list shows the same error; fix what it names, or remove "+store+" if it holds nothing you need", "cannot read the checkpoints in %s: %v", store, err)
		return
	}
	for _, p := range h.Paths {
		a.doctorCheckpointPath(c, p, store, s)
	}
	if len(h.Paths) > 0 && a.Cfg.Getenv != nil && a.getenv("CABOOSE_DATA_DIR") != "" {
		c.note(ckLabel, "CABOOSE_DATA_DIR is set: the daemon takes checkpoints only for an environment at its default data dir; 'caboose checkpoint create' still takes them")
	}
	if !exists {
		if len(h.Paths) > 0 {
			c.note(ckLabel, "the store %s is not made yet: the first checkpoint makes it", store)
		}
		return
	}
	a.doctorCheckpointStore(c, store, s)
	a.doctorCheckpointSpace(c, store, s)
	if sched != nil && sched.Exposed != "" {
		c.problem(ckLabel, ckFixReach, "no checkpoint is taken: %s", sched.Exposed)
	}
	for _, p := range h.Paths {
		if p.PinsError != "" {
			c.problem(ckLabel, "move "+filepath.Join(store, "pins.json")+" aside if it holds nothing you need (the checkpoints it pinned are then pruned by the rules)",
				"the pins cannot be read, so nothing is pruned: %s", p.PinsError)
			break
		}
	}
	for _, p := range h.Paths {
		a.doctorCheckpointPathState(c, h, p, running)
	}
	a.doctorCheckpointNotes(c, h, store, s)
}

// doctorCheckpointPath says whether a path resolves and the method its
// checkpoints get.
func (a *App) doctorCheckpointPath(c *checkup, p ckPathHealth, store string, s ckSeams) {
	if !p.Resolved {
		c.problem(ckLabel, ckFixPaths, "%s: not copied: %s", p.Path, p.Reason)
		return
	}
	kind := "a writable part of a root"
	if p.Source.Kind == checkpoint.SourceHome {
		kind = "a kept path of the home"
	}
	c.ok(ckLabel, "%s: %s, copied from %s", p.Path, kind, p.Source.Host())
	m := s.fs.method(p.Source.Host(), store)
	switch {
	case m.Slow && s.fs.goos == "darwin":
		c.note(ckLabel, "warning: %s: %s (%s)", p.Path, m.Why, m.Name)
	case m.Slow:
		c.note(ckLabel, "%s: %s (%s)", p.Path, m.Why, m.Name)
	default:
		c.ok(ckLabel, "%s: %s (%s)", p.Path, m.Name, m.Why)
	}
}

// doctorCheckpointStore checks the store that exists: a directory with
// mode 0700, and out of every sandbox's reach.
func (a *App) doctorCheckpointStore(c *checkup, store string, s ckSeams) {
	fi, err := os.Lstat(store)
	switch {
	case err != nil:
		c.unchecked(ckLabel, "the store: %v", err)
		return
	case !fi.IsDir():
		c.problem(ckLabel, "move "+store+" aside (it must be a real directory, not a link or a file); the next checkpoint makes it again",
			"the store %s is not a directory (%s)", store, fi.Mode().Type())
		return
	case fi.Mode().Perm() != 0o700:
		c.problem(ckLabel, "chmod 700 "+store, "the store %s has mode %04o, not 0700", store, fi.Mode().Perm())
	default:
		c.ok(ckLabel, "the store %s has mode 0700", store)
	}
	st, err := checkpoint.Open(a.Cfg.CabooseHome, a.Cfg.EnvDir)
	if err != nil {
		c.problem(ckLabel, "move CABOOSE_HOME or the environment's directory so the store is inside CABOOSE_HOME", "the store cannot be opened: %v", err)
		return
	}
	defer st.Close()
	reach, err := a.checkpointReach(s.apps)
	if err != nil {
		c.unchecked(ckLabel, "whether a sandbox can reach the store: %v", err)
		return
	}
	switch err := st.CheckReach(reach); {
	case errors.Is(err, checkpoint.ErrExposed):
		c.problem(ckLabel, ckFixReach, "%v", err)
	case err != nil:
		c.unchecked(ckLabel, "whether a sandbox can reach the store: %v", err)
	default:
		c.ok(ckLabel, "no root or mount of any environment reaches the store")
	}
}

// doctorCheckpointSpace checks the free space of the store's volume
// against the floors a take is judged by.
func (a *App) doctorCheckpointSpace(c *checkup, store string, s ckSeams) {
	avail, total, err := s.fs.statfs(nearestExisting(store))
	if err != nil {
		c.unchecked(ckLabel, "free space: %v", err)
		return
	}
	soft, hard := checkpoint.Floors(total)
	free := sizeString(int64(avail))
	switch {
	case avail < hard:
		c.problem(ckLabel, "free space on the volume that holds "+store+", or move CABOOSE_HOME to a larger one",
			"only %s free where the store is, below the hard floor of %s: no checkpoint is being taken", free, sizeString(int64(hard)))
	case avail < soft:
		c.problem(ckLabel, "free space on the volume that holds "+store+", or move CABOOSE_HOME to a larger one",
			"only %s free where the store is, below the soft floor of %s: the daemon skips its checkpoints until there is more ('caboose checkpoint create' still takes one, down to %s), and prunes none early for it", free, sizeString(int64(soft)), sizeString(int64(hard)))
	default:
		c.ok(ckLabel, "%s free where the store is", free)
	}
}

// doctorCheckpointPathState checks what the checkpoints of a resolved path
// show: a take that fails, one too old while the sandbox runs, a sharp
// shrink.
func (a *App) doctorCheckpointPathState(c *checkup, h *ckHealth, p ckPathHealth, running bool) {
	if !p.Resolved {
		return
	}
	last := ""
	switch {
	case h.Sched != nil && h.Sched.SpaceSkipped != "":
		last = "; they are skipped: the disk is below its soft floor"
	case h.Sched != nil && h.Sched.LastError != "":
		last = "; last error: " + h.Sched.LastError
	}
	switch {
	case p.Failing != "":
		c.problem(ckLabel, "caboose checkpoint create "+p.Path+" tries now and shows the error; caboose daemon restart if the daemon is stuck",
			"%s: the last checkpoint failed: %s", p.Path, p.Failing)
	case running && p.HasNewest && h.Now.Sub(p.Newest) > ckStale:
		c.problem(ckLabel, "caboose checkpoint create "+p.Path+" tries now and shows the error; caboose daemon restart if the daemon is stuck",
			"%s: the newest checkpoint is %s, and one is due every 10 minutes while the sandbox runs%s", p.Path, ckAgo(p.Newest, h.Now), last)
	case p.HasNewest:
		c.ok(ckLabel, "%s: newest checkpoint %s (%s), %d kept", p.Path, ckAgo(p.Newest, h.Now), p.Method, p.Count)
	default:
		c.note(ckLabel, "%s: none taken yet%s", p.Path, last)
	}
	if p.Shrunk != "" {
		c.problem(ckLabel, fmt.Sprintf("look at what the sandbox deleted, and restore from the earlier checkpoint if you need it: caboose checkpoint restore %s --to DIR (the flag clears when a later checkpoint is as large again)", p.Shrunk),
			"%s: the newest checkpoint holds far less than the one before it (%s)", p.Path, p.Shrunk)
	}
	for _, pin := range p.Pinned {
		c.note(ckLabel, "%s: %s is pinned, since %s held half or less of it: it is kept until a later checkpoint holds as much again, or until caboose checkpoint release %s", p.Path, pin.ID, pin.By, pin.ID)
	}
}

// doctorCheckpointNotes are the findings that clear up by themselves or
// are a choice: leftovers of a take that did not finish, checkpoints that
// cannot be read, and checkpoints kept for paths no longer configured.
func (a *App) doctorCheckpointNotes(c *checkup, h *ckHealth, store string, s ckSeams) {
	var left []string
	if des, err := os.ReadDir(store); err == nil {
		for _, de := range des {
			n := de.Name()
			if !strings.HasPrefix(n, ".tmp-") && !strings.HasPrefix(n, ".trash-") {
				continue
			}
			if fi, err := os.Lstat(filepath.Join(store, n)); err == nil && s.now().Sub(fi.ModTime()) >= checkpoint.LeftoverAge {
				left = append(left, n)
			}
		}
	}
	if len(left) > 0 {
		c.note(ckLabel, "%d %s left by a checkpoint that did not finish (%s); the daemon removes them while it runs", len(left), plural(len(left), "directory", "directories"), strings.Join(left, ", "))
	}
	for _, p := range h.Paths {
		if p.Unreadable > 0 {
			c.note(ckLabel, "%s: %d %s cannot be read (never pruned); 'caboose checkpoint list' shows them", p.Path, p.Unreadable, plural(p.Unreadable, "checkpoint", "checkpoints"))
		}
	}
	for _, o := range h.Orphans {
		var bytes int64
		for _, v := range o.Checkpoints {
			bytes += v.Bytes
		}
		c.note(ckLabel, "%d %s kept for %s, which is no longer in [checkpoints] paths (up to %s; they are pruned by the same rule, or removed with the environment)",
			len(o.Checkpoints), plural(len(o.Checkpoints), "checkpoint", "checkpoints"), o.Path, sizeString(bytes))
	}
}
