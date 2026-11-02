package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/vm"
)

// volumeUsage is caboose volume's usage line.
const volumeUsage = "usage: caboose volume create NAME PATH [--size SIZE] [--profile PROFILE] | ls [--json] | rm NAME [--profile PROFILE] [--delete-disk] | resize NAME SIZE"

// Volume is `caboose volume ACTION`: create, ls, rm or resize.
func (a *App) Volume(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return a.volumeCreate(args[1:])
		case "ls":
			return a.volumeLs(args[1:])
		case "rm":
			return a.volumeRm(args[1:])
		case "resize":
			return a.volumeResize(args[1:])
		}
	}
	got := "nothing"
	if len(args) > 0 {
		got = fmt.Sprintf("%q", args[0])
	}
	return &ExitError{Code: 2, Msg: volumeUsage + " (got " + got + ")"}
}

// volumeArgs splits args into the positional ones and the flags: those in
// valued take a value (--size 8G, or --size=8G), those in flags none. An
// unknown flag, or one given twice, is an error.
func volumeArgs(args []string, valued map[string]*string, flags map[string]*bool) ([]string, error) {
	var pos []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		x := args[i]
		if !strings.HasPrefix(x, "-") {
			pos = append(pos, x)
			continue
		}
		name, val, hasVal := strings.Cut(x, "=")
		if seen[name] {
			return nil, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if dst, ok := valued[name]; ok {
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
			if val == "" {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			*dst = val
			continue
		}
		if dst, ok := flags[name]; ok && !hasVal {
			*dst = true
			continue
		}
		return nil, fmt.Errorf("unknown flag %s", proposal.Printable(x))
	}
	return pos, nil
}

// volumeProfile is the isolation profile a volume command acts on: the
// one --profile names, which config.toml must define, else the one in
// use, which must be a profile config.toml defines.
func (a *App) volumeProfile(flag string) (string, error) {
	c := a.Cfg
	var have []string
	if c.File != nil {
		have = c.File.Profiles
	}
	list := "none"
	if len(have) > 0 {
		list = strings.Join(have, ", ")
	}
	switch {
	case flag != "" && !slices.Contains(have, flag):
		return "", Die("config.toml defines no isolation profile %s (its profiles: %s); --profile takes one as <kind>.<name>, such as vm.default", proposal.Printable(flag), list)
	case flag != "":
		return flag, nil
	case c.Profile == "" || !slices.Contains(have, c.Profile):
		return "", Die("a volume is a table of an isolation profile, and config.toml defines none in use (isolation %s): "+
			"add one ([vm.default], say, and isolation = \"vm.default\"; 'caboose setup isolation' does that), or name one with --profile (its profiles: %s)", isolationOf(c), list)
	}
	return c.Profile, nil
}

// profileKind is a profile's kind: "vm" for "vm.default".
func profileKind(profile string) string {
	kind, _, _ := strings.Cut(profile, ".")
	return kind
}

// fileVolume is the volume name of profile as config.toml declares it.
func (a *App) fileVolume(profile, name string) (config.FileVolume, bool) {
	if a.Cfg.File == nil {
		return config.FileVolume{}, false
	}
	for _, v := range a.Cfg.File.Volumes[profile] {
		if v.Name == name {
			return v, true
		}
	}
	return config.FileVolume{}, false
}

// declaring is the profiles of config.toml that declare volume name, of
// any kind, or of vm alone; sorted.
func (a *App) declaring(name string, vmOnly bool) []string {
	if a.Cfg.File == nil {
		return nil
	}
	var out []string
	for _, p := range slices.Sorted(maps.Keys(a.Cfg.File.Volumes)) {
		if _, ok := a.fileVolume(p, name); ok && (!vmOnly || profileKind(p) == config.KindVM) {
			out = append(out, p)
		}
	}
	return out
}

// envVM is the environment's VM, whichever isolation config.toml now
// picks: the VM may still be there from when it picked vm. nil when the
// sandbox is the tests' and no VM.
func (a *App) envVM() interface {
	backend.Backend
	backend.VolumeResizer
} {
	type vmBackend interface {
		backend.Backend
		backend.VolumeResizer
	}
	if b, ok := a.box().(vmBackend); ok {
		return b
	}
	if a.Backend != nil {
		return nil
	}
	return a.vmBox()
}

// holdVMStart takes the start lock of the environment's VM, waiting for
// it, and returns what lets go of it; with no VM directory there is no VM
// to order, and nothing to hold. The tests stand in for it.
var holdVMStart = func(a *App) (func(), error) {
	unlock, err := a.vmBox().HoldStart()
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	return unlock, err
}

// vmVolumeState is how the environment's VM has volume name: the zero
// state when there is no VM.
func (a *App) vmVolumeState(name string) (backend.VolumeState, error) {
	vm := a.envVM()
	if vm == nil {
		return backend.VolumeState{}, nil
	}
	return vm.VolumeState(name)
}

// writeVolumeEdit writes e to config.toml as writeConfig does, provided
// the file it makes loads as a launch would load it -- every check of the
// volumes included: their names, paths, nesting, the roots, sizes, how
// many -- and says why not otherwise, writing nothing. An edit that
// changes nothing is checked all the same: a table to remove that the
// edit could not find is still there, and the lines to change by hand are
// shown.
func (a *App) writeVolumeEdit(e config.Edit) error {
	path, data, edited, mode, err := a.checkVolumeEdit(e)
	if err != nil || string(edited) == string(data) {
		return err
	}
	if err := replaceConfig(a.Cfg.EnvDir, path, edited, mode); err != nil {
		return Die("%v", err)
	}
	return nil
}

// checkVolumeEdit is what writeVolumeEdit checks, writing nothing:
// config.toml as it is (data) and as e would make it (edited), or why e
// cannot be written. A [volume] proposal is planned with it.
func (a *App) checkVolumeEdit(e config.Edit) (path string, data, edited []byte, mode os.FileMode, err error) {
	c := a.Cfg
	path, data, mode, err = readConfigToEdit(c.EnvDir)
	if err != nil {
		return "", nil, nil, 0, Die("%v", err)
	}
	edited = config.EditFile(data, e)
	// What config.toml does not take (a size under container, say) is
	// refused as such; a file the edit broke is the edit's to say.
	if config.CheckTOML(edited) == nil {
		if _, err := config.ParseFile(path, edited); err != nil {
			return "", nil, nil, 0, Die("%v\n       nothing was written to %s", err, path)
		}
	}
	if err := config.CheckEdit(path, data, edited, e); err != nil {
		return "", nil, nil, 0, Die("cannot edit %s safely (%v);\n"+
			"       nothing was written to it. Make this change by hand:\n%s", path, err, indent(e.Snippet(), "         "))
	}
	if string(edited) != string(data) {
		if _, err := config.Load(c.Getenv, confOverlay{path: filepath.Clean(path), data: edited}, c.Env); err != nil {
			return "", nil, nil, 0, Die("%v\n       nothing was written to %s", err, path)
		}
	}
	return path, data, edited, mode, nil
}

// volumeCreate is `caboose volume create NAME PATH [--size SIZE]
// [--profile PROFILE]`: it adds [PROFILE.volume.NAME] to config.toml, after
// the profile's tables, with path and, under vm, size (64G when not
// given). Under container and gvisor a volume has no size, which the
// file's keys refuse. Nothing else is made: the disk of a vm volume is
// made at the VM's next start, and a running VM gets the volume when
// 'caboose restart' recreates it.
func (a *App) volumeCreate(args []string) error {
	var size, flagProfile string
	pos, err := volumeArgs(args, map[string]*string{"--size": &size, "--profile": &flagProfile}, nil)
	if err != nil || len(pos) != 2 {
		msg := "usage: caboose volume create NAME PATH [--size SIZE] [--profile PROFILE]"
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		return &ExitError{Code: 2, Msg: msg}
	}
	name, path := pos[0], pos[1]
	if err := config.CheckVolumeName(name); err != nil {
		return Die("%v", err)
	}
	profile, err := a.volumeProfile(flagProfile)
	if err != nil {
		return err
	}
	table := profile + ".volume." + name
	if v, ok := a.fileVolume(profile, name); ok {
		next := "'caboose volume rm " + name + profileFlag(a, profile) + "' removes it first"
		if profileKind(profile) == config.KindVM {
			next = "'caboose volume resize " + name + " SIZE' changes its size; " + next
		}
		return Die("[%s] is in config.toml already, at %s: %s", table, v.Path, next)
	}
	set := map[string]any{table + ".path": path}
	if profileKind(profile) == config.KindVM && size == "" {
		size = sizeValue(config.DefaultVolumeSize)
	}
	if size != "" {
		set[table+".size"] = size
	}
	if err := a.writeVolumeEdit(config.Edit{Tables: []string{table}, Set: set}); err != nil {
		return err
	}
	said := fmt.Sprintf("config.toml: [%s] path = %q", table, path)
	if size != "" {
		said += fmt.Sprintf(", size = %q", size)
	}
	a.Note("%s", said)
	if profileKind(profile) != config.KindVM {
		a.Note("under %s it is docker volume %s, which the container's creation makes if the engine has none, and keeps when the container goes", profileKind(profile), backend.DockerVolumeName(a.Cfg.Container, name))
		switch {
		case profile != a.Cfg.Profile:
			a.Note("%s is not the profile in use (%s): a container of %s gets the volume", profile, profileLabel(a.Cfg), profile)
		case a.state() == "absent":
			a.Note("the container's creation, at the next start, mounts it at %s", path)
		default:
			a.Note("the container was created without it: 'caboose restart' recreates it with the volume (this kills running sessions)")
		}
		return nil
	}
	disk := a.volumeDisk(name)
	if fi, err := os.Stat(disk); err == nil {
		a.Note("its disk, %s, is here already (%s, %s allocated): the volume gets it as it is, with what it holds ('caboose volume rm %s%s --delete-disk' deletes it, for an empty one)",
			disk, gibString(fi.Size()), sizeString(diskAllocated(fi)), name, profileFlag(a, profile))
		if n, err := config.ParseScratchSize(size); err == nil && fi.Size() > n {
			a.Note("it is larger than %s: kept, as a start never shrinks a disk ('caboose volume resize %s %s' shrinks it while the VM runs)", size, name, size)
		}
	} else if n, err := config.ParseScratchSize(size); err == nil {
		if w := diskSpaceWarning(a.Cfg.DataDir, "["+table+"] size", "the volume's disk", n); w != "" {
			a.Note("%s.", w)
		}
	}
	if profile != a.Cfg.Profile {
		a.Note("%s is not the profile in use (%s): a VM of %s gets the volume", profile, profileLabel(a.Cfg), profile)
		return nil
	}
	// A VM's volumes are fixed when it is created: a start mounts those.
	state := "absent"
	if vm := a.envVM(); vm != nil {
		state = vm.State()
	}
	switch state {
	case "running":
		a.Note("the VM runs without it: 'caboose restart' recreates the VM with the volume, its disk made then (this kills running sessions)")
	case "absent":
		a.Note("the VM's creation, at the next start, makes its disk, if there is none, and mounts it at %s", path)
	default:
		a.Note("the VM, stopped, was created without it, and its next start would not mount it: 'caboose restart' recreates the VM with the volume")
	}
	return nil
}

// diskAllocated is the space fi's file takes up on this machine: its
// blocks, a sparse disk's holes not counted. The tests stand in for it.
var diskAllocated = func(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}

// profileFlag is " --profile PROFILE" when profile is not the one in use,
// for a command to name it; "" when it is.
func profileFlag(a *App, profile string) string {
	if profile == a.Cfg.Profile {
		return ""
	}
	return " --profile " + profile
}

// volumeRm is `caboose volume rm NAME [--profile PROFILE] [--delete-disk]`:
// it comments out [PROFILE.volume.NAME] in config.toml. With
// --delete-disk it also deletes the volume's disk, once no vm profile
// declares the volume, after asking (CABOOSE_FORCE=1 does not ask); NAME
// may then be a volume no profile declares, whose disk is left over.
// Under container and gvisor --delete-disk deletes the volume's docker
// volume too, once the container no longer has it, unless PROFILE is a
// vm profile or another container or gvisor profile still declares the
// volume, which keep it, with a note; and a vm disk only if no other vm
// profile declares the volume. What fails to go after the config edit is
// said, with what did go.
// Refused while the running VM has the volume, when PROFILE is the one in
// use or the disk is to go: its disk is in use, and the VM would go on
// writing to a volume config.toml no longer has.
func (a *App) volumeRm(args []string) error {
	var flagProfile string
	var deleteDisk bool
	pos, err := volumeArgs(args, map[string]*string{"--profile": &flagProfile}, map[string]*bool{"--delete-disk": &deleteDisk})
	if err != nil || len(pos) != 1 {
		msg := "usage: caboose volume rm NAME [--profile PROFILE] [--delete-disk]"
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		return &ExitError{Code: 2, Msg: msg}
	}
	name := pos[0]
	if name == dockerVolume {
		return Die("%s is dockerd's disk, caboose's own: 'caboose prune --docker' empties it", name)
	}
	if err := config.CheckVolumeName(name); err != nil {
		return Die("%v", err)
	}
	all := a.declaring(name, false)
	var profile string
	switch {
	case flagProfile != "":
		if profile, err = a.volumeProfile(flagProfile); err != nil {
			return err
		}
		if !slices.Contains(all, profile) {
			return Die("[%s.volume.%s] is not in config.toml (%s)", profile, name, declaredBy(name, all))
		}
	case slices.Contains(all, a.Cfg.Profile):
		profile = a.Cfg.Profile
	case len(all) > 0:
		return Die("the profile in use (%s) has no volume %s; %s: name one with --profile", profileLabel(a.Cfg), name, declaredBy(name, all))
	case !deleteDisk:
		next := ""
		if isFile(a.volumeDisk(name)) {
			next = fmt.Sprintf(": its disk, %s, is left over ('caboose volume rm %s --delete-disk' deletes it)", a.volumeDisk(name), name)
		} else if dv, _ := a.engineVolume(name, false); dv != nil {
			next = fmt.Sprintf(": its docker volume, %s, is left over ('caboose volume rm %s --delete-disk' deletes it)", dv.Engine, name)
		}
		return Die("no profile in config.toml declares a volume %s%s", name, next)
	}
	// A start of the VM waits while the disk is checked, asked about and
	// deleted, so that none opens it in between; one that was running
	// already holds the disk's lock, which diskFree finds.
	if deleteDisk {
		unlock, err := holdVMStart(a)
		if err != nil {
			return Die("%v", err)
		}
		defer unlock()
	}
	// The VM is the profile in use's: another profile's volume of the same
	// name is no business of it, unless its disk is to go.
	var vs backend.VolumeState
	if deleteDisk || (profile == a.Cfg.Profile && profileKind(profile) == config.KindVM) {
		if vs, err = a.vmVolumeState(name); err != nil {
			return Die("cannot tell whether the VM has volume %s: %v", name, err)
		}
	}
	if vs.Running && vs.Has {
		return Die("the running VM has volume %s: 'caboose stop' first (this ends its sessions), then run this again; its disk is kept unless --delete-disk", name)
	}
	disk := a.volumeDisk(name)
	// What a restore left beside the disk: refused without --delete-disk,
	// which deletes it too, so that it is never left behind a volume
	// config.toml no longer has.
	var restoreLeft []listedLeftover
	lefts, err := a.volumeLeftovers()
	if err != nil {
		return Die("%v", err)
	}
	for _, l := range leftoversOf(lefts, name) {
		if l.Kind == leftoverBeforeRestore || l.Kind == leftoverRestoring {
			restoreLeft = append(restoreLeft, l)
		}
	}
	if len(restoreLeft) > 0 && !deleteDisk {
		var lines []string
		for _, l := range restoreLeft {
			lines = append(lines, a.leftoverLine(l))
		}
		return Die("%s; deal with %s first, or add --delete-disk, which deletes %s with the disk, after asking", strings.Join(lines, "; "), plural(len(restoreLeft), "it", "them"), plural(len(restoreLeft), "it", "them"))
	}
	var fi os.FileInfo
	// Under container and gvisor the volume is a docker volume, which
	// --delete-disk deletes, unless it is a vm profile's volume going
	// (whose disk is not it) or another container or gvisor profile still
	// declares it, which keep it; and once the container does not have
	// it. A vm disk of the same name goes too, unless a vm profile still
	// declares it.
	var dv *backend.EngineVolume
	dockerKept := "" // why the docker volume stays, when it does
	diskKept := false
	if deleteDisk && !a.isVM() {
		left := slices.DeleteFunc(a.declaringDocker(name), func(p string) bool { return p == profile })
		switch {
		case profileKind(profile) == config.KindVM:
			dockerKept = fmt.Sprintf("%s is a vm profile, whose volume is a disk, not it", profile)
		case len(left) > 0:
			dockerKept = fmt.Sprintf("%s still %s the volume", strings.Join(left, ", "), plural(len(left), "declares", "declare"))
		case a.containerHasVolume(name):
			rm := "'caboose volume rm " + name + profileFlag(a, profile) + "', then "
			if profile == "" {
				rm = ""
			}
			return Die("the container was created with volume %s, and holds its docker volume: %s'caboose restart' (recreates it without the volume, ending its sessions), then run 'caboose volume rm %s --delete-disk'", name, rm, name)
		default:
			if dv, err = a.engineVolume(name, true); err != nil {
				return Die("cannot list the docker volumes of %s: %v", a.Cfg.Container, err)
			}
		}
		diskKept = slices.ContainsFunc(a.declaring(name, true), func(p string) bool { return p != profile })
	}
	if diskKept {
		// A vm profile still declares the volume: what a restore left
		// stays beside its disk, which stays.
		restoreLeft = nil
	}
	if deleteDisk && !diskKept {
		if left := slices.DeleteFunc(a.declaring(name, true), func(p string) bool { return p == profile }); len(left) > 0 {
			return Die("%s still %s volume %s, on the same disk: remove it there first ('caboose volume rm %s --profile %s'), or leave out --delete-disk",
				strings.Join(left, ", "), plural(len(left), "declares", "declare"), name, name, left[0])
		}
		if fi, err = os.Lstat(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Die("%v", err)
		}
	}
	if fi != nil && !fi.Mode().IsRegular() {
		return Die("%s is not a disk image: delete it by hand if it is nothing of yours", disk)
	}
	if fi != nil {
		if err := diskFree(disk); err != nil {
			return Die("volume %s: %v", name, err)
		}
		a.Note("this deletes %s (%s, %s allocated): everything volume %s holds", disk, gibString(fi.Size()), sizeString(diskAllocated(fi)), name)
	}
	for _, l := range restoreLeft {
		if l.SameAsDisk {
			a.Note("and %s, a second name of that disk", l.Path)
			continue
		}
		what := "the disk as it was before a checkpoint's restore"
		if l.Kind == leftoverRestoring {
			what = "a copy of a checkpoint an interrupted restore left (the checkpoint keeps it)"
		}
		a.Note("and %s (%s, %s allocated), %s", l.Path, gibString(l.Size), sizeString(l.Allocated), what)
	}
	if dv != nil {
		a.Note("this deletes docker volume %s (%s): everything volume %s holds", dv.Engine, or(dv.Size, "size unknown"), name)
	}
	if fi != nil || dv != nil || len(restoreLeft) > 0 {
		if a.force() {
			a.Note("CABOOSE_FORCE=1 set, continuing.")
		} else {
			q := "delete it?"
			if fi != nil && dv != nil {
				q = "delete both?"
			}
			yes, asked, err := a.askYes(q)
			switch {
			case err != nil:
				return err
			case !asked:
				return Die("refusing to delete it non-interactively (set CABOOSE_FORCE=1 to delete it); nothing was changed")
			case !yes:
				return Die("aborted; nothing was changed")
			}
		}
		// Again, after the wait for an answer: with no VM directory there
		// was no start lock to hold, and a VM made since may have the disk.
		if fi != nil || len(restoreLeft) > 0 {
			if err := diskFree(disk); err != nil {
				return Die("volume %s: %v; nothing was changed", name, err)
			}
		}
	}
	if profile != "" {
		if err := a.writeVolumeEdit(config.Edit{Remove: []string{profile + ".volume." + name}}); err != nil {
			return err
		}
		a.Note("config.toml: [%s.volume.%s] commented out", profile, name)
	}
	if vs.Has {
		a.Note("the VM, stopped, was created with volume %s, and its next start still mounts it (making an empty disk if there is none): 'caboose restart' recreates it without", name)
	}
	kept := false // a note said what of the volume is kept
	if !a.isVM() && !deleteDisk {
		if profile == a.Cfg.Profile && a.containerHasVolume(name) {
			a.Note("the container, created with volume %s, keeps it until 'caboose restart' recreates it without (this kills running sessions)", name)
		}
		if ev, _ := a.engineVolume(name, false); ev != nil {
			a.Note("its docker volume, %s, is kept, with what it holds ('caboose volume rm %s --delete-disk' deletes it once no container or gvisor profile declares it)", ev.Engine, name)
		}
	}
	if dockerKept != "" {
		if ev, _ := a.engineVolume(name, false); ev != nil {
			a.Note("its docker volume, %s, is kept: %s", ev.Engine, dockerKept)
			kept = true
		}
	}
	// What was asked to go is tried in full: a docker volume that will not
	// go leaves the disk to be deleted all the same, and the error says
	// what went and what did not.
	var failed []string
	if dv != nil {
		if err := a.box().(backend.EngineVolumes).RemoveVolume(name); err != nil {
			failed = append(failed, fmt.Sprintf("cannot delete docker volume %s: %v", dv.Engine, err))
		} else {
			a.Note("deleted docker volume %s", dv.Engine)
		}
	}
	for _, l := range restoreLeft {
		if err := os.Remove(l.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, fmt.Sprintf("cannot delete %s: %v", l.Path, err))
			continue
		}
		a.Note("deleted %s", l.Path)
	}
	if diskKept && isFile(disk) {
		a.Note("its vm disk, %s, is kept: %s still %s the volume", disk, strings.Join(a.declaring(name, true), ", "), plural(len(a.declaring(name, true)), "declares", "declare"))
		kept = true
	}
	switch {
	case fi != nil:
		if err := removeDisk(disk); err != nil {
			failed = append(failed, fmt.Sprintf("cannot delete %s: %v", disk, err))
			break
		}
		a.Note("deleted %s", disk)
		if isFile(disk + backend.BackupSuffix) {
			a.Note("%s, a shrink's backup of it, is still here: delete it by hand once you no longer need it", disk+backend.BackupSuffix)
		}
	case deleteDisk && dv == nil && !kept && len(restoreLeft) == 0:
		a.Note("volume %s has no disk or docker volume here: nothing to delete", name)
	case deleteDisk:
	case isFile(disk):
		if left := slices.DeleteFunc(a.declaring(name, true), func(p string) bool { return p == profile }); len(left) > 0 {
			a.Note("its disk, %s, is kept: %s still %s the volume", disk, strings.Join(left, ", "), plural(len(left), "declares", "declare"))
		} else {
			a.Note("its disk, %s, is kept, with what it holds ('caboose volume rm %s --delete-disk' deletes it; 'caboose volume create %s PATH' gives it back to a profile)", disk, name, name)
		}
	}
	if len(failed) > 0 {
		return a.rmFailed(name, profile, failed)
	}
	return nil
}

