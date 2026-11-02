package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/diskusage"
)

// caboose disk: what caboose keeps on this machine, by kind, each three
// ways -- apparent (the files' sizes), allocated (their blocks, as du
// counts them) and unshared (the blocks no other file shares, which is
// what deleting them gives back; APFS on macOS alone reports it). Only
// stat and getattrlist: the host never reads a disk's contents, nor any
// file's.

// defaultDiskBudget bounds the time caboose disk spends measuring
// unshared sizes: a checkpoint store of a million files takes tens of
// seconds.
const defaultDiskBudget = 2 * time.Minute

// diskWalker is what caboose disk measures with: diskusage.Walker, which
// the tests wrap to make allocated sizes their own.
type diskWalker interface {
	Walk(u *diskusage.Usage, path string, skip func(string) bool)
	OverBudget() bool
	Elapsed() time.Duration
	Unsupported() int64
	Unmeasured() int64
	LinkedOutside() int64
	Untracked() int64
}

// newDiskWalker makes caboose disk's walker; the tests stand in for it.
var newDiskWalker = func(budget time.Duration) (diskWalker, string) {
	p := diskusage.DefaultUnshared()
	w := &diskusage.Walker{Unshared: p, Budget: budget}
	if p == nil {
		return w, ""
	}
	return w, diskusage.UnsharedMethod
}

// diskKind is one kind of what caboose keeps: what was walked for it, and
// what the walk found.
type diskKind struct {
	Kind  string   `json:"kind"`
	Paths []string `json:"paths"`
	diskusage.Usage
	// heavy kinds are walked last, so that the budget for unshared sizes
	// goes to the rest first.
	heavy bool
	// other is the rest of Paths once every other kind of the scope is
	// left out.
	other bool
}

// diskScope is an environment, or caboose's own files.
type diskScope struct {
	// Env is the environment's name, "" for caboose's own files.
	Env   string          `json:"env,omitempty"`
	Dir   string          `json:"dir"`
	Kinds []diskKind      `json:"kinds"`
	Total diskusage.Usage `json:"total"`
}

// diskReport is caboose disk's.
type diskReport struct {
	Envs    []diskScope     `json:"environments"`
	Caboose *diskScope      `json:"caboose,omitempty"`
	Total   diskusage.Usage `json:"total"`
	// Unshared is how unshared sizes were measured, "" when they were not.
	Unshared    string `json:"unshared_method"`
	OverBudget  bool   `json:"over_budget,omitempty"`
	Unsupported int64  `json:"unsupported,omitempty"`
	// Unmeasured is the files whose unshared size could not be read.
	Unmeasured int64 `json:"unmeasured,omitempty"`
	// LinkedOutside is the files with a hard link outside what was walked,
	// and Untracked those of several links past what the walk tracks:
	// both counted, with no unshared size.
	LinkedOutside int64 `json:"linked_outside,omitempty"`
	Untracked     int64 `json:"untracked,omitempty"`
	ElapsedMS     int64 `json:"elapsed_ms"`
}

// Disk is `caboose disk [--json] [--budget DURATION]`: every
// environment's files and caboose's own, or, when env is not "" (-e, or
// CABOOSE_ENV), that environment's and caboose's own.
func (a *App) Disk(env string, args []string) error {
	var asJSON bool
	budget := defaultDiskBudget
	usage := func(why string) error {
		return &ExitError{Code: 2, Msg: "usage: caboose [-e ENV] disk [--json] [--budget DURATION] (" + why + ")"}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--budget":
			if i+1 == len(args) {
				return usage(args[i] + " needs a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return usage(fmt.Sprintf("--budget %q: a duration such as 30s or 5m", args[i+1]))
			}
			budget = d
			i++
		default:
			return usage(fmt.Sprintf("got %q", args[i]))
		}
	}
	envs, err := a.envsWithDir()
	if err != nil {
		return Die("%v", err)
	}
	// envsWithDir keeps this environment whether or not it exists.
	envs = slices.DeleteFunc(envs, func(e string) bool { return !a.envExists(e) })
	all := envs
	if env != "" {
		if !slices.Contains(envs, env) {
			return Die("no environment %s: 'caboose env list' lists them, 'caboose -e %s setup' creates it", env, env)
		}
		envs = []string{env}
	}
	r := a.measureDisk(envs, all, budget)
	if asJSON {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return Die("%v", err)
		}
		fmt.Fprintf(a.Stdout, "%s\n", b)
		return nil
	}
	a.printDisk(a.Stdout, r, budget)
	return nil
}

