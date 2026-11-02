package launcher

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/vm"
)

// A profile's volumes ([<kind>.NAME.volume.VOL] in config.toml). Under vm
// each is a disk of the environment's, vm/volumes/VOL.img in the data
// dir, shared by every vm profile that declares it, on caboose-vmm's own
// disk device from macOS 27 (on the framework's own before: liveVolumes)
// and mounted at its path in the guest; its size is the profile's, read
// at every start (vmHost.Volume). Under container and gvisor each is a
// docker volume of the engine's, caboose-<env>-VOL (backend.DockerVolumeName),
// shared by every container and gvisor profile of the environment that
// declares it, made at the container's creation and given to the agent
// user (backend.Spec.VolumeOwner), and kept when the container is
// removed.

// liveVolumes says whether a VM created now puts the profile's volumes on
// caboose-vmm's own disk device, whose size can change while the VM runs:
// only from macOS 27 (vm.LiveDiskMacOS), where caboose-vmm can serve one.
// Before, and when caboose-vmm does not say which macOS this is, they go
// on the framework's own disks, which every macOS vm runs on boots with,
// and whose size changes at a start only: the VM records which kind each
// volume got (vm.State.LiveVolume), so a later resize can tell.
func (a *App) liveVolumes() bool {
	return macOSMajor(a.macOSVersion()) >= vm.LiveDiskMacOS
}

// macOSVersion is this Mac's product version as caboose-vmm --check says
// it, asked once; "" when it cannot be told (no caboose-vmm, off a Mac).
func (a *App) macOSVersion() string {
	if a.macOS != nil {
		return a.macOS()
	}
	if a.macOSSeen == nil {
		v := ""
		if f, _ := a.findVMFiles(); f.VMM != "" && isFile(f.VMM) {
			v = a.checkVMM(f.VMM).Check.MacOS
		}
		a.macOSSeen = &v
	}
	return *a.macOSSeen
}

// volumesKindNote says, under vm, when the profile's volumes go on the
// framework's own disks rather than caboose-vmm's, and why; "" when they
// go on caboose-vmm's, or there are none.
func (a *App) volumesKindNote() string {
	if !a.isVM() || len(a.Cfg.Volumes) == 0 || a.liveVolumes() {
		return ""
	}
	mac := "caboose-vmm did not say which macOS this Mac runs"
	if v := a.macOSVersion(); v != "" {
		mac = "this Mac runs macOS " + v
	}
	return fmt.Sprintf("%s %s on Virtualization.framework's own disks, as %s: caboose-vmm serves volumes on a disk of its own, whose size can change while the VM runs, from macOS %d; on these, a new size takes effect at the VM's next start",
		volumeNames(a.Cfg.Volumes), plural(len(a.Cfg.Volumes), "goes", "go"), mac, vm.LiveDiskMacOS)
}

// noteVolumes says, as the sandbox is created, what of the profile's
// volumes it does not get as they are written: under vm before macOS 27,
// not on caboose-vmm's disks.
func (a *App) noteVolumes() {
	if n := a.volumesKindNote(); n != "" {
		a.Note("%s.", n)
	}
}

// volumeNames is vols' names, joined.
func volumeNames(vols []config.Volume) string {
	var names []string
	for _, v := range vols {
		names = append(names, v.Name)
	}
	return strings.Join(names, ", ")
}

