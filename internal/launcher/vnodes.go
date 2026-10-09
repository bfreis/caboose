package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// A Mac running vm holds one open file in its Virtualization process for
// every file and directory the guest has cached from the shares, so a
// guest that walks a large repo can use up kern.maxvnodes (263,168 by
// default on a 64 GB Mac), after which every process on the Mac fails
// with "Too many open files in system". wantMaxVnodes is what caboose
// asks for; docs/vm.md has the numbers.
const wantMaxVnodes = 1048576

// maxVnodesPlist is where setup makes the setting stick across reboots,
// and maxVnodesLabel its launchd label.
const (
	maxVnodesLabel = "dev.caboose.maxvnodes"
	maxVnodesPlist = "/Library/LaunchDaemons/" + maxVnodesLabel + ".plist"
)

// maxVnodesNow is the Mac's kern.maxvnodes, a variable so that tests can
// play it; errNoVnodes is its answer where there is no such setting to read
// (not a Mac).
var (
	maxVnodesNow = sysMaxVnodes
	errNoVnodes  = errors.New("kern.maxvnodes is a macOS setting")
)

// maxVnodesLow says whether cur, a kern.maxvnodes read as err said, is
// below what vm wants. A value that cannot be read is not low: nothing is
// known to be wrong.
func maxVnodesLow(cur uint32, err error) bool {
	return err == nil && cur < wantMaxVnodes
}

// maxVnodesFix is the one-line command that raises the limit until the
// next reboot.
func maxVnodesFix() string {
	return "sudo sysctl kern.maxvnodes=" + strconv.Itoa(wantMaxVnodes)
}

// maxVnodesLine is the doctor's problem text for a Mac whose limit is cur.
func maxVnodesLine(cur uint32) string {
	return fmt.Sprintf("kern.maxvnodes is %d: a VM that walks a large repo holds a host file open for each one it caches, "+
		"and when they run out every process on this Mac fails with 'Too many open files in system'", cur)
}

// doctorMaxVnodes checks, under vm on a Mac, that kern.maxvnodes leaves
// room for the files a VM holds open.
func (a *App) doctorMaxVnodes(c *checkup) {
	cur, err := maxVnodesNow()
	switch {
	case errors.Is(err, errNoVnodes):
		return
	case err != nil:
		c.unchecked("file limit", "cannot read kern.maxvnodes: %v", err)
	case maxVnodesLow(cur, err):
		c.problem("file limit", fmt.Sprintf("%s (until the next reboot), or %s to make it stick", maxVnodesFix(), SetupCommand(a.Cfg.Env, "isolation")),
			"%s", maxVnodesLine(cur))
	default:
		c.ok("file limit", "kern.maxvnodes is %d", cur)
	}
	a.doctorVMFiles(c)
}

// doctorVMFiles says how many of the Mac's vnodes the running VM holds
// open, one per file or directory its guest has cached from the shares.
// A VM that is not running, or not found, is no finding.
func (a *App) doctorVMFiles(c *checkup) {
	held, max, _, err := a.vmFilesHeld()
	if err != nil || max <= 0 {
		return
	}
	line := fmt.Sprintf("the VM holds %s of the Mac's %s vnodes open", thousands(held), thousands(max))
	if held*100 >= max*vnodeWarnPct {
		c.problem("VM's open files", "in the sandbox, sync; echo 2 > /proc/sys/vm/drop_caches lets go of them (the next walk of a large repo starts cold); fewer worktrees inside a repo keeps them down", "%s", line)
		return
	}
	c.ok("VM's open files", "%s", line)
}

// maxVnodesPlistText is the LaunchDaemon that sets kern.maxvnodes at boot.
func maxVnodesPlistText() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + maxVnodesLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/sbin/sysctl</string>
		<string>kern.maxvnodes=` + strconv.Itoa(wantMaxVnodes) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

// maxVnodesCommands are the commands, each run through sudo, that raise
// the limit now and at every boot; src is the plist as written for
// install to copy.
func maxVnodesCommands(src string) [][]string {
	return [][]string{
		{"install", "-m", "0644", "-o", "root", "-g", "wheel", src, maxVnodesPlist},
		{"sysctl", "kern.maxvnodes=" + strconv.Itoa(wantMaxVnodes)},
		{"launchctl", "bootstrap", "system", maxVnodesPlist},
	}
}

// runSudo runs sudo with args on the terminal, which it asks the password
// on; a variable so that tests can play it.
var runSudo = func(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

// offerMaxVnodes, under vm on a Mac whose kern.maxvnodes is below
// wantMaxVnodes, offers to raise it and keep it raised: showing the
// LaunchDaemon and the commands, asking, and only then running them
// through sudo. Declined, it says the one command to run later. It fails
// nothing.
func (a *App) offerMaxVnodes(p *prompter, isolation string) error {
	if isolation != isolationVM {
		return nil
	}
	cur, err := maxVnodesNow()
	if !maxVnodesLow(cur, err) {
		return nil
	}
	p.blank()
	p.warn("%s.", maxVnodesLine(cur))
	p.say("caboose can raise it to %d now and at every boot, with a LaunchDaemon, %s:", wantMaxVnodes, maxVnodesPlist)
	p.blank()
	for _, l := range strings.Split(strings.TrimRight(maxVnodesPlistText(), "\n"), "\n") {
		p.say("    %s", l)
	}
	p.blank()
	src := "<the plist above, in a temporary file>"
	for _, cmd := range maxVnodesCommands(src) {
		p.say("    sudo %s", strings.Join(cmd, " "))
	}
	p.blank()
	ok, err := p.yesNo("Run these through sudo, which asks for your password?", true)
	if err != nil {
		return err
	}
	if !ok {
		p.same("Not changed. To raise it until the next reboot, run %s; %s offers the rest again.",
			p.code(maxVnodesFix()), p.code(SetupCommand(a.Cfg.Env, "isolation")))
		return nil
	}
	f, err := os.CreateTemp("", "caboose-maxvnodes-*.plist")
	if err != nil {
		p.fail("Not changed: %v", err)
		return nil
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(maxVnodesPlistText())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		p.fail("Not changed: %v", err)
		return nil
	}
	for _, cmd := range maxVnodesCommands(f.Name()) {
		if err := runSudo(cmd...); err != nil {
			if cmd[0] == "launchctl" {
				// The limit is raised already; only the boot-time setting is not.
				p.warn("kern.maxvnodes is raised, but 'launchctl bootstrap' failed (%v); if %s is loaded already that is why, "+
					"else run 'sudo launchctl bootstrap system %s' yourself.", err, filepath.Base(maxVnodesPlist), maxVnodesPlist)
				return nil
			}
			p.fail("'sudo %s' failed (%v), so nothing more was run. To raise it until the next reboot, run %s.",
				cmd[0], err, p.code(maxVnodesFix()))
			return nil
		}
	}
	p.ok("kern.maxvnodes is %d, now and at every boot (%s)", wantMaxVnodes, maxVnodesPlist)
	return nil
}
