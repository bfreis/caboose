// Package launcher attaches Claude Code sessions to the long-lived caboose
// sandbox container, and implements caboose's own commands (status, restart, sync, ...).
//
// The container outlives any individual session -- it is not a `docker run
// --rm` per session. That is what makes the agents/fleet feature usable: the
// daemon registers sessions by PID and by unix socket under /tmp, so every
// session that should see every other one has to live in the same container.
// One container, many tmux sessions, many repos.
//
// The container holds no state of its own — everything Claude Code persists
// is bind-mounted out of the data dir — so `caboose restart` remains a safe
// no-op at any time, other than killing whatever is currently running
// inside.
//
// That dir defaults to ~/.caboose/envs/<env>/data, deliberately OUTSIDE any
// checkout. Inside
// one, the live OAuth credential and every session transcript would be
// guarded only by .gitignore — a file the VCS itself tracks: a history
// rewrite that checks out a commit predating it deletes it, un-ignores the
// state, lets the next snapshot take it, and deletes it on the following
// working-copy update. A checkout is something you rewrite, clean and throw
// away; machine state is not.
//
// Mounts:
//
//	$CABOOSE_DATA_DIR                  (default ~/.caboose/envs/<env>/data)
//	$CABOOSE_DATA_DIR/home/<path>     -> ~/<path>, for each [[keep]] entry of the
//	    sandbox config (~/.config/caboose/sandbox.toml): ~/.claude, ~/.claude.json,
//	    ~/.config/caboose (start.d, shell.d, the sandbox config), ~/.config/git,
//	    ~/.config/jj, ~/.config/gh, ~/.ssh by default, and whatever the user adds
//	$CABOOSE_DATA_DIR/local/<platform>/bin           -> ~/.local/bin
//	$CABOOSE_DATA_DIR/local/<platform>/share/claude  -> ~/.local/share/claude
//	$CABOOSE_DATA_DIR/local/<platform>/cache/claude  -> ~/.cache/claude
//	    (<platform> is the image's Claude Code build: linux-x64, linux-arm64-musl, ...)
//	$CABOOSE_DATA_DIR/sync            -> ~/.caboose-sync (caboose sync's repo; git runs it in here)
//	$CABOOSE_DATA_DIR/proposals       -> ~/.caboose-proposals (sessions' proposals for caboose apply)
//	each root in config.toml's [roots] -> /work/<name>, or its long form's path
//
// The caboose checkout is NOT mounted separately. It lives under the repo
// root like any other checkout, so it is already visible under /work; a
// second path for the same files would buy nothing but confusion about
// which one to edit. Cloned outside every root it is simply not visible
// from inside, and is edited from the host.
//
// The roots are mounted at fixed paths, not at their host paths, and a
// session's container cwd is the host cwd's place under them:
// ~/dev/you/caboose is /work/you/caboose on every machine, whatever the
// user name. Claude Code keys per-project state off the cwd, so the keys
// are the same everywhere too, and caboose sync carries them as they are.
//
// tmux here has no prefix key (it must not intercept keystrokes from the
// TUI), so detaching is either closing the terminal or caboose detach from
// another one. tmux = false in [session] skips tmux entirely (native rendering, but
// closing the terminal kills the session instead of detaching).
//
// Old versions are also pruned automatically at container start, retaining
// keep_versions in [session] (default 2) plus whatever the launcher symlink
// points at. Start-up pruning uses the value baked in when the container was
// created, so changing it needs a caboose restart to take effect; caboose prune
// always uses the current value.
package launcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
)

// Entrypoint is where the image installs entrypoint.sh.
const Entrypoint = "/usr/local/bin/caboose-entrypoint"

// ExitError ends the launcher with Code, printing Msg (if any) as
// "caboose: Msg" first. An empty Msg means whatever failed has already
// explained itself on stderr, as docker does.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Msg
}

