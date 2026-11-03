package launcher

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
)

// sandboxHome is the session user's home in the sandbox: where a session
// starts when the sandbox has no roots and nothing names a directory.
const sandboxHome = "/home/agent"

// dirSource is how a launch's directory was chosen.
type dirSource int

const (
	// dirMirrored: the directory caboose was run from is under a root, and
	// the session starts at its path there. Never probed.
	dirMirrored dirSource = iota
	// dirFlag: -C.
	dirFlag
	// dirSetting: [session] dir.
	dirSetting
	// dirHome: no root, no -C and no [session] dir, in a sandbox with no
	// roots: the home directory.
	dirHome
)

// launchDir is the sandbox directory a launch works in. A session's name
// is made from Path however it was chosen, so the routes to one directory
// are one session.
type launchDir struct {
	Path   string
	Source dirSource
}

// dirChoice is what pickDir decides from. Cwd is the directory caboose was
// run from, physical, "" when it could not be told. Mounted is what the
// sandbox has mounted and Known whether it could be asked (mountedRootsKnown):
// an unknown sandbox is judged by the configured roots, which are about to
// be mounted; a known one by what it has, even when that is nothing.
type dirChoice struct {
	Flag, Setting string
	Cwd           string
	Mounted       []config.Root
	Known         bool
	Configured    []config.Root
	Origin, Noun  string
}

// pickDir chooses the directory of a launch, in this order:
//
//  1. -C DIR, taken as written when absolute, else relative to [session] dir.
//  2. The directory caboose was run from, when a root the sandbox has
//     mounted holds it: the path that mirrors it there.
//  3. [session] dir.
//  4. The home directory, when the sandbox has no roots.
//  5. Otherwise a refusal that says how to give a directory.
//
// A directory the host's roots hold but the sandbox has not mounted (a root
// added since it was created) is a refusal that says to restart, before 3:
// the project would otherwise start somewhere it is not. It looks at
// nothing in the sandbox: whether a directory is there is startIn's.
func pickDir(c dirChoice) (launchDir, error) {
	if c.Flag != "" {
		p, err := config.ResolveStartDir(c.Flag, c.Setting)
		if err != nil {
			return launchDir{}, Die("%v", err)
		}
		return launchDir{p, dirFlag}, nil
	}
	roots := c.Configured
	if c.Known {
		roots = c.Mounted
	}
	if c.Cwd != "" {
		if p, ok := config.ContainerPath(roots, c.Cwd); ok {
			return launchDir{p, dirMirrored}, nil
		}
		if c.Known && !config.SameRoots(c.Mounted, c.Configured) {
			if _, ok := config.ContainerPath(c.Configured, c.Cwd); ok {
				_, err := CheckInsideRoot(c.Cwd, c.Mounted, c.Known, c.Configured, c.Origin, c.Noun)
				return launchDir{}, err
			}
		}
	}
	if c.Setting != "" {
		return launchDir{c.Setting, dirSetting}, nil
	}
	if len(roots) == 0 {
		return launchDir{sandboxHome, dirHome}, nil
	}
	_, err := CheckInsideRoot(c.Cwd, c.Mounted, c.Known, c.Configured, c.Origin, c.Noun)
	return launchDir{}, err
}

// pickLaunchDir is pickDir for this launch: the flag, the setting, this
// process's directory and the roots as they are now. A directory that
// cannot be told is no reason to fail when nothing needs it.
func (a *App) pickLaunchDir() (launchDir, error) {
	c := dirChoice{Flag: a.Dir, Setting: a.Cfg.SessionDir, Configured: a.Cfg.Roots, Origin: a.Cfg.RootsOrigin(), Noun: a.noun()}
	c.Mounted, c.Known = a.mountedRootsKnown()
	var cwdErr error
	if a.Dir == "" {
		c.Cwd, cwdErr = enterProjectDir(".")
	}
	d, err := pickDir(c)
	if err != nil && cwdErr != nil {
		return launchDir{}, cwdErr
	}
	return d, err
}

// startIn is where a launch can start for d, and the warning when it is not
// d: the mirrored path and the home are not looked at. -C that the session's
// user cannot enter is a refusal, no session started; [session] dir that
// cannot be entered, or whose volume is not mounted, or that cannot be
// checked, is the home directory with one line naming the setting and both
// directories (a.Note, and warning for a caller that can show it where the
// terminal will not hide it).
func (a *App) startIn(d launchDir) (dir, warning string, err error) {
	switch d.Source {
	case dirFlag, dirSetting:
	default:
		return d.Path, "", nil
	}
	why, cerr := a.startProblem(d.Path)
	if cerr != nil {
		why = "could not check it in the sandbox (" + firstLine(cerr.Error()) + ")"
	}
	switch {
	case why == "":
		return d.Path, "", nil
	case d.Source == dirFlag:
		return "", "", Die("-C %s: %s.\n       -C takes a directory in the sandbox, not on this machine (a ~ is expanded by your shell\n       to a path here). No session was started. If it is not there yet, make it from\n       'caboose shell' (git clone ..., mkdir -p ...), then run this again.",
			d.Path, why)
	}
	warning = fmt.Sprintf("[session] dir = %q: %s; starting in %s, the home directory", d.Path, why, sandboxHome)
	a.Note("%s", warning)
	return sandboxHome, warning, nil
}

