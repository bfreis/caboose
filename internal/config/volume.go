package config

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// volumeKey is the family of a profile's volume tables,
// [<kind>.NAME.volume.VOLUME].
const volumeKey = "volume"

// volumeKeys are the keys of a volume table, by the kind of the profile it
// is in. Only vm's take a size: under container and gvisor a volume is a
// docker volume, which has none, so a size cannot be written there.
var volumeKeys = map[string]map[string]valueType{
	KindContainer: {"path": typeString},
	KindGVisor:    {"path": typeString},
	KindVM:        {"path": typeString, "size": typeString},
}

// DefaultVolumeSize is a vm volume's size when its table gives none.
const DefaultVolumeSize = 64 << 30

// MaxVMVolumes is how many volumes a vm profile may have: each is a disk
// device of the VM's, and a VM takes no more than 19 beside the devices
// caboose gives every sandbox, dockerd's disk (DockerVolume) among them
// (vm.MaxBlockDevices, measured).
const MaxVMVolumes = 19

// DockerVolume is the volume caboose keeps the sandbox's dockerd storage on
// under vm; no profile may name one so.
const DockerVolume = "docker"

// Volume is a volume of the profile in use: its name, the container path
// it is mounted at, and under vm its size in bytes (0 under container and
// gvisor).
type Volume struct {
	Name, Path string
	Size       int64
}

// FileVolume is a volume as read from a profile's table: Path is the
// container path, ~/ expanded to the home.
type FileVolume struct {
	Name, Path string
	Size       int64
}

