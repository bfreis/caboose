package conformance

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"

	"github.com/bfreis/caboose/internal/agent"
)

// This file is the one place that spells out the names the rest of the
// package must not: what a sandbox looks like from inside, the sockets no
// mount may be, and the product's own files under the throwaway home. The
// source scan (scan_test.go) allows them here and nowhere else.

// Detector is one sign that this process runs inside a sandbox or a
// container. Present reports whether the sign is there; an error is taken
// as present, since a sign that cannot be read cannot be ruled out.
type Detector struct {
	Name    string
	Present func() (bool, error)
}

// DefaultDetectors are the real signs. Tests pass fakes of their own.
func DefaultDetectors() []Detector {
	return []Detector{
		{"caboose's managed instructions (/etc/claude-code)", exists("/etc/claude-code")},
		{"/.dockerenv", exists("/.dockerenv")},
		{"/run/.containerenv", exists("/run/.containerenv")},
		{"the sandbox's link socket", exists(agent.SocketPath)},
		{"a container runtime in /proc/1/cgroup", cgroupNamesRuntime("/proc/1/cgroup")},
		{"the sandbox's user (agent, HOME=/home/agent, /work)", sandboxUser},
		{"CABOOSE_SESSION in the environment", func() (bool, error) {
			_, ok := os.LookupEnv("CABOOSE_SESSION")
			return ok, nil
		}},
	}
}

func exists(p string) func() (bool, error) {
	return func() (bool, error) {
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
}

// runtimeWords are the names a container runtime leaves in a cgroup path.
var runtimeWords = []string{"docker", "containerd", "kubepods", "libpod", "lxc", "crio", "garden"}

func cgroupNamesRuntime(p string) func() (bool, error) {
	return func() (bool, error) {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			for _, w := range runtimeWords {
				if strings.Contains(s.Text(), w) {
					return true, nil
				}
			}
		}
		return false, s.Err()
	}
}

func sandboxUser() (bool, error) {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name != "agent" || os.Getenv("HOME") != "/home/agent" {
		return false, nil
	}
	return exists("/work")()
}

// sandboxMessage is what a refusal to start in a sandbox says.
const sandboxMessage = "this is a sandbox; run the conformance suite on the host"

// detectSandbox runs every detector and refuses on the first one present.
func detectSandbox(ds []Detector) error {
	if ds == nil {
		ds = DefaultDetectors()
	}
	for _, d := range ds {
		found, err := d.Present()
		if found || err != nil {
			why := d.Name
			if err != nil {
				why += fmt.Sprintf(" (could not be read: %v)", err)
			}
			return fmt.Errorf("%s: found %s", sandboxMessage, why)
		}
	}
	return nil
}

// LinuxNote is what the results file says of a Linux host.
const LinuxNote = "Linux hosts are untested"

// checkPlatform refuses every platform but darwin and linux.
func checkPlatform(goos string) error {
	switch goos {
	case "darwin", "linux":
		return nil
	}
	return fmt.Errorf("the conformance suite runs on macOS or Linux, not %s", goos)
}

// forbiddenSources are host paths no mount or share may have as its
// source, whatever else the audit finds: the engine's sockets and the SSH
// agent's. sshAuthSock is the caller's SSH_AUTH_SOCK, compared as a path.
func forbiddenSources() []string {
	s := []string{
		"/var/run/docker.sock",
		"/run/docker.sock",
		"/run/host-services/ssh-auth.sock",
		"/run/host-services/docker.proxy.sock",
	}
	if p := os.Getenv("SSH_AUTH_SOCK"); p != "" {
		s = append(s, p)
	}
	return s
}

// productWritten are the paths under B that the product itself writes
// while the suite runs (the daemon, the link, the entrypoint, Claude Code's
// install, logs, the broker's state), so the post-check walk does not
// report them. Each is slash-separated and relative to B; "<env>" stands
// for the environment's name, and "<data>" for its data dir, the
// environment's default (home/envs/<env>/data). A pattern ending in "/" is
// a whole subtree; one with "*" matches within its last element only. The
// canaries placed inside them are still compared, by identity. Each entry
// is under home/, never roots/ or canary/, and says what of the product's
// writes it; one is added only for a write the product's code makes.
var productWritten = []string{
	"home/daemon/",     // the daemon's directory (daemon.Dirs): socket, lock, log, alias-tmp
	"home/daemon-dev/", // the same for a checkout's build
	"home/envs/<env>/*.log",
	"home/envs/<env>/grants.json",
	"home/envs/<env>/requests.json",
	// broker.lockFile: the link's broker takes it as the link starts
	// (launcher linkbroker.go, broker.Open).
	"home/envs/<env>/broker.lock",
	// writeConfig's temporary file beside config.toml, renamed over it
	// (launcher setup.go replaceConfig). config.toml itself is no product
	// path: during a run only the suite rewrites it (the trust and egress
	// keys), and a change to it is allowed only to the contents the suite
	// last wrote (Guard.NoteConfigWritten). No check answers "always", the
	// broker's one write of it (launcher configwriter.go).
	"home/envs/<env>/.config.toml-*",
	// The checkpoint store (checkpoint.Open on EnvDir/checkpoints): every
	// take and prune writes it.
	"home/envs/<env>/checkpoints/",
	// The apko package cache (launcher apko.go, CABOOSE_HOME/cache/apk):
	// building the image fills it, and apko expands packages there into
	// temporary tars larger than the suite hashes. No mount reaches it,
	// and nothing else of the product's writes under home/cache.
	"home/cache/apk/",
	"<data>/local/",
	"<data>/home/.claude*",
	"<data>/link.*",
	// datadir.ManagedDir: InstallManaged writes it on every launch.
	"<data>/claude-code/",
	// datadir.SyncDir: the sandbox's caboose sync works on it, mounted at
	// ~/.caboose-sync.
	"<data>/sync/",
	// datadir.ProposalsDir: the sandbox writes proposals there, mounted at
	// ~/.caboose-proposals, and a launch writes current/ (exportProposals).
	"<data>/proposals/",
	// The sandbox's ~/.config, a kept directory the sandbox writes
	// (sandboxcfg's defaults: ~/.config/caboose, git, jj, gh).
	"<data>/home/.config/",
	// datadir.LastGoodConfig: written whenever the sandbox config is read
	// and parses (datadir.LoadSandboxConfig).
	"<data>/sandbox.last-good.toml",
}

// vmProductWritten are productWritten's entries for what only the vm
// isolation writes: under container and gvisor these subtrees are walked
// like any other path.
var vmProductWritten = []string{
	// A release build's kernel and builder, fetched and pruned only when a
	// vm sandbox needs them (launcher vmassets.go, CABOOSE_HOME/vm).
	"home/vm/",
	// The data dir's vm/: each VM's directory, the image store, the
	// initramfs and the volumes (launcher vm.go vmRoot), used only when
	// the isolation is vm.
	"<data>/vm/",
}

// suiteWritten are the files under B the suite's own stubs append to while
// the run goes on: exact paths, never a pattern, each in canary/, which no
// mount reaches. The walk leaves them out; StubCalls reads them.
var suiteWritten = []string{StubLog}
