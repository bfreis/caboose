package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/docker"
)

// Docker is a sandbox that is a docker container, under whatever runtime
// its Spec named: the docker and gvisor isolations.
type Docker struct {
	CLI  *docker.CLI
	Name string // the container's
}

// NewDocker is the container name on cli's engine.
func NewDocker(cli *docker.CLI, name string) *Docker { return &Docker{CLI: cli, Name: name} }

var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NewDockerByID is the container with the full 64-hex ID id on cli's
// engine: every call names it by that ID, never by a name, which another
// container could take between a check and an exec. docker resolves a
// full ID before any name, and never reuses one.
func NewDockerByID(cli *docker.CLI, id string) (*Docker, error) {
	if !fullID.MatchString(id) {
		return nil, fmt.Errorf("%q is not a full container ID", id)
	}
	return &Docker{CLI: cli, Name: id}, nil
}

func (d *Docker) State() string { return d.CLI.ContainerState(d.Name) }

// Create runs `docker run` with RunArgv, showing docker's stderr, once
// the volumes are ready (prepareVolumes). A failure to ready them is a
// *VolumeError: docker has shown nothing of it.
func (d *Docker) Create(s Spec) error {
	if s.Egress != "" {
		return errors.New("a container has no outbound proxy of caboose's: the engine dials its connections from this machine already")
	}
	if err := d.prepareVolumes(s); err != nil {
		return err
	}
	return d.CLI.Run(RunArgv(d.Name, s)...)
}

// VolumeError is a Create that failed readying the sandbox's volumes,
// before the sandbox existed. Msg says why, with what docker said, and
// what to do: docker's stderr was captured, never shown, so whoever
// reports it prints Msg, whatever exit status Err (the docker command's
// failure, nil for a refusal of caboose's) unwraps to.
type VolumeError struct {
	Msg string
	Err error
}

func (e *VolumeError) Error() string { return e.Msg }
func (e *VolumeError) Unwrap() error { return e.Err }

// volumeErrorf is a *VolumeError of err, its Msg formatted from format
// and args.
func volumeErrorf(err error, format string, args ...any) error {
	return &VolumeError{Msg: fmt.Sprintf(format, args...), Err: err}
}

// prepareVolumes makes each of s's volumes the engine does not have yet,
// labelled as the container's (VolumeLabelSandbox, VolumeLabelName). It
// refuses one the engine has that is not the container's: one another
// container's labels claim, since two names can meet (environment a's
// volume b-c, environment a-b's volume c), and one caboose did not make
// (no VolumeLabelSandbox), or that is anything but docker's own storage
// (a driver other than local, or driver options): a volume bound to a
// host directory would hand the sandbox that directory, and the chown
// below would chown it. Then, with s.VolumeOwner set, it gives each
// volume's root to that owner in a throwaway container of the same
// image, runtime and user namespace (a --userns of s.RunArgs), as root,
// no network, running the image's chown alone: a new volume at a path
// the image lacks is root's, 0755, and the sandbox's own processes are
// not root under runc. It is done before the sandbox exists, so nothing
// of the sandbox's runs before its volumes are its own; only the root of
// each is changed, never what it holds.
func (d *Docker) prepareVolumes(s Spec) error {
	for _, v := range s.Volumes {
		name := DockerVolumeName(d.Name, v.Name)
		have, exists, err := d.CLI.InspectVolume(name)
		if err != nil {
			return volumeErrorf(err, "volume %s: %v; the container was not created: once docker answers ('docker info'), run caboose again", v.Name, err)
		}
		if exists {
			if err := d.checkVolume(name, v.Name, have); err != nil {
				return err
			}
			continue
		}
		if _, err := d.CLI.Output("volume", "create", "--label", VolumeLabelSandbox+"="+d.Name,
			"--label", VolumeLabelName+"="+v.Name, "--", name); err != nil {
			return volumeErrorf(err, "creating docker volume %s: %v; the container was not created: fix what docker says, then run caboose again", name, err)
		}
	}
	if s.VolumeOwner == "" || len(s.Volumes) == 0 {
		return nil
	}
	if _, err := d.CLI.Output(OwnerArgv(d.Name, s)...); err != nil {
		return volumeErrorf(err, "giving the volumes' roots to the sandbox's user (%s), in a throwaway container of the image running its chown as root: %v; "+
			"the container was not created: fix what docker says, then run caboose again", s.VolumeOwner, err)
	}
	return nil
}

