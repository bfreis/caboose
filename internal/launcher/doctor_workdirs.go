package launcher

import (
	"path"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
)

// A volume or [session] dir is where work lives when no root holds it. Two
// things follow that doctor says: the memory of projects there syncs only
// when the sandbox config's roots name a directory that holds them, and
// with no roots at all a volume holds the only copy of what is on it.

// homeKeptDirs are the directories under the home that volumes are most
// often made at for caches, not for projects: a volume there is not worth
// a note about project memory.
var homeKeptDirs = []string{".cache", ".config", ".local"}

// isProjectPath reports whether p could hold projects: not a cache or
// config directory of the home.
func isProjectPath(p string) bool {
	for _, d := range homeKeptDirs {
		if config.Within(p, path.Join(config.ContainerHome, d)) {
			return false
		}
	}
	return true
}

// doctorUnsyncedWorkdirs notes each volume and [session] dir that no entry
// of the sandbox config's roots holds, when something syncs: the memory of
// a project there would not. roots are the entries, container paths.
func (a *App) doctorUnsyncedWorkdirs(c *checkup, roots []string, edit string) {
	for _, p := range a.startPaths() {
		// A volume under a cache or config directory of the home is not
		// worth a note; [session] dir was named as a place to work.
		if (p != a.Cfg.SessionDir && !isProjectPath(p)) || slices.ContainsFunc(roots, func(r string) bool { return config.Within(p, r) }) {
			continue
		}
		c.note("roots", "%s is a volume's path or [session] dir, and the sandbox config's roots hold no directory of it, so the memory of its projects does not sync; "+
			"add %s to its roots to sync it (%s)", p, p, edit)
	}
}

// doctorVolumeCopies notes, when no root is configured, each volume that
// holds the only copy of what is on it because no checkpoint covers it:
// under vm, no [checkpoints] paths entry is the volume's path; under
// container and gvisor a volume cannot be checkpointed at all (only vm
// copies a volume's disk).
func (a *App) doctorVolumeCopies(c *checkup) {
	if len(a.Cfg.Roots) > 0 {
		return
	}
	for _, v := range a.Cfg.Volumes {
		switch {
		case !a.isVM():
			c.note("volumes", "volume %s at %s cannot be checkpointed under %s (only vm copies a volume's disk), and with no roots no directory of this machine has its files: it holds the only copy",
				v.Name, v.Path, isolationOf(a.Cfg))
		case !slices.ContainsFunc(a.Cfg.Checkpoints, func(p string) bool { return strings.TrimSuffix(displayPathAbs(p), "/") == v.Path }):
			c.note("volumes", "volume %s at %s is in no [checkpoints] paths entry, and with no roots no directory of this machine has its files: it holds the only copy; "+
				"add %q to paths under [checkpoints] in config.toml", v.Name, v.Path, v.Path)
		}
	}
}

// displayPathAbs is a [checkpoints] path as the sandbox's absolute path:
// ~/REL is under the home.
func displayPathAbs(p string) string {
	if rel, ok := strings.CutPrefix(p, "~/"); ok {
		return config.ContainerHome + "/" + rel
	}
	return p
}