// rmFailed is volume rm's error once what it was to delete did not all
// go: failed says what did not, the notes before it what did. The config
// edit is done by then, which the error says, with what finishes the job.
func (a *App) rmFailed(name, profile string, failed []string) error {
	msg := strings.Join(failed, "; ")
	if profile != "" {
		msg += fmt.Sprintf("; [%s.volume.%s] is commented out in config.toml already", profile, name)
	}
	if left := slices.DeleteFunc(a.declaring(name, false), func(p string) bool { return p == profile }); len(left) == 0 {
		msg += fmt.Sprintf(": once that is fixed, 'caboose volume rm %s --delete-disk' finishes the job", name)
	} else {
		msg += fmt.Sprintf(": %s still %s the volume, so delete what is left by hand once that is fixed", strings.Join(left, ", "), plural(len(left), "declares", "declare"))
	}
	return Die("%s", msg)
}

// removeDisk deletes a volume's disk; the tests stand in for it.
var removeDisk = os.Remove

// declaringDocker is the container and gvisor profiles of config.toml that
// declare volume name, which share its docker volume; sorted.
func (a *App) declaringDocker(name string) []string {
	return slices.DeleteFunc(a.declaring(name, false), func(p string) bool { return profileKind(p) == config.KindVM })
}

// engineVolume is volume name's docker volume, as the sandbox's backend
// lists it (backend.EngineVolumes), with its size if sizes; nil when
// there is none, or the backend keeps no volumes of the engine's (a VM's
// are disks).
func (a *App) engineVolume(name string, sizes bool) (*backend.EngineVolume, error) {
	ev, ok := a.box().(backend.EngineVolumes)
	if !ok || a.isVM() {
		return nil, nil
	}
	vols, err := ev.ListVolumes(sizes)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if v.Name == name {
			return &v, nil
		}
	}
	return nil, nil
}