// checkVolume refuses the docker volume name, which the engine has for
// volume vol, unless it is one caboose made for this container.
func (d *Docker) checkVolume(name, vol string, have docker.Volume) error {
	rm := fmt.Sprintf("if nothing of yours needs it, 'docker volume rm %s' deletes it, and the next start makes caboose's own", name)
	owner := have.Labels[VolumeLabelSandbox]
	switch {
	case owner != "" && owner != d.Name:
		return volumeErrorf(nil, "docker volume %s is container %s's (its label %s), not %s's: their names meet; "+
			"rename volume %s in config.toml, or delete that volume ('docker volume rm %s') if nothing of yours needs it",
			name, owner, VolumeLabelSandbox, d.Name, vol, name)
	case owner == "":
		return volumeErrorf(nil, "docker volume %s was not made by caboose (it has no label %s), and caboose mounts only its own: %s", name, VolumeLabelSandbox, rm)
	case have.Driver != "local" || len(have.Options) > 0:
		return volumeErrorf(nil, "docker volume %s is not docker's own storage (driver %q, options %v), and caboose mounts only a plain local volume, whose root it gives to the sandbox's user: %s",
			name, have.Driver, have.Options, rm)
	}
	return nil
}

// OwnerArgv is the `docker run` prepareVolumes gives s's volumes to
// s.VolumeOwner with: chown of each volume's root, at its own path. It
// runs in the user namespace the sandbox will (the last --userns= of
// s.RunArgs): under an engine's userns-remap a chown in another one
// would give the volumes to a user the sandbox is not.
func OwnerArgv(name string, s Spec) []string {
	argv := []string{"run", "--rm", "--network", "none", "--user", "0:0"}
	if s.Runtime != "" {
		argv = append(argv, "--runtime", s.Runtime)
	}
	if userns, ok := lastFlag(s.RunArgs, "--userns="); ok {
		argv = append(argv, "--userns", userns)
	}
	argv = append(argv, "--entrypoint", "chown")
	var targets []string
	for _, v := range s.Volumes {
		argv = append(argv, "-v", DockerVolumeName(name, v.Name)+":"+v.Target)
		targets = append(targets, v.Target)
	}
	return append(append(argv, s.Image, s.VolumeOwner), targets...)
}

// lastFlag is the value of the last of args that starts with prefix
// ("--userns="), which docker takes of a single-value flag.
func lastFlag(args []string, prefix string) (string, bool) {
	for i := len(args) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(args[i], prefix); ok {
			return v, true
		}
	}
	return "", false
}

var _ EngineVolumes = (*Docker)(nil)

// VolumeSizeTimeout bounds the docker system df that ListVolumes asks
// sizes of: it walks every volume of the engine, and its answer only adds
// to what is shown.
var VolumeSizeTimeout = 10 * time.Second

// ListVolumes is the engine's volumes labelled as this container's; with
// sizes, each with the space docker system df says it takes, "" for each
// when df fails or takes longer than VolumeSizeTimeout.
func (d *Docker) ListVolumes(sizes bool) ([]EngineVolume, error) {
	out, err := d.CLI.Output("volume", "ls", "--filter", "label="+VolumeLabelSandbox+"="+d.Name,
		"--format", `{{.Name}}{{"\t"}}{{.Label "`+VolumeLabelName+`"}}`)
	if err != nil {
		return nil, err
	}
	var vols []EngineVolume
	for _, line := range strings.Split(out, "\n") {
		engine, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		// One whose name is not this container's for its volume label is
		// not one caboose made for it.
		if !ok || name == "" || engine != DockerVolumeName(d.Name, name) {
			continue
		}
		vols = append(vols, EngineVolume{Name: name, Engine: engine})
	}
	if len(vols) == 0 || !sizes {
		return vols, nil
	}
	size := map[string]string{}
	if df, err := d.CLI.OutputWithin(VolumeSizeTimeout, "system", "df", "-v", "--format", "json"); err == nil {
		var u struct{ Volumes []struct{ Name, Size string } }
		if json.Unmarshal([]byte(df), &u) == nil {
			for _, v := range u.Volumes {
				size[v.Name] = v.Size
			}
		}
	}
	for i := range vols {
		vols[i].Size = size[vols[i].Engine]
	}
	return vols, nil
}