// startProblem says why target cannot be a session's start directory in
// the running sandbox: "" when it can, else the reason as a clause. The
// error is for a check that could not be run.
//
// A volume's path exists before the volume is mounted, as an empty
// directory of the disk under it, so entering it proves nothing there:
// a target at or below a volume's path is first checked against the mounts.
// Where the mounts cannot be read, or show none, that check is skipped
// without a word, and entering the directory alone decides.
func (a *App) startProblem(target string) (string, error) {
	if v, ok := a.volumeHolding(target); ok {
		if points, err := a.mountPoints(); err == nil && len(points) > 0 {
			mounted := false
			for _, p := range points {
				if p == v.Path {
					mounted = true
				}
			}
			if !mounted {
				return fmt.Sprintf("volume %q is not mounted", v.Name), nil
			}
		}
	}
	can, err := a.sandboxCanEnter(target)
	switch {
	case err != nil:
		return "", err
	case !can:
		return "cannot enter " + target + " in the sandbox (it is missing, not a directory, or not allowed to the session's user)", nil
	}
	return "", nil
}

// volumeHolding is the profile's volume whose path is target or holds it,
// the innermost one.
func (a *App) volumeHolding(target string) (config.Volume, bool) {
	var best config.Volume
	found := false
	for _, v := range a.Cfg.Volumes {
		if config.Within(target, v.Path) && (!found || len(v.Path) > len(best.Path)) {
			best, found = v, true
		}
	}
	return best, found
}

// enterFailed is the status the probe exits with when it cannot enter the
// directory: only that status means so. Any other failure (a missing shell,
// the engine's own errors, which use 1 and 125 to 127) is a check that did
// not run.
const enterFailed = 3

// sandboxCanEnter says whether the session's user can cd into target in the
// sandbox, with the shell and nothing else, which every base has
// (imagecheck.sh requires it). Status enterFailed is the no; any other
// failure is an error.
func (a *App) sandboxCanEnter(target string) (bool, error) {
	err := a.box().Command(backend.ExecSpec{Argv: []string{"/bin/sh", "-c", `cd -- "$1" || exit 3`, "sh", target}}).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == enterFailed:
		return false, nil
	}
	return false, err
}

// readMountinfo prints the sandbox's /proc/self/mountinfo with shell
// builtins alone, so that it needs no tool a base may lack.
var readMountinfo = []string{"/bin/sh", "-c", `while IFS= read -r l; do printf '%s\n' "$l"; done < /proc/self/mountinfo`}

// mountPoints are the sandbox's mount points.
func (a *App) mountPoints() ([]string, error) {
	out, err := backend.RawOutput(a.box(), readMountinfo...)
	if err != nil {
		return nil, err
	}
	return parseMountPoints(out), nil
}

// parseMountPoints is the mount points in mountinfo's text: the fifth
// field of each line, whose space, tab, newline and backslash the kernel
// writes as octal escapes (\040).
func parseMountPoints(text string) []string {
	var points []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		points = append(points, unescapeMountinfo(f[4]))
	}
	return points
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// startNotice is the tmux command that shows warning in the session's
// client for 15 seconds, to chain after new-session: the line a.Note wrote
// to stderr is hidden by the alternate screen the client switches to. A #
// and a % in the text are escaped, as tmux reads # as the start of a format
// and % as a strftime conversion. It returns nil for no warning, and when
// an argument ends in ";" that is not escaped as "\;" (tmux would read it
// as the end of the command, and the text is only a second copy of what
// was noted).
func startNotice(argv []string, warning string) []string {
	if warning == "" {
		return nil
	}
	for _, a := range argv {
		if strings.HasSuffix(a, ";") && !strings.HasSuffix(a, `\;`) {
			return nil
		}
	}
	text := strings.ReplaceAll(strings.ReplaceAll(warning, "#", "##"), "%", "%%")
	if strings.HasSuffix(text, ";") {
		text += " "
	}
	return []string{";", "display-message", "-d", "15000", text}
}

// doctorSessionDir checks [session] dir by the check a launch makes: in the
// running sandbox, that the directory is there and its volume mounted. It
// creates nothing. A sandbox that is not running is not asked, and the
// directory is said to be unchecked, with notRunning, why it could not be
// asked.
func (a *App) doctorSessionDir(c *checkup, running bool, notRunning string) {
	const label = "session dir"
	dir := a.Cfg.SessionDir
	if dir == "" {
		return
	}
	where := ""
	if a.Cfg.File != nil {
		where = " in " + a.Cfg.File.Path
	}
	fixSetting := "change or remove dir in [session]" + where
	if !running {
		c.unchecked(label, "%s, and the directory is checked in it", notRunning)
		return
	}
	why, err := a.startProblem(dir)
	switch {
	case err != nil:
		c.note(label, "[session] dir = %q: could not check it in the sandbox (%s)", dir, firstLine(err.Error()))
	case why != "":
		fix := "make " + dir + " a directory the session's user can enter in the sandbox, or " + fixSetting
		if strings.HasPrefix(why, "volume") {
			fix = "run 'caboose restart' to mount the volume" + endsSessions + ", or " + fixSetting
		}
		c.problem(label, fix, "[session] dir = %q: %s; a session started outside the roots would start in %s instead", dir, why, sandboxHome)
	default:
		c.ok(label, "[session] dir = %q", dir)
	}
}

// startPaths are the sandbox paths a project's sessions work in that no
// root holds: the profile's volumes and [session] dir.
func (a *App) startPaths() []string {
	var out []string
	for _, v := range a.Cfg.Volumes {
		out = append(out, v.Path)
	}
	if a.Cfg.SessionDir != "" {
		out = append(out, a.Cfg.SessionDir)
	}
	return out
}

// holdsStartPath reports whether dir is, or holds, a volume's path or
// [session] dir.
func (a *App) holdsStartPath(dir string) bool {
	return slices.ContainsFunc(a.startPaths(), func(p string) bool { return config.Within(p, dir) })
}