// containerHasVolume reports whether the container exists, created with
// volume name (assets.LabelVolumes): docker refuses to delete a volume a
// container has, stopped or not.
func (a *App) containerHasVolume(name string) bool {
	if a.isVM() || a.state() == "absent" {
		return false
	}
	labels, err := a.box().Labels()
	if err != nil {
		return false
	}
	var have map[string]string
	if json.Unmarshal([]byte(labels[assets.LabelVolumes]), &have) != nil {
		return false
	}
	_, ok := have[name]
	return ok
}

// declaredBy says which profiles declare volume name: all.
func declaredBy(name string, all []string) string {
	if len(all) == 0 {
		return "no profile declares a volume " + name
	}
	return fmt.Sprintf("%s %s volume %s", strings.Join(all, ", "), plural(len(all), "declares", "declare"), name)
}

// volumeResize is `caboose volume resize NAME SIZE`: it makes the vm
// profile's volume NAME SIZE, in config.toml and, while the VM runs with
// the volume on one of caboose-vmm's disks, in the VM at once (growing
// or shrinking: backend.VolumeResizer). The new size is written to
// config.toml once the disk has it, so that the next start does not undo
// it. A stopped VM's volume only grows (a start grows a disk, never
// shrinks one); a shrink needs the VM running, whose guest shrinks the
// filesystem first.
func (a *App) volumeResize(args []string) error {
	if len(args) != 2 {
		return &ExitError{Code: 2, Msg: "usage: caboose volume resize NAME SIZE (a size as config.toml writes one: \"48G\", \"49152M\")"}
	}
	name, sizeArg := args[0], args[1]
	c := a.Cfg
	if !a.isVM() {
		return Die("under %s a volume has no size: only a vm profile's volumes are disks of their own ([vm.NAME.volume.VOL] size)", isolationOf(c))
	}
	if name == dockerVolume {
		return Die("dockerd's disk is sized by docker_size in [%s] of config.toml, at the VM's next start ('caboose prune --docker' makes an empty one)", c.Profile)
	}
	var vol *config.Volume
	for i := range c.Volumes {
		if c.Volumes[i].Name == name {
			vol = &c.Volumes[i]
		}
	}
	if vol == nil {
		have := "none"
		if len(c.Volumes) > 0 {
			have = volumeNames(c.Volumes)
		}
		return Die("profile %s has no volume %s (its volumes: %s): add a [%s.volume.%s] table, with its path and size, to config.toml", c.Profile, name, have, c.Profile, name)
	}
	size, err := config.ParseScratchSize(sizeArg)
	if err != nil {
		return Die("size: %v", err)
	}
	key := c.Profile + ".volume." + name + ".size"
	edit := config.Edit{Set: map[string]any{key: sizeValue(size)}}
	// Before the VM is touched: a config.toml that cannot be edited safely
	// would leave the disk at a size no file records.
	if err := a.checkConfigEdit(edit); err != nil {
		return err
	}
	vr, ok := a.box().(backend.VolumeResizer)
	if !ok {
		return Die("this sandbox's volumes cannot be resized")
	}
	vs, err := vr.VolumeState(name)
	if err != nil {
		return Die("%v", err)
	}
	table := volumeTable(c, name)
	switch {
	case vs.Running && vs.Has && vs.Live:
		// An interrupt is the resize's to handle: the first one stops it
		// where that is safe (before the unmount, nothing is done; after,
		// the step under way ends and the volume is mounted again), and
		// stops the catching, so that a second one ends the launcher at
		// once, as the resize said it would.
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		stopped := make(chan struct{})
		go func() {
			select {
			case <-sigs:
				signal.Stop(sigs)
				cancel()
			case <-stopped:
			}
		}()
		_, err := a.liveResize(ctx, vr, name, size, edit, table)
		close(stopped)
		signal.Stop(sigs)
		interrupted := ctx.Err() != nil
		cancel()
		switch {
		case err != nil && interrupted:
			return &ExitError{Code: 130, Msg: err.Error()}
		case err != nil:
			return Die("%v", err)
		}
		return nil
	case vs.Running && vs.Has:
		why := fmt.Sprintf("the VM has volume %s on one of Virtualization.framework's own disks, as it was created on a macOS before %d: their size changes only at a start, and only up", name, vm.LiveDiskMacOS)
		if size < a.volumeDiskSize(name, vol.Size) {
			return Die("%s, so a shrink needs the VM recreated on macOS %d or later ('caboose restart' there), and run again", why, vm.LiveDiskMacOS)
		}
		if err := a.recordVolumeSize(edit, table, size); err != nil {
			return err
		}
		a.Note("%s: the volume grows at the VM's next start ('caboose restart' on macOS %d or later recreates the VM with its volumes on caboose-vmm's disks, which resize while it runs)", why, vm.LiveDiskMacOS)
		return nil
	}
	if cur := a.volumeDiskSize(name, 0); cur > size {
		if vs.Running {
			return Die("the VM was created without volume %s: 'caboose restart' gives it the volume, at its disk's size, %s; then run this again to shrink it", name, gibString(cur))
		}
		return Die("volume %s's disk is %s, and a shrink needs the VM running, whose guest shrinks the filesystem first (a start never shrinks a disk): start the sandbox ('caboose' in a project) and run this again", name, gibString(cur))
	}
	if err := a.recordVolumeSize(edit, table, size); err != nil {
		return err
	}
	if vs.Running {
		a.Note("the VM was created without volume %s: 'caboose restart' gives it the volume, at %s", name, gibString(size))
	} else {
		a.Note("the VM is not running: volume %s grows to %s at its next start", name, gibString(size))
	}
	return nil
}

