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
//
// TypeVolume asks the agent for one step of a volume's resize (Volume,
// a VolumeOp), on a connection of its own, one step at a time; the agent
// answers each with a TypeVolume of its own (VolumeInfo, or Error). An
// agent that does not know it never answers, which the host's bounded
// wait tells: the guest's agent is from another caboose.

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
	// ReadOnly binds it read-only, over a share directory the host
	// already serves read-only.
	ReadOnly bool `json:"read_only,omitempty"`
}

// GuestDisk is one of the VM's disks, mounted at Target: by its Device,
// one of the framework's own disks, whose names follow their order; or by
// its Serial, one of caboose-vmm's block devices, which the guest names
// in the order it finds them.
type GuestDisk struct {
	Device string `json:"device,omitempty"` // /dev/vdc, /dev/vdd, ...
	Serial string `json:"serial,omitempty"`
	Target string `json:"target"`
}

// The steps of a volume's resize (VolumeOp.Op). The volume is named by
// its disk's serial, its name, and is one of the boot's disks; the agent
// finds its device and target itself.
const (
	// VolumeUsage says what the volume holds, and whether the image has
	// what a shrink runs: VolumeInfo, changing nothing.
	VolumeUsage = "usage"
	// VolumeGrow grows the mounted filesystem, online, to fill its disk,
	// once the disk is Size bytes (the host grew it: the guest hears of
	// the new size a moment later).
	VolumeGrow = "grow"
	// VolumeUnmount trims the volume and unmounts it, refusing while any
	// process holds a file, a directory or a mapping on it
	// (VolumeInfo.Holders) or anything is mounted inside it, and mounting
	// it again when its filesystem stays in use elsewhere (another mount
	// namespace's mount of it).
	VolumeUnmount = "unmount"
	// VolumeShrinkFS checks the unmounted filesystem (e2fsck), discarding
	// its free blocks, and shrinks it to Size bytes (resize2fs), each to
	// its end, however long it takes.
	VolumeShrinkFS = "shrinkfs"
	// VolumeMount mounts the volume again, as a boot does, and grows its
	// filesystem to fill the disk once the disk is Size bytes; a disk
	// still of another size after a bounded wait is mounted ungrown, and
	// said so (VolumeInfo.Note).
	VolumeMount = "mount"
)

// VolumeOp is one step of a volume's resize, from the host.
type VolumeOp struct {
	Op   string `json:"op"`
	Name string `json:"name"`
	// Size is the disk's size in bytes the step is for: Grow's, ShrinkFS'
	// and Mount's.
	Size int64 `json:"size,omitempty"`
}

// The bounds of a VolumeInfo, which the agent keeps to and the host checks
// again: it is the guest's.
const (
	MaxVolumeHolders = 10
	MaxVolumeText    = 1000
)

// VolumeInfo is the agent's answer to a VolumeOp. Everything in it is
// the guest's, which the host bounds and checks before use.
type VolumeInfo struct {
	Op string `json:"op"`
	// Target is where the volume is mounted, Mounted whether it is now.
	Target  string `json:"target,omitempty"`
	Mounted bool   `json:"mounted,omitempty"`
	// DiskBytes is the disk's size as the guest sees it.
	DiskBytes int64 `json:"disk_bytes,omitempty"`
	// FSBytes and UsedBytes are the filesystem's size and what is used of
	// it (statfs), while it is mounted.
	FSBytes   int64 `json:"fs_bytes,omitempty"`
	UsedBytes int64 `json:"used_bytes,omitempty"`
	// Holders are the processes and mounts that keep the volume from
	// being unmounted, at most MaxVolumeHolders, each "PID (COMMAND):
	// PATH" or "mount at PATH"; HoldersMore says how many more there are.
	Holders     []string `json:"holders,omitempty"`
	HoldersMore int      `json:"holders_more,omitempty"`
	// Missing names the tools a shrink runs that the image lacks
	// (e2fsck, resize2fs), "" when it has them: Usage's.
	Missing string `json:"missing,omitempty"`
	// Dirty: a ShrinkFS that failed may have left the filesystem half
	// changed, which must not be mounted again.
	Dirty bool `json:"dirty,omitempty"`
	// Note is what the step has to say beyond its outcome; Error, why it
	// failed.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}