// Die is a message and exit status 1.
func Die(format string, args ...any) error {
	return &ExitError{Code: 1, Msg: fmt.Sprintf(format, args...)}
}

// dockerFailed passes a failed docker command's status through. docker has
// already said why on stderr -- unless it never ran.
func dockerFailed(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return &ExitError{Code: 127, Msg: fmt.Sprintf("cannot run docker: %v", err)}
	}
	return &ExitError{Code: docker.ExitCode(err)}
}

// boxFailed is dockerFailed for whatever the sandbox is: under vm there is
// no docker to have said why, so the error says it, or the command's own
// status stands.
func (a *App) boxFailed(err error) error {
	if !a.isVM() {
		return dockerFailed(err)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode()}
	}
	return Die("%v", err)
}

// noun is what the sandbox is called in what the user reads: a VM under
// vm, a container under docker and gvisor, where it is one.
func (a *App) noun() string {
	if a.isVM() {
		return "VM"
	}
	return "container"
}

// nounLabel is noun as the label of a status or version line, padded as
// the others are ("container : NAME (running)").
func (a *App) nounLabel() string { return fmt.Sprintf("%-9s :", a.noun()) }

// box is the sandbox: Backend, or the configured container.
func (a *App) box() backend.Backend {
	if a.Backend != nil {
		return a.Backend
	}
	if a.isVM() {
		return a.vmBox()
	}
	return backend.NewDocker(a.Docker, a.Cfg.Container)
}

// App is one launcher invocation.
type App struct {
	Cfg *config.Config
	// Docker is the engine: images, builds, and what it says of itself.
	// The sandbox is Backend's.
	Docker *docker.CLI
	// Backend, when set, is the sandbox; otherwise it is the container
	// Cfg names on Docker (App.box).
	Backend backend.Backend
	// Checkout is the host path of the caboose checkout this launcher runs
	// from, or "" when it is an installed binary with no checkout around.
	Checkout string
	// Suffix is the --session / CABOOSE_SESSION name, or "".
	Suffix string
	Stdout io.Writer
	Stderr io.Writer
	// HostPart, when set, stands in for the machine's name as hostname.go
	// finds it: the tests'.
	HostPart func() string

	// install is what the container being brought up installs before it is
	// ready (noteInstall), for waitUntilReady to say.
	install pendingInstall
	// image is what ensureImage returned, once it succeeded.
	image struct {
		ready  bool
		labels map[string]string
	}
	// Terminal, when set, stands in for the terminal caboose setup asks its
	// questions on (openTerminal): the tests' scripted answers.
	Terminal func() (io.ReadCloser, error)
	// inSetup is set while caboose setup runs: a launch's "not set up"
	// line would only interrupt it.
	inSetup bool
	// sb is the sandbox config, once read (App.sandboxConfig).
	sb *datadir.Sandbox
	// lossConfirmed is set once the user has said yes to ending the
	// running sessions (caboose apply's restart): confirmSessionLoss does
	// not ask again.
	lossConfirmed bool
	// attach, when set, stands in for Attach at the end of setup: the
	// tests' way to see it called without attaching anything.
	attach func(args []string) error
	// Executable, Spawn and Now, when set, stand in for os.Executable,
	// starting a detached process, and time.Now: the updater's, for tests.
	Executable func() (string, error)
	Spawn      func(exe string, args ...string) error
	Now        func() time.Time
	// checkGuest, when set, stands in for booting the builder guest for
	// caboose check-image under vm: the tests' fake.
	checkGuest func() (imageGuest, error)
	// buildGuest, when set, stands in for booting the builder guest for
	// caboose build under vm (startBuilder): the tests' fake.
	buildGuest func(out, template string) (vmBuildGuest, error)
	// apko, when set, stands in for internal/apkobuild in an apko build:
	// the tests' way to build without the network.
	apko *apkoTools
	// buildImage, when set, stands in for building the image when caboose
	// apply applies packages (App.build, on the configuration it is
	// called with): the tests' fake.
	buildImage func() error
	// deferLock, set by caboose apply around its build, keeps an apko
	// build from writing the lock: it leaves writing it in pendingLock
	// instead, for apply to call once config.toml says what the lock is
	// of. nil when the build had no lock to write.
	deferLock   bool
	pendingLock func() error
	// vmFiles, when set, stands in for finding vm's files beside the
	// launcher (findVMFiles), which only a Mac has: the tests' way to
	// take the vm isolation's paths anywhere.
	vmFiles func() (vmFiles, error)
	// terminal and replace, when set, stand in for asking whether caboose
	// runs on a terminal (isTerminal) and for replacing this process with
	// a command (exec): the tests' way to run what ends in one.
	terminal func() bool
	replace  func(*exec.Cmd) error
}

