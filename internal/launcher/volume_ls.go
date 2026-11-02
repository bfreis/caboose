package launcher

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/bfreis/caboose/internal/config"
)

// volumeListing is what `caboose volume ls` shows: every volume of the
// environment, by name, and dockerd's disk.
type volumeListing struct {
	Volumes []listedVolume `json:"volumes"`
	// Docker is dockerd's disk, under vm: nil when it has none.
	Docker *listedDocker `json:"docker,omitempty"`
}

// listedVolume is a volume name: the profiles that declare it, its disk
// when it has one, and how the running VM has it.
type listedVolume struct {
	Name     string       `json:"name"`
	Profiles []declaredIn `json:"profiles"`
	Disk     *listedDisk  `json:"disk,omitempty"`
	Attached string       `json:"attached,omitempty"`
	Orphan   bool         `json:"orphan,omitempty"`
}

// The values of listedVolume.Attached: on which kind of disk the running
// VM has the volume.
const (
	attachedLive      = "caboose-vmm"
	attachedFramework = "framework"
)

// declaredIn is a profile's table of a volume: its path in the sandbox,
// its size under vm (0 otherwise), and whether it is the profile in use.
type declaredIn struct {
	Profile string `json:"profile"`
	Path    string `json:"path"`
	Size    int64  `json:"size,omitempty"`
	InUse   bool   `json:"in_use,omitempty"`
}

// listedDisk is a disk in the data dir's vm/volumes: where, its size, and
// the space it takes up on this machine.
type listedDisk struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Allocated int64  `json:"allocated"`
}

// listedDocker is dockerd's disk, and docker_size of the profile in use
// (0 when it is no vm profile).
type listedDocker struct {
	listedDisk
	DockerSize int64 `json:"docker_size,omitempty"`
}

// volumeLs is `caboose volume ls [--json]`.
func (a *App) volumeLs(args []string) error {
	var asJSON bool
	pos, err := volumeArgs(args, nil, map[string]*bool{"--json": &asJSON})
	if err != nil || len(pos) > 0 {
		msg := "usage: caboose volume ls [--json]"
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		return &ExitError{Code: 2, Msg: msg}
	}
	l, err := a.listVolumes()
	if err != nil {
		return Die("%v", err)
	}
	if asJSON {
		b, err := json.MarshalIndent(l, "", "  ")
		if err != nil {
			return Die("%v", err)
		}
		fmt.Fprintf(a.Stdout, "%s\n", b)
		return nil
	}
	a.printVolumes(a.Stdout, l)
	return nil
}

// listVolumes gathers the listing: the volumes config.toml declares, in
// any profile, and the disks of vm/volumes in the data dir, each with how
// the running VM has it.
func (a *App) listVolumes() (volumeListing, error) {
	// Empty lists, not nil ones, which JSON would show as null.
	l := volumeListing{Volumes: []listedVolume{}}
	byName := map[string]*listedVolume{}
	if f := a.Cfg.File; f != nil {
		for _, p := range slices.Sorted(maps.Keys(f.Volumes)) {
			for _, v := range f.Volumes[p] {
				lv := byName[v.Name]
				if lv == nil {
					lv = &listedVolume{Name: v.Name, Profiles: []declaredIn{}}
					byName[v.Name] = lv
				}
				lv.Profiles = append(lv.Profiles, declaredIn{Profile: p, Path: v.Path, Size: v.Size, InUse: p == a.Cfg.Profile})
			}
		}
	}
	disks, err := a.volumeDisks()
	if err != nil {
		return l, err
	}
	for _, name := range slices.Sorted(maps.Keys(disks)) {
		d := disks[name]
		if name == dockerVolume {
			l.Docker = &listedDocker{listedDisk: d}
			if a.isVM() {
				l.Docker.DockerSize, _ = dockerSize(a.Cfg)
			}
			continue
		}
		lv := byName[name]
		if lv == nil {
			lv = &listedVolume{Name: name, Profiles: []declaredIn{}}
			byName[name] = lv
		}
		lv.Disk = &d
		lv.Orphan = len(a.declaring(name, false)) == 0
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		lv := byName[name]
		if vs, err := a.vmVolumeState(name); err == nil && vs.Running && vs.Has {
			lv.Attached = attachedFramework
			if vs.Live {
				lv.Attached = attachedLive
			}
		}
		l.Volumes = append(l.Volumes, *lv)
	}
	return l, nil
}