// liveResize resizes volume name of the running VM to size
// (backend.VolumeResizer), saying each step, and writes edit, its new
// size, to config.toml once the disk has it, whatever else failed: the
// next start must not undo it. caboose volume resize and a resize the
// sandbox asked for both make it.
func (a *App) liveResize(ctx context.Context, vr backend.VolumeResizer, name string, size int64, edit config.Edit, table string) (backend.VolumeResize, error) {
	res, err := vr.ResizeVolume(ctx, name, size, func(format string, args ...any) { a.Note(format, args...) })
	if res.Applied {
		if werr := a.recordVolumeSize(edit, table, size); werr != nil {
			return res, errors.Join(werr, err)
		}
	}
	return res, err
}

// volumeDiskSize is the size of volume name's disk on this machine, or
// dflt when it has none yet.
func (a *App) volumeDiskSize(name string, dflt int64) int64 {
	if fi, err := os.Stat(a.volumeDisk(name)); err == nil {
		return fi.Size()
	}
	return dflt
}

// recordVolumeSize writes edit, a volume's new size, to config.toml, and
// says so, with the free-space warning a scratch disk's size gets.
func (a *App) recordVolumeSize(edit config.Edit, table string, size int64) error {
	changed, err := a.writeConfig(edit)
	if err != nil {
		return err
	}
	if changed {
		a.Note("config.toml: %s size = %q", table, sizeValue(size))
	}
	if w := diskSpaceWarning(a.Cfg.DataDir, table+" size", "the volume's disk", size); w != "" {
		a.Note("%s.", w)
	}
	return nil
}

// checkConfigEdit says, as writeConfig would, when e cannot be written to
// config.toml safely; it writes nothing.
func (a *App) checkConfigEdit(e config.Edit) error {
	path, data, _, err := readConfigToEdit(a.Cfg.EnvDir)
	if err != nil {
		return Die("%v", err)
	}
	edited := config.EditFile(data, e)
	if string(edited) == string(data) {
		return nil
	}
	if err := config.CheckEdit(path, data, edited, e); err != nil {
		return Die("cannot edit %s safely (%v);\n"+
			"       nothing was changed. Make this change by hand, then run this again:\n%s", path, err, indent(e.Snippet(), "         "))
	}
	return nil
}

// sizeValue is size as config.toml's sizes are written: "48G", or
// "49664M" where it is no whole number of GiB.
func sizeValue(size int64) string {
	if size%(1<<30) == 0 {
		return fmt.Sprintf("%dG", size>>30)
	}
	return fmt.Sprintf("%dM", size>>20)
}