// envExists reports whether env has anything on this machine: its
// directory, or its data dir (which CABOOSE_DATA_DIR may put elsewhere).
func (a *App) envExists(env string) bool {
	envDir, data := a.envDataDir(env)
	for _, d := range []string{envDir, data} {
		// One that cannot be looked at is kept: the walk says so.
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// envDataDir is env's data dir: this environment's own (CABOOSE_DATA_DIR
// may move it), another's in its directory.
func (a *App) envDataDir(env string) (envDir, dataDir string) {
	if env == a.Cfg.Env {
		return a.Cfg.EnvDir, a.Cfg.DataDir
	}
	envDir = config.EnvDirFor(a.Cfg.CabooseHome, env)
	return envDir, envDir + "/data"
}

// envDiskKinds are the kinds of an environment, in the order they print.
func (a *App) envDiskKinds(env string) diskScope {
	envDir, data := a.envDataDir(env)
	vmRoot := filepath.Join(data, vmDirName)
	glob := func(pattern string) []string {
		m, _ := filepath.Glob(filepath.Join(vmRoot, pattern))
		return m
	}
	return diskScope{Env: env, Dir: envDir, Kinds: []diskKind{
		{Kind: "root disks", Paths: glob("images/root-*.img")},
		{Kind: "empty disks", Paths: glob("images/empty-ext4*.img")},
		{Kind: "scratch disks", Paths: glob("*/scratch.img")},
		{Kind: "volumes", Paths: []string{filepath.Join(vmRoot, "volumes")}},
		{Kind: "builder cache", Paths: []string{filepath.Join(vmRoot, builderName, "cache.img")}},
		{Kind: "kept home", Paths: []string{filepath.Join(data, "home")}},
		{Kind: "Claude Code", Paths: []string{filepath.Join(data, "local")}},
		{Kind: "checkpoints", Paths: []string{checkpoint.StoreDir(envDir)}, heavy: true},
		{Kind: "other", Paths: uniqueDirs(envDir, data), other: true},
	}}
}

// cabooseDiskKinds are caboose's own files, outside every environment.
func (a *App) cabooseDiskKinds() diskScope {
	h := a.Cfg.CabooseHome
	return diskScope{Dir: h, Kinds: []diskKind{
		{Kind: "caboose versions", Paths: []string{filepath.Join(h, "versions")}},
		{Kind: "package cache", Paths: []string{filepath.Join(h, "cache")}},
		{Kind: "vm assets", Paths: []string{filepath.Join(h, "vm")}},
		{Kind: "runsc", Paths: []string{filepath.Join(h, "runsc")}},
		{Kind: "Caboose.app", Paths: []string{appbundle.InstallPath(a.Cfg.Home), appbundle.CheckoutPath(h)}},
		{Kind: "other", Paths: []string{h}, other: true},
	}}
}

// uniqueDirs is dirs less any inside another.
func uniqueDirs(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if !slices.ContainsFunc(dirs, func(o string) bool { return o != d && within(d, o) }) {
			out = append(out, d)
		}
	}
	return out
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// measureDisk walks every kind of envs and of caboose's own files, the
// heavy kinds last; all is every environment, which caboose's own leave
// out whether walked or not.
func (a *App) measureDisk(envs, all []string, budget time.Duration) diskReport {
	w, method := newDiskWalker(budget)
	r := diskReport{Unshared: method}
	var scopes []*diskScope
	for _, e := range envs {
		s := a.envDiskKinds(e)
		scopes = append(scopes, &s)
	}
	own := a.cabooseDiskKinds()
	scopes = append(scopes, &own)
	for _, heavy := range []bool{false, true} {
		for _, s := range scopes {
			for i := range s.Kinds {
				k := &s.Kinds[i]
				if k.heavy != heavy {
					continue
				}
				var skip func(string) bool
				if k.other {
					// Everything the scope's other kinds walk, and, for
					// caboose's own, every environment (anything else in
					// envs/ is caboose's other).
					var covered []string
					for _, o := range s.Kinds {
						if !o.other {
							covered = append(covered, o.Paths...)
						}
					}
					if s.Env == "" {
						for _, e := range all {
							covered = append(covered, a.envPaths(e)...)
						}
					}
					skip = func(p string) bool { return slices.Contains(covered, p) }
				}
				// Into the kind itself: a file of several links has its
				// unshared size added where it was counted once a later
				// walk meets its last link.
				for _, p := range k.Paths {
					w.Walk(&k.Usage, p, skip)
				}
			}
		}
	}
	for _, s := range scopes {
		// A kind that found nothing is left out, but not one that found
		// only what it could not read: the report says how much that is.
		s.Kinds = slices.DeleteFunc(s.Kinds, func(k diskKind) bool { return k.Files == 0 && k.Allocated == 0 && k.Unreadable == 0 })
		for _, k := range s.Kinds {
			s.Total.Add(k.Usage)
		}
		r.Total.Add(s.Total)
		if s.Env == "" {
			r.Caboose = s
		} else {
			r.Envs = append(r.Envs, *s)
		}
	}
	r.OverBudget, r.Unsupported, r.ElapsedMS = w.OverBudget(), w.Unsupported(), w.Elapsed().Milliseconds()
	r.Unmeasured, r.LinkedOutside, r.Untracked = w.Unmeasured(), w.LinkedOutside(), w.Untracked()
	return r
}

// envPaths is env's directory and data dir.
func (a *App) envPaths(env string) []string {
	envDir, data := a.envDataDir(env)
	return []string{envDir, data}
}

// unsharedString is u's unshared size as caboose disk prints it: "-" when
// none was measured, "≥" before one measured in part.
func unsharedString(u diskusage.Usage, measured bool) string {
	switch {
	case !measured || u.UnsharedFiles == 0 && u.Files > 0:
		return "-"
	case !u.UnsharedComplete():
		return "≥" + sizeString(u.Unshared)
	}
	return sizeString(u.Unshared)
}

// diskFrees is what frees each kind, in caboose's words.
var diskFrees = map[string]string{
	"root disks":       "the next 'caboose build' deletes those no image or VM uses",
	"empty disks":      "what scratch disks and volumes are cloned from: they share its blocks, so deleting it frees only its unshared size",
	"scratch disks":    "made at a VM's start, deleted at its stop: 'caboose prune' deletes those left over by a VM that is not running",
	"volumes":          "'caboose volume rm NAME --delete-disk' deletes a volume's disk, 'caboose prune --docker' empties dockerd's",
	"builder cache":    "only grows: 'caboose prune --builder-cache' deletes it (asking first), and the next build makes it again, fetching everything anew",
	"checkpoints":      "kept by the retention rules ('caboose help checkpoint'); clones of each other and of the live files, so only unshared counts",
	"Claude Code":      "'caboose prune' deletes the versions beyond keep_versions",
	"package cache":    "'caboose prune' deletes the packages no environment's lock names",
	"caboose versions": "an update keeps the newest two",
}

// printDisk prints r for a terminal.
func (a *App) printDisk(w io.Writer, r diskReport, budget time.Duration) {
	measured := r.Unshared != ""
	fmt.Fprintln(w, "apparent: the files' sizes; allocated: the blocks they hold, as du counts them;")
	if measured {
		fmt.Fprintln(w, "unshared: the blocks no other file, clone or snapshot holds (APFS's private size),")
		fmt.Fprintln(w, "which deleting the files frees at least")
	} else {
		fmt.Fprintln(w, "unshared: not measured, as only APFS on macOS reports it (allocated counts each")
		fmt.Fprintln(w, "clone's shared blocks again, so it is more than deleting would free)")
	}
	tilde := func(s string) string { return tildeHome(s, a.Cfg.Home) }
	kinds := map[string]bool{}
	scopes := slices.Clone(r.Envs)
	if r.Caboose != nil {
		scopes = append(scopes, *r.Caboose)
	}
	for _, s := range scopes {
		fmt.Fprintln(w)
		if s.Env != "" {
			fmt.Fprintf(w, "environment %s (%s)\n", s.Env, tilde(s.Dir))
		} else {
			fmt.Fprintf(w, "caboose's own (%s, outside the environments)\n", tilde(s.Dir))
		}
		if len(s.Kinds) == 0 {
			fmt.Fprintln(w, "  nothing")
			continue
		}
		// The numbers right-aligned, the kinds left: each kind padded to
		// the widest, so that aligning it right changes nothing.
		kw := len("total")
		for _, k := range s.Kinds {
			kw = max(kw, len(k.Kind))
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		row := func(kind string, u diskusage.Usage) {
			fmt.Fprintf(tw, "  %-*s\t%d\t%s\t%s\t%s\t\n", kw, kind, u.Files, sizeString(u.Apparent), sizeString(u.Allocated), unsharedString(u, measured))
		}
		fmt.Fprintf(tw, "  %-*s\tfiles\tapparent\tallocated\tunshared\t\n", kw, "")
		for _, k := range s.Kinds {
			kinds[k.Kind] = true
			row(k.Kind, k.Usage)
		}
		row("total", s.Total)
		tw.Flush()
	}
	fmt.Fprintln(w)
	line := fmt.Sprintf("all of it: %s allocated", sizeString(r.Total.Allocated))
	if measured {
		// Blocks two of these files share are in neither's unshared size,
		// yet deleting both frees them: the sum is a floor.
		line += fmt.Sprintf(", %s unshared; deleting all of it would free at least that", unsharedString(r.Total, measured))
	}
	fmt.Fprintln(w, line)
	if r.OverBudget {
		fmt.Fprintf(w, "partial: unshared sizes measured for %d of %d files in %s, the budget ('--budget 10m' gives more); '≥' marks a kind measured in part\n",
			r.Total.UnsharedFiles, r.Total.Files, budget)
	}
	if r.Unsupported > 0 {
		fmt.Fprintf(w, "%d %s on a filesystem that reports no unshared size\n", r.Unsupported, plural(int(r.Unsupported), "file is", "files are"))
	}
	if r.Unmeasured > 0 {
		fmt.Fprintf(w, "%d %s unshared size could not be read: counted, but not measured\n", r.Unmeasured, plural(int(r.Unmeasured), "file's", "files'"))
	}
	if r.LinkedOutside > 0 {
		fmt.Fprintf(w, "%d %s a hard link outside what was looked at: counted, but with no unshared size, as deleting what was looked at would not free %s\n",
			r.LinkedOutside, plural(int(r.LinkedOutside), "file has", "files have"), plural(int(r.LinkedOutside), "it", "them"))
	}
	if r.Untracked > 0 {
		fmt.Fprintf(w, "%d %s of several hard links met past the most tracked at once: counted wherever met, maybe more than once, with no unshared size\n",
			r.Untracked, plural(int(r.Untracked), "file", "files"))
	}
	if r.Total.Unreadable > 0 {
		fmt.Fprintf(w, "%d %s could not be read, and %s not counted\n", r.Total.Unreadable, plural(int(r.Total.Unreadable), "entry", "entries"), plural(int(r.Total.Unreadable), "is", "are"))
	}
	var frees []string
	for _, k := range []string{"root disks", "empty disks", "scratch disks", "volumes", "builder cache", "checkpoints", "Claude Code", "package cache", "caboose versions"} {
		if kinds[k] {
			frees = append(frees, fmt.Sprintf("  %s: %s", k, diskFrees[k]))
		}
	}
	if len(frees) > 0 {
		fmt.Fprintln(w, "\nWhat frees what:")
		for _, f := range frees {
			fmt.Fprintln(w, f)
		}
	}
}
