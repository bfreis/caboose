package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
)

// [session.start] maps a project's host directory to the sandbox directory
// its sessions start in, in place of the path that mirrors the host's
// (containerDir). Only caboose and caboose claude use it; a shell keeps
// the mirrored path, and nothing that names a project's sessions (the
// session's name, the attach records) is keyed on where one starts.

// startFor is the [session.start] entry whose host directory is projectDir
// itself (not a directory below it), judged by identity: another case on a
// case-insensitive volume, a symlink or a firmlink is the same directory,
// as for the roots. An entry whose directory does not exist matches
// nothing. The entries are in order of host directory, so the first match
// is the same on every run.
func (a *App) startFor(projectDir string) (config.StartDir, bool) {
	if len(a.Cfg.SessionStart) == 0 {
		return config.StartDir{}, false
	}
	pfi, err := os.Stat(projectDir)
	if err != nil {
		return config.StartDir{}, false
	}
	for _, s := range a.Cfg.SessionStart {
		if fi, err := os.Stat(s.Host); err == nil && os.SameFile(fi, pfi) {
			return s, true
		}
	}
	return config.StartDir{}, false
}

// startDir is where a session for projectDir starts: the [session.start]
// entry's directory when there is one and the sandbox has it, else def, the
// path mirroring projectDir. A directory it cannot use, and a directory
// check that could not be run, are each said in one line naming the setting
// and both directories (a.Note, and returned as warning, for a caller that
// can show it where the terminal will not hide it). A launch is never
// refused for it. warning is "" when nothing fell back.
func (a *App) startDir(projectDir, def string) (dir, warning string) {
	s, ok := a.startFor(projectDir)
	if !ok {
		return def, ""
	}
	why, err := a.startProblem(s.Target)
	switch {
	case err != nil:
		why = "could not check it in the sandbox (" + firstLine(err.Error()) + ")"
	case why == "":
		return s.Target, ""
	}
	warning = fmt.Sprintf("[session.start] %q = %q: %s; starting in %s", s.Host, s.Target, why, def)
	a.Note("%s", warning)
	return def, warning
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

// doctorSessionStart checks each [session.start] entry: on this machine,
// that its project directory exists and lies under a root (a session can
// start only in a project a root holds); in the sandbox, when it runs,
// that the start directory is there and its volume mounted, by the check a
// launch makes. It creates nothing. A sandbox that is not running is not
// asked, and its directories are said to be unchecked, with notRunning, why
// it could not be asked.
func (a *App) doctorSessionStart(c *checkup, rootsOK, running bool, notRunning string) {
	const label = "session start"
	if len(a.Cfg.SessionStart) == 0 {
		return
	}
	where := ""
	if a.Cfg.File != nil {
		where = " in " + a.Cfg.File.Path
	}
	asked := false
	for _, s := range a.Cfg.SessionStart {
		entry := fmt.Sprintf("[session.start] %q = %q", s.Host, s.Target)
		fixEntry := "change or remove the entry" + where
		if fi, err := os.Stat(s.Host); err != nil || !fi.IsDir() {
			// An entry whose directory is not there matches nothing (a disk
			// that is unplugged, say): worth knowing, not a break.
			c.note(label, "%s: %s is not a directory on this machine, so the entry matches nothing until it is", entry, s.Host)
			continue
		}
		if rootsOK && !slices.ContainsFunc(a.Cfg.Roots, func(r config.Root) bool { return config.HoldsDir(r.Host, s.Host) }) {
			c.problem(label, "put "+s.Host+" under a root ('caboose setup roots'), or "+fixEntry,
				"%s: %s is under no root, so a session cannot be started there", entry, s.Host)
			continue
		}
		if !running {
			asked = true
			continue
		}
		why, err := a.startProblem(s.Target)
		switch {
		case err != nil:
			c.note(label, "%s: could not check %s in the sandbox (%s)", entry, s.Target, firstLine(err.Error()))
		case why != "":
			fix := "make " + s.Target + " a directory the session's user can enter in the sandbox, or " + fixEntry
			if strings.HasPrefix(why, "volume") {
				fix = "run 'caboose restart' to mount the volume" + endsSessions + ", or " + fixEntry
			}
			c.problem(label, fix, "%s: %s; sessions there start in the path that mirrors the project", entry, why)
		default:
			c.ok(label, "%s", entry)
		}
	}
	if asked {
		c.unchecked(label, "%s, and the start directories are checked in it", notRunning)
	}
}

// holdsStartTarget reports whether dir is, or holds, the start directory of
// some [session.start] entry.
func (a *App) holdsStartTarget(dir string) bool {
	for _, s := range a.Cfg.SessionStart {
		if config.Within(s.Target, dir) {
			return true
		}
	}
	return false
}

// startNotice is the tmux command that shows warning in the session's
// client for 15 seconds, to chain after new-session: the line a.Note wrote
// to stderr is hidden by the alternate screen the client switches to. A #
// in the text is escaped, as tmux reads it as the start of a format. It
// returns nil for no warning, and when an argument ends in ";" (tmux would
// read it as the end of the command, and the text is only a second copy of
// what was noted).
func startNotice(argv []string, warning string) []string {
	if warning == "" {
		return nil
	}
	for _, a := range argv {
		if strings.HasSuffix(a, ";") {
			return nil
		}
	}
	text := strings.ReplaceAll(warning, "#", "##")
	if strings.HasSuffix(text, ";") {
		text += " "
	}
	return []string{";", "display-message", "-d", "15000", text}
}
