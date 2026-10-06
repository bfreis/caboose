// Package vm is the host's side of a vm guest, whatever runs it: where a
// VM's files are, what the launcher records of it, and how the host speaks
// to its agent through vm.sock (the control port, the exec helper, and the
// clock). The runner itself, vmm, reads only Machine; what the guest boots
// into is the launcher's, sent over the control port (backend.VM).
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bfreis/caboose/internal/agentproto"
)

// Dir is one VM's directory, the data dir's vm/<name>/. It is the host's
// alone: never shared into a guest, so nothing in it can be forged from
// inside, which is why State is trusted where a container's labels are not.
type Dir string

// Socket is vm.sock, which vmm serves (internal/hvsock). Whoever can
// connect runs commands in the guest, so the directory is 0700.
func (d Dir) Socket() string { return filepath.Join(string(d), "vm.sock") }

// State is the launcher's record of the VM (State).
func (d Dir) State() string { return filepath.Join(string(d), "state.json") }

// Machine is what vmm boots (Machine), written at every start.
func (d Dir) Machine() string { return filepath.Join(string(d), "machine.json") }

// Console is the guest's console: the kernel's, the init's and the
// entrypoint's output, docker logs' equivalent.
func (d Dir) Console() string { return filepath.Join(string(d), "console.log") }

// Scratch is the overlay's upper disk, new at every start.
func (d Dir) Scratch() string { return filepath.Join(string(d), "scratch.img") }

// PID is vmm's process ID, which vmm writes while it runs.
func (d Dir) PID() string { return filepath.Join(string(d), "vmm.pid") }

// Lock is what makes one vmm the VM's only one.
func (d Dir) Lock() string { return filepath.Join(string(d), "vmm.lock") }

// Log is where vmm's own output goes: what it did, and why it stopped.
func (d Dir) Log() string { return filepath.Join(string(d), "vmm.log") }

// ShareTag is the virtio-fs tag of the one share every mount is a
// directory of (Machine.Shares).
const ShareTag = "caboose"

// Machine is what vmm makes: the hardware, never what runs on it.
type Machine struct {
	Kernel    string `json:"kernel"`
	Initramfs string `json:"initramfs"`
	Cmdline   string `json:"cmdline,omitempty"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memory_mib"`
	// Disks are attached in order: vda, the image's root, read-only;
	// vdb, the scratch disk.
	Disks []Disk `json:"disks"`
	// Shares are the directories of the one virtio-fs share, ShareTag,
	// each under its Name.
	Shares []Share `json:"shares,omitempty"`
	// Network is "nat", or "none" for a VM with no network of its own.
	Network string `json:"network"`
	// MAC is the network device's address (Dir.MAC), which Dir.Boot fills
	// in when empty. Empty only for a VM with no network.
	MAC string `json:"mac,omitempty"`
	// Console is the file the guest's console is written to.
	Console string `json:"console"`
}

// Disk is a raw disk image.
type Disk struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Share is one host directory in the share, at Name in the guest's mount
// of it.
type Share struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Mount is one of the launcher's mounts, as it asked for it.
type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// State is what the launcher records of a VM when it creates it: what a
// container's labels, image and mounts are to docker inspect, plus what
// every start needs.
type State struct {
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
	Mounts []Mount           `json:"mounts,omitempty"`
	// Boot is sent to the agent at every start.
	Boot agentproto.BootSpec `json:"boot"`
	// Machine is written to machine.json at every start.
	Machine Machine `json:"machine"`
	// Volumes name the volumes whose disks follow the scratch disk in
	// Machine's, in order: each start asks for them again, which makes
	// one that was deleted afresh.
	Volumes []string `json:"volumes,omitempty"`
}

// ReadState reads d's State; an error wrapping os.ErrNotExist means there
// is no VM.
func (d Dir) ReadState() (State, error) {
	var st State
	b, err := os.ReadFile(d.State())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", d.State(), err)
	}
	return st, nil
}

// WriteState writes st, making d first.
func (d Dir) WriteState(st State) error {
	return d.writeJSON(d.State(), st)
}

// WriteMachine writes m for vmm.
func (d Dir) WriteMachine(m Machine) error {
	return d.writeJSON(d.Machine(), m)
}

// writeJSON replaces path with v's JSON, through a rename, so a reader
// never sees half of it.
func (d Dir) writeJSON(path string, v any) error {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(string(d), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