// volumeName is a volume's name: a profile's rule, but at most 20 bytes,
// which is what a virtio-blk disk's serial holds, and a vm volume's disk
// is told apart in the guest by its serial, its name.
var volumeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,19}$`)

// ValidVolumeName reports whether name can name a volume: it becomes a
// file's name, a docker volume's and a disk's serial.
func ValidVolumeName(name string) bool { return volumeName.MatchString(name) }

// CheckVolumeName says why name cannot name a profile's volume: not a
// volume name, or caboose's own (DockerVolume).
func CheckVolumeName(name string) error {
	if !ValidVolumeName(name) {
		return fmt.Errorf("'%s' is not a volume name: lowercase letters, digits, - and _, starting with a letter or digit, at most 20", name)
	}
	if name == DockerVolume {
		return fmt.Errorf("%s is the volume caboose keeps the sandbox's dockerd storage on; give yours another name", name)
	}
	return nil
}

// readVolumes reads profile's [<profile>.volume.NAME] tables, each with the
// keys its kind's volumes take, a path every one, none nesting another.
func (f *File) readVolumes(kind, profile string, v any) ([]FileVolume, error) {
	notTables := fmt.Errorf("%s in [%s] must be tables of volumes, [%s.%s.NAME]", volumeKey, profile, profile, volumeKey)
	named, ok := v.(map[string]any)
	if !ok {
		return nil, notTables
	}
	for _, t := range named {
		if _, ok := t.(map[string]any); !ok {
			return nil, notTables
		}
	}
	if kind == KindVM && len(named) > MaxVMVolumes {
		return nil, fmt.Errorf("[%s] has %d volumes, and a VM takes at most %d besides caboose's", profile, len(named), MaxVMVolumes)
	}
	var vols []FileVolume
	for _, name := range slices.Sorted(maps.Keys(named)) {
		table := profile + "." + volumeKey + "." + name
		if err := CheckVolumeName(name); err != nil {
			return nil, fmt.Errorf("[%s]: %v", table, err)
		}
		if err := f.readTable(table, named[name], volumeKeys[kind]); err != nil {
			return nil, err
		}
		p, ok := f.Vals[table+".path"].(string)
		if !ok {
			return nil, fmt.Errorf("[%s] needs path, where the sandbox sees the volume", table)
		}
		ctr, err := VolumePath(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", displayKey(table+".path"), err)
		}
		fv := FileVolume{Name: name, Path: ctr}
		if kind == KindVM {
			fv.Size = DefaultVolumeSize
			if s, ok := f.Vals[table+".size"].(string); ok {
				if fv.Size, err = ParseScratchSize(s); err != nil {
					return nil, fmt.Errorf("%s: %v", displayKey(table+".size"), err)
				}
			}
		}
		vols = append(vols, fv)
	}
	for i, a := range vols {
		for _, b := range vols[i+1:] {
			switch {
			case a.Path == b.Path:
				return nil, fmt.Errorf("volumes %s and %s of [%s] are both at %s", a.Name, b.Name, profile, a.Path)
			case Within(b.Path, a.Path):
				return nil, nestErr(profile, a, b)
			case Within(a.Path, b.Path):
				return nil, nestErr(profile, b, a)
			}
		}
	}
	return vols, nil
}

func nestErr(profile string, outer, inner FileVolume) error {
	return fmt.Errorf("volumes %s and %s of [%s] nest (%s, %s): give them paths that do not contain one another",
		outer.Name, inner.Name, profile, outer.Path, inner.Path)
}

// VolumePath checks a volume's path as written and returns the container
// path it names. One in the home, ~/REL (or /home/agent/REL), sits
// directly in one of sandboxcfg.Parents, as a kept path does, and is not
// at, in or around caboose's own mounts (sandboxcfg.Reserved) or the paths
// kept on this machine because caboose reads them there
// (sandboxcfg.Required). Any other is a path CheckContainerPath allows.
// Where the roots are is checked once they are known (checkVolumeRoots).
func VolumePath(p string) (string, error) {
	if err := badPathChar(p, ":,"); err != nil {
		return "", fmt.Errorf("path %q %v", p, err)
	}
	if t := strings.TrimSuffix(p, "/"); t == "~" || t == ContainerHome {
		return "", fmt.Errorf("%q is the whole home: a volume goes in a directory of it, such as ~/.cache/go-build", p)
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, ContainerHome+"/") {
		rel, err := sandboxcfg.HomeRel(p)
		if err != nil || strings.HasSuffix(p, "/") {
			return "", fmt.Errorf("%q is not a clean path under the home (~/...)", p)
		}
		if err := homeVolume(rel); err != nil {
			return "", err
		}
		if !slices.Contains(sandboxcfg.Parents, path.Dir("/" + rel)[1:]) {
			return "", fmt.Errorf("%q is too deep: a volume in the home sits directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache (the directories above it would be made as root)", p)
		}
		return ContainerHome + "/" + rel, nil
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("%q: only ~/ (the sandbox's home) may start a path", p)
	}
	if !path.IsAbs(p) || path.Clean(p) != p {
		return "", fmt.Errorf("%q is not an absolute, clean path in the sandbox (or one starting with ~/)", p)
	}
	if err := CheckContainerPath(p); err != nil {
		return "", err
	}
	return p, nil
}

// homeVolume refuses rel, a home-relative path, at, in or around what
// caboose mounts in the home or keeps on this machine for itself.
func homeVolume(rel string) error {
	within := func(a, b string) bool { return a == b || strings.HasPrefix(a, b+"/") }
	for _, r := range sandboxcfg.Reserved {
		switch {
		case rel == r:
			return fmt.Errorf("~/%s is caboose's own", r)
		case within(r, rel):
			return fmt.Errorf("~/%s holds ~/%s, which caboose mounts itself", rel, r)
		case within(rel, r):
			return fmt.Errorf("~/%s is inside ~/%s, which caboose mounts itself", rel, r)
		}
	}
	for _, k := range sandboxcfg.Required {
		switch {
		case rel == k.Rel:
			return fmt.Errorf("~/%s is kept on this machine, which reads it: it cannot be a volume", k.Rel)
		case within(k.Rel, rel):
			return fmt.Errorf("~/%s holds ~/%s, which is kept on this machine, which reads it", rel, k.Rel)
		case within(rel, k.Rel):
			return fmt.Errorf("~/%s is inside ~/%s, which is kept on this machine, which reads it", rel, k.Rel)
		}
	}
	return nil
}

// checkVolumeRoots refuses a volume of any profile at, inside or around a
// root's container path, or WorkDir, where roots go by default.
func (c *Config) checkVolumeRoots() error {
	f := c.File
	for _, profile := range slices.Sorted(maps.Keys(f.Volumes)) {
		for _, v := range f.Volumes[profile] {
			at := func(why string) error {
				return fmt.Errorf("%s: %s: %q is at, inside or around %s", f.Path,
					displayKey(profile+"."+volumeKey+"."+v.Name+".path"), v.Path, why)
			}
			for _, r := range c.Roots {
				if Within(v.Path, r.Container) || Within(r.Container, v.Path) {
					return at("root " + r.Name + " at " + r.Container)
				}
			}
			if Within(v.Path, WorkDir) || Within(WorkDir, v.Path) {
				return at(WorkDir + ", which is for the roots")
			}
		}
	}
	return nil
}

// volumeTable splits a volume table's dotted name, "<kind>.<name>.volume.V",
// into its kind; ok is false for any other table.
func volumeTable(table string) (kind string, ok bool) {
	parts := strings.Split(table, ".")
	if len(parts) != 4 || parts[2] != volumeKey || volumeKeys[parts[0]] == nil {
		return "", false
	}
	return parts[0], true
}