// Note prints "caboose: ..." to stderr.
func (a *App) Note(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "caboose: "+format+"\n", args...)
}

func (a *App) getenv(k string) string { return a.Cfg.Getenv(k) }

// force reports whether CABOOSE_FORCE skips the questions before ending
// sessions or deleting, read as config.EnvBool reads it (Load refused any
// other value).
func (a *App) force() bool {
	on, _ := config.EnvBool(a.getenv, "CABOOSE_FORCE")
	return on
}

// indent prefixes every line of s, including a final unterminated one, as
// `sed 's/^/prefix/'` does.
func indent(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			b.WriteString(prefix)
			b.WriteString(l)
		}
	}
	return b.String()
}

// HostTimezone derives the host's zone NAME (not its offset, which would be
// wrong again at the next DST change).
//
// Containers run in UTC unless told otherwise, so every timestamp Claude
// Code renders -- and every date a tool inside writes into a file -- is off
// by the host's UTC offset.
//
// tz in [session] wins, then $TZ. Otherwise read /etc/localtime: a symlink into
// the zoneinfo tree on both macOS (/var/db/timezone/zoneinfo/<Zone>) and
// Linux (/usr/share/zoneinfo/<Zone>, sometimes relative), so everything after
// the last 'zoneinfo/' is the name. Debian's /etc/timezone covers the distros
// that copy the file instead of linking it. Returns "" if none of that works.
func HostTimezone(cabooseTZ, tz string, readlink func(string) (string, error), readFile func(string) ([]byte, error)) string {
	if cabooseTZ != "" {
		return cabooseTZ
	}
	if tz != "" {
		return tz
	}
	if link, err := readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(link, "/zoneinfo/"); i >= 0 {
			return link[i+len("/zoneinfo/"):]
		}
	}
	b, err := readFile("/etc/timezone")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, first)
}

func (a *App) hostTimezone() string {
	return HostTimezone(a.Cfg.TZ, a.getenv("TZ"), os.Readlink, os.ReadFile)
}

// DockerSockPath is the host socket engine_socket asks to mount, or ""
// for none: DOCKER_HOST's, when it is a unix:// one, else the default.
//
// OFF BY DEFAULT, and the default is the point: /var/run/docker.sock is
// root-equivalent access to the HOST. Anything that reaches it can run
// `docker run -v /:/host --privileged` and own the machine, which removes the
// isolation this project exists to provide. The realistic threat is not the
// model misbehaving but an injection arriving in the untrusted input it reads
// all day -- repo contents, web pages, MCP responses -- and escalating from a
// container to your laptop.
//
// Note also that it does NOT let the suite run from inside: tests/run.sh
// restarts the container on its third line, so it would destroy the session
// running it. What it buys is inspect/logs/exec, not self-testing.
//
// Baked in at creation, so changing it takes a caboose restart.
func DockerSockPath(on bool, dockerHost string) string {
	if !on {
		return ""
	}
	if strings.HasPrefix(dockerHost, "unix://") {
		return strings.TrimPrefix(dockerHost, "unix://")
	}
	return "/var/run/docker.sock"
}

func isSocket(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