// volumesLabel is the value of assets.LabelVolumes for vols: each one's
// name and the path it is mounted at.
func volumesLabel(vols []backend.Volume) string {
	m := map[string]string{}
	for _, v := range vols {
		m[v.Name] = v.Target
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// volumesDrift says how the sandbox's volumes differ from the
// configuration's, or "" when they do not, or it cannot be told.
func (a *App) volumesDrift() string {
	if d := a.missingLabel(assets.LabelVolumes, "volumes"); d != "" {
		return d
	}
	labels, err := a.box().Labels()
	if err != nil {
		return ""
	}
	var have map[string]string
	if json.Unmarshal([]byte(labels[assets.LabelVolumes]), &have) != nil || have == nil {
		return "the " + a.noun() + "'s record of its volumes cannot be read"
	}
	want := map[string]string{}
	for _, v := range a.volumesLive(false) {
		want[v.Name] = v.Target
	}
	if maps.Equal(have, want) {
		return ""
	}
	return fmt.Sprintf("the %s was created with volumes %s; the configuration says %s", a.noun(), describeVolumes(have), describeVolumes(want))
}

// describeVolumes is vols, by name and path, but caboose's own (dockerd's
// disk under vm, at /var/lib/docker: a profile's volume cannot be named
// docker).
func describeVolumes(vols map[string]string) string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(vols)) {
		if name != dockerVolume {
			out = append(out, name+" at "+vols[name])
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// warnIfVolumesDrifted is warnIfRunArgsDrifted for the volumes.
func (a *App) warnIfVolumesDrifted() {
	if d := a.volumesDrift(); d != "" {
		a.Note("%s.", d)
		what := "every volume's disk"
		if !a.isVM() {
			what = "every volume's docker volume"
		}
		a.Note("run 'caboose restart' to recreate it with them (this kills running sessions; %s is kept).", what)
	}
}

// dropKeepsUnderVolumes skips each of sb's keep entries at, inside or
// around the path of a volume of the profile, under every isolation, as
// one of its problems: the volume wins. Mounted both, one would hide the
// other, or the volume's mount point would be made in the data dir's
// home.
func (a *App) dropKeepsUnderVolumes(sb *datadir.Sandbox) {
	if len(a.Cfg.Volumes) == 0 || sb.Config == nil {
		return
	}
	keep := sb.Keep[:0]
	for _, k := range sb.Keep {
		p := config.ContainerHome + "/" + k.Rel
		i := slices.IndexFunc(a.Cfg.Volumes, func(v config.Volume) bool {
			return config.Within(p, v.Path) || config.Within(v.Path, p)
		})
		if i < 0 {
			keep = append(keep, k)
			continue
		}
		v := a.Cfg.Volumes[i]
		sb.Problems = append(sb.Problems, fmt.Sprintf("keep %s is at, inside or around volume %s (%s): skipped, the volume wins (%s)",
			k.Path, v.Name, v.Path, volumeTable(a.Cfg, v.Name)))
	}
	sb.Keep = keep
}

// volumeTable is where config.toml declares the profile's volume name.
func volumeTable(c *config.Config, name string) string {
	return "[" + c.Profile + ".volume." + name + "]"
}

// doctorVolumes is the volumes' row of doctor: under vm, each volume's
// size and its disk's, saying when a disk is larger than its size (kept:
// a start never shrinks one); under container and gvisor, each volume's
// docker volume (doctorDockerVolumes).
func (a *App) doctorVolumes(c *checkup) {
	vols := a.Cfg.Volumes
	if !a.isVM() {
		a.doctorDockerVolumes(c)
		return
	}
	var lines, bigger []string
	for _, v := range vols {
		size := gibString(v.Size)
		fi, err := os.Stat(a.volumeDisk(v.Name))
		switch {
		case err != nil:
			lines = append(lines, fmt.Sprintf("%s at %s, %s (made at the next start)", v.Name, v.Path, size))
		case fi.Size() > v.Size:
			lines = append(lines, fmt.Sprintf("%s at %s, %s", v.Name, v.Path, gibString(fi.Size())))
			bigger = append(bigger, fmt.Sprintf("%s's disk is %s, more than its size, %s (%s): kept, as a start never shrinks a disk",
				v.Name, gibString(fi.Size()), size, volumeTable(a.Cfg, v.Name)))
		default:
			lines = append(lines, fmt.Sprintf("%s at %s, %s", v.Name, v.Path, size))
		}
	}
	if want, err := dockerSize(a.Cfg); err == nil {
		if fi, err := os.Stat(a.dockerDisk()); err == nil && fi.Size() > want {
			bigger = append(bigger, fmt.Sprintf("dockerd's disk is %s, more than docker_size, %s: kept, as a start never shrinks a disk ('caboose prune --docker' makes an empty one)",
				gibString(fi.Size()), gibString(want)))
		}
	}
	for _, b := range bigger {
		c.note("volumes", "%s", b)
	}
	kind := a.volumesKindNote()
	if kind != "" {
		c.note("volumes", "%s", kind)
	}
	if len(lines) > 0 && len(bigger) == 0 {
		if kind == "" {
			lines[len(lines)-1] += "; each on caboose-vmm's own disk"
		}
		c.ok("volumes", "%s", strings.Join(lines, "; "))
	}
}

// doctorDockerVolumes is doctorVolumes under container and gvisor: each of
// the profile's volumes at its path, with its docker volume and the space
// the engine says it takes, or that the container's creation makes one.
func (a *App) doctorDockerVolumes(c *checkup) {
	vols := a.Cfg.Volumes
	if len(vols) == 0 {
		return
	}
	ev, ok := a.box().(backend.EngineVolumes)
	if !ok {
		return
	}
	have, err := ev.ListVolumes(true)
	if err != nil {
		c.note("volumes", "cannot list the docker volumes of %s: %v", a.Cfg.Container, err)
		return
	}
	var lines []string
	for _, v := range vols {
		i := slices.IndexFunc(have, func(e backend.EngineVolume) bool { return e.Name == v.Name })
		if i < 0 {
			lines = append(lines, fmt.Sprintf("%s at %s (its docker volume, %s, is made as the container is created)",
				v.Name, v.Path, backend.DockerVolumeName(a.Cfg.Container, v.Name)))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s at %s, docker volume %s%s", v.Name, v.Path, have[i].Engine, dockerSizeSuffix(have[i].Size)))
	}
	c.ok("volumes", "%s", strings.Join(lines, "; "))
}

// dockerSizeSuffix is ", SIZE" for a docker volume's size as the engine
// says it, ", size unknown" when it says none.
func dockerSizeSuffix(size string) string {
	return ", " + or(size, "size unknown")
}
