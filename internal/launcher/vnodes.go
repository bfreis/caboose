package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// A Mac running vm holds one open file in its Virtualization process for
// every file and directory the guest has cached from the shares, so a
// guest that walks a large repo can use up kern.maxvnodes (a few hundred
// thousand by default), after which every process on the Mac fails with
// "Too many open files in system". wantMaxVnodes is what caboose asks
// for; docs/configuration.md ("Large repos") says more.
const wantMaxVnodes = 1048576

// maxVnodesPlist is where setup makes the setting stick across reboots,
// and maxVnodesLabel its launchd label.
const (
	maxVnodesLabel = "dev.bfreis.caboose.maxvnodes"
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
// open, one per file or directory its guest has cached from the shares,
// and how much of the Mac's disk the deleted ones among them keep. A VM
// that is not running, or not found, is no finding.
func (a *App) doctorVMFiles(c *checkup) {
	f, err := a.vnodeFinder()
	if err != nil {
		return
	}
	s, err := f.Sample()
	if err != nil || s.Max <= 0 {
		return
	}
	line := fmt.Sprintf("the VM holds %s of the Mac's %s vnodes open", thousands(s.Files), thousands(s.Max))
	if s.Files*100 >= s.Max*vnodeWarnPct {
		c.problem("VM's open files", "in the sandbox, sync; echo 2 > /proc/sys/vm/drop_caches lets go of them (the next walk of a large repo starts cold); fewer worktrees inside a repo keeps them down", "%s", line)
	} else {
		c.ok("VM's open files", "%s", line)
	}
	c.checking("the deleted files the VM holds")
	d, err := f.Deleted(time.Now().Add(vmDeletedBudget))
	if err != nil {
		c.unchecked("VM's deleted files", "%v", err)
		return
	}
	doctorVMDeleted(c, d)
}

// A file the Mac deletes keeps its space while the VM's process holds it
// open, which it does for as long as the guest has it cached: until
// memory pressure or a cache drop in the guest, however long ago the
// guest last used it.
const (
	// vmDeletedLarge is how much of that is a problem. A sandbox in use
	// keeps a few files of this kind, a few MiB; tens of GiB have been
	// seen kept by files a session deleted in bulk. A GiB is more than
	// the first, and enough of a disk to free at the price of the drop
	// (the guest reads what it needs next from the Mac again).
	vmDeletedLarge = 1 << 30
	// vmDeletedBudget is the longest doctor reads the VM's descriptors.
	vmDeletedBudget = 3 * time.Second
)

// doctorVMDeleted reports d, what the VM holds of files deleted on the
// Mac.
func doctorVMDeleted(c *checkup, d vnodes.Deleted) {
	const label = "VM's deleted files"
	atLeast, partial := "", ""
	if d.Read < d.Open {
		atLeast = "at least "
		partial = fmt.Sprintf(" (%s of its %s open files read in %s)", thousands(d.Read), thousands(d.Open), vmDeletedBudget)
	}
	if d.Files == 0 {
		if d.Read < d.Open {
			c.ok(label, "the VM holds no file deleted on the Mac open among the %s of its %s open files read in %s", thousands(d.Read), thousands(d.Open), vmDeletedBudget)
			return
		}
		c.ok(label, "the VM holds no files deleted on the Mac open")
		return
	}
	line := fmt.Sprintf("the VM holds %s%s %s deleted on the Mac open, %s of disk%s", atLeast, thousands(d.Files), plural(d.Files, "file", "files"), sizeText(d.Bytes), partial)
	if d.Bytes >= vmDeletedLarge {
		c.problem(label, "in the sandbox, sync; echo 2 > /proc/sys/vm/drop_caches lets go of them (the guest reads what it uses next from the Mac again)",
			"%s: the space comes back only when the guest lets go of them", line)
		return
	}
	c.ok(label, "%s", line)
}

// sizeText is n bytes in MiB, or GiB from one up, to one decimal.
func sizeText(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
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