// volumeDisks is the disks of vm/volumes, NAME.img by NAME; a shrink's
// backups and anything else there are not volumes' disks.
func (a *App) volumeDisks() (map[string]listedDisk, error) {
	entries, err := os.ReadDir(a.volumesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	disks := map[string]listedDisk{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".img")
		if !ok || !e.Type().IsRegular() || (name != dockerVolume && !config.ValidVolumeName(name)) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		disks[name] = listedDisk{Path: a.volumeDisk(name), Size: fi.Size(), Allocated: diskAllocated(fi)}
	}
	return disks, nil
}

// orphans is the listing's volumes whose disk no profile declares.
func (l volumeListing) orphans() []listedVolume {
	var out []listedVolume
	for _, v := range l.Volumes {
		if v.Orphan {
			out = append(out, v)
		}
	}
	return out
}

// printVolumes prints l for a terminal: each volume's line, then its
// profiles' tables, one a line; dockerd's disk last.
func (a *App) printVolumes(w io.Writer, l volumeListing) {
	if len(l.Volumes) == 0 && l.Docker == nil {
		fmt.Fprintln(w, "No volumes: config.toml declares none, and the data dir holds no volume's disk. 'caboose volume create NAME PATH' adds one.")
		return
	}
	for _, v := range l.Volumes {
		fmt.Fprintf(w, "%s: %s\n", v.Name, a.volumeSummary(v))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range v.Profiles {
			p := d.Profile
			if d.InUse {
				p += " (in use)"
			}
			size := "-"
			if profileKind(d.Profile) == config.KindVM {
				size = gibString(d.Size)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", p, d.Path, size)
		}
		tw.Flush()
	}
	if d := l.Docker; d != nil {
		line := fmt.Sprintf("docker: dockerd's disk, %s, %s allocated", gibString(d.Size), sizeString(d.Allocated))
		if d.DockerSize > 0 {
			line += fmt.Sprintf(" (docker_size in [%s]: %s)", a.Cfg.Profile, gibString(d.DockerSize))
		}
		fmt.Fprintln(w, line+"; 'caboose prune --docker' empties it")
	}
}

// volumeSummary is a volume's line: its disk, or why it has none, and how
// the running VM has it.
func (a *App) volumeSummary(v listedVolume) string {
	var s string
	vmDeclared := slices.ContainsFunc(v.Profiles, func(d declaredIn) bool { return profileKind(d.Profile) == config.KindVM })
	switch {
	case v.Disk != nil:
		s = fmt.Sprintf("disk %s, %s allocated", gibString(v.Disk.Size), sizeString(v.Disk.Allocated))
	case vmDeclared:
		s = "no disk yet: a VM of a profile that declares it makes it as it starts"
	default:
		s = "not attached: caboose makes volumes only under vm so far"
	}
	switch v.Attached {
	case attachedLive:
		s += "; attached to the running VM, on caboose-vmm's own disk (resizes while it runs)"
	case attachedFramework:
		s += "; attached to the running VM, on Virtualization.framework's own disk (resizes at a start)"
	}
	if v.Disk != nil && !v.Orphan && !vmDeclared {
		s += "; only container and gvisor profiles declare it, which attach no disk"
	}
	if v.Orphan && v.Disk != nil {
		s += fmt.Sprintf("; no profile declares it: 'caboose volume rm %s --delete-disk' deletes it", v.Name)
	}
	return s
}
