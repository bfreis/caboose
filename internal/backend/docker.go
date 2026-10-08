package backend

import (
	"errors"
	"io"
	"os/exec"
	"strconv"

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

func (d *Docker) State() string { return d.CLI.ContainerState(d.Name) }

// Create runs `docker run` with RunArgv, showing docker's stderr.
func (d *Docker) Create(s Spec) error {
	if s.Egress != "" {
		return errors.New("a container has no outbound proxy of caboose's: the engine dials its connections from this machine already")
	}
	return d.CLI.Run(RunArgv(d.Name, s)...)
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
		argv = append(argv, "-v", name+"-"+v.Name+":"+v.Target)
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

func (d *Docker) Mounts() ([]Mount, error) {
	ms, err := d.CLI.Mounts(d.Name)
	if err != nil {
		return nil, err
	}
	var out []Mount
	for _, m := range ms {
		out = append(out, Mount{Source: m.Source, Target: m.Destination})
	}
	return out, nil
}

func (d *Docker) Logs(stdout, stderr io.Writer, lines int) error {
	return d.CLI.Stream(stdout, stderr, "logs", "--tail", strconv.Itoa(lines), d.Name)
}