// RemoveVolume is `docker volume rm` of volume name's docker volume; its
// error is docker's stderr.
func (d *Docker) RemoveVolume(name string) error {
	_, err := d.CLI.Output("volume", "rm", "--", DockerVolumeName(d.Name, name))
	return err
}

// RunArgv is the `docker run` that creates container name from s: detached,
// under tini (--init), restarted unless stopped, with s.RunArgs after all
// of caboose's own, where docker takes the last of a single-value flag.
func RunArgv(name string, s Spec) []string {
	argv := []string{"run", "-d", "--name", name, "--hostname", s.Hostname, "--restart", "unless-stopped", "--init"}
	for _, e := range s.Env {
		argv = append(argv, "-e", e)
	}
	for _, m := range s.Mounts {
		v := m.Source + ":" + m.Target
		if m.ReadOnly {
			v += ":ro"
		}
		argv = append(argv, "-v", v)
	}
	for _, v := range s.Volumes {
		argv = append(argv, "-v", DockerVolumeName(name, v.Name)+":"+v.Target)
	}
	for _, g := range s.Groups {
		argv = append(argv, "--group-add", g)
	}
	if s.Runtime != "" {
		argv = append(argv, "--runtime", s.Runtime)
	}
	if s.User != "" {
		argv = append(argv, "--user", s.User)
	}
	for _, l := range s.Labels {
		argv = append(argv, "--label", l)
	}
	argv = append(argv, s.RunArgs...)
	return append(append(argv, s.Image), s.Cmd...)
}

// Start is `docker start`; its error is docker's stderr.
func (d *Docker) Start() error {
	_, err := d.CLI.Output("start", d.Name)
	return err
}

func (d *Docker) Stop() error   { return d.CLI.Run("stop", d.Name) }
func (d *Docker) Remove() error { return d.CLI.Run("rm", "-f", d.Name) }

// Command is `docker exec`.
func (d *Docker) Command(s ExecSpec) *exec.Cmd {
	argv := []string{"exec"}
	if s.User != "" {
		argv = append(argv, "-u", s.User)
	}
	if s.Stdin {
		argv = append(argv, "-i")
	}
	if s.TTY {
		argv = append(argv, "-t")
	}
	for _, e := range s.Env {
		argv = append(argv, "-e", e)
	}
	if s.Dir != "" {
		argv = append(argv, "-w", s.Dir)
	}
	argv = append(append(argv, d.Name), s.Argv...)
	return d.CLI.Command(argv...)
}

func (d *Docker) Labels() (map[string]string, error) { return d.CLI.ContainerLabels(d.Name) }

func (d *Docker) Image() string { return d.CLI.ContainerImage(d.Name) }

var _ Starter = (*Docker)(nil)

// Started is the container's State.StartedAt, while it runs.
func (d *Docker) Started() (time.Time, bool) {
	out, err := d.CLI.Output("inspect", "--type=container", "-f", "{{.State.Running}} {{.State.StartedAt}}", d.Name)
	running, at, ok := strings.Cut(strings.TrimSpace(out), " ")
	if err != nil || !ok || running != "true" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || t.IsZero() || t.Year() < 2000 {
		return time.Time{}, false
	}
	return t, true
}

func (d *Docker) Mounts() ([]Mount, error) {
	ms, err := d.CLI.Mounts(d.Name)
	if err != nil {
		return nil, err
	}
	var out []Mount
	for _, m := range ms {
		out = append(out, Mount{Source: m.Source, Target: m.Destination, Type: m.Type, Volume: m.Name})
	}
	return out, nil
}

func (d *Docker) Logs(stdout, stderr io.Writer, lines int) error {
	return d.CLI.Stream(stdout, stderr, "logs", "--tail", strconv.Itoa(lines), d.Name)
}
