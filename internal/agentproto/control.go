package agentproto

// The control port is how the host runs a vm guest, where docker run, the
// polling for the entrypoint's ready file and docker stop are today. The
// host dials, opens a Session as on the link and sends TypeHello, then
// TypeClock, so the clock is right before anything runs, then TypeBoot.
// The agent answers with its own hello and, once the sandbox is ready or
// could not be made so, TypeReady (Error says why not). A boot on a guest
// already booted is answered the same way, so a later launch can connect
// again and learn it is up. The host sends TypeClock again after every
// sleep of the host's, and TypeShutdown to stop the guest.

// The ports the agent listens on in a vm guest; the host dials each
// through the VM's vsock (vm.sock, internal/hvsock).
const (
	PortControl = 1024
	PortExec    = 1025
	PortLink    = 1026
)

// ScratchTmpfs, on a guest kernel's command line, has the init put the
// overlay's upper on tmpfs and leave every disk after the root alone: the
// builder guest's, whose other disks are its cache and its output.
const ScratchTmpfs = "caboose.scratch=tmpfs"

// BootSpec is what docker run's flags say today, for the guest's init.
type BootSpec struct {
	Hostname string `json:"hostname"`
	// Env is every sandbox process's whole environment: the image's, then
	// caboose's.
	Env []string `json:"env,omitempty"`
	// User is "UID:GID", who the sandbox runs as: the image's USER, which
	// the guest cannot read, or root under IS_SANDBOX.
	User   string       `json:"user"`
	Mounts []GuestMount `json:"mounts,omitempty"`
	// Disks are the VM's disks after the root and the scratch disk, each
	// an ext4 filesystem mounted at its target: what the sandbox keeps
	// across restarts, as docker's storage.
	Disks []GuestDisk `json:"disks,omitempty"`
	// Cmd is the entrypoint and its arguments.
	Cmd []string `json:"cmd"`
	// Ready is the file the entrypoint makes once the sandbox is ready.
	Ready string `json:"ready"`
	// WaitResolver has the boot answer ready only once the guest's
	// resolver has answered, or its warm-up gave up: the builder's, whose
	// first steps are lookups made by programs of any libc (the probe's
	// curl in a user's image, dockerd's pulls), for which resolv.conf's
	// options, written for the root's libc, may not retry soon enough.
	// The sandbox leaves it off: its boot does not wait on the network.
	WaitResolver bool `json:"wait_resolver,omitempty"`
	// Egress is where the outbound proxy is to be served (EgressListen),
	// when Env points the sandbox at it (EgressEnv): the agent points ssh
	// at it too, at every boot (sshEgress). "" for none.
	Egress string `json:"egress,omitempty"`
}

// GuestAgent is where a vm guest's init copies itself on the new root, to
// run as the agent from: in every guest, the sandbox's and the builder's,
// and always the launcher's own agent, whatever the image holds.
const GuestAgent = "/run/caboose/agent"

// GuestMount is a directory of one of the VM's virtio-fs shares, mounted
// at Target in the sandbox.
type GuestMount struct {
	Tag string `json:"tag"`
	// Path is within the share, "" for its root.
	Path   string `json:"path,omitempty"`
	Target string `json:"target"`
}

// GuestDisk is one of the VM's disks, mounted at Target.
type GuestDisk struct {
	Device string `json:"device"` // /dev/vdc, /dev/vdd, ...
	Target string `json:"target"`
}
