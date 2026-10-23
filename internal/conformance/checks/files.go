package checks

import (
	"context"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

// roErr and the like keep the tables below short.
func ro(name string, op c.Op, target string, tkind c.Kind) part {
	return part{name: name, op: op, target: target, tz: zRO, tkind: tkind, allowed: tkind, expect: expErofs}
}

func dev(name string, op c.Op, target string, tkind c.Kind) part {
	return part{name: name, op: op, target: target, tz: zDev, tkind: tkind, allowed: tkind, expect: expErofs}
}

// withControl gives a part a control that is not its own operation: the
// attempt is refused wherever it is made, so the control only shows that
// the sandbox is up and the writable part takes a write.
func withControl(p part) part {
	p.ctrl = &appendOp
	p.allowed = c.File
	return p
}

func r1() fileCheck {
	return fileCheck{
		id: "R1", title: "a read-only root refuses writes", control: "a read-only root refuses writes",
		class:  c.Write,
		rootIn: []string{"vm"},
		parts: []part{
			ro("append", c.OpAppend, "ro-file", c.File),
			ro("truncate", c.OpTruncate, "ro-file", c.File),
			ro("touch", c.OpTouch, "ro-file", c.File),
			ro("chmod", c.OpChmod, "ro-file", c.File),
			ro("mkdir", c.OpMkdirIn, "ro-dir", c.Dir),
			ro("symlink", c.OpSymlinkIn, "ro-dir", c.Dir),
			ro("hardlink", c.OpHardlinkIn, "ro-tree", c.Tree),
		},
	}
}

func r2() fileCheck {
	return fileCheck{
		id: "R2", title: "a read-only root refuses deletion", control: "a read-only root refuses deletion",
		class: c.Destructive, rootIn: []string{"vm"},
		parts: []part{
			ro("rm", c.OpRemove, "rmf-ro-file", c.File),
			ro("rmdir", c.OpRmdir, "rmd-ro-dir", c.Dir),
			ro("rmtree", c.OpRemoveTree, "rmt-ro-tree", c.Tree),
			ro("rename", c.OpMoveIn, "mv-ro-tree", c.Tree),
		},
	}
}

// r3 tries the root's mount point itself: mv, and rmdir. Never rm -r,
// which works depth first: it would empty the writable part inside the
// root (writable by design, and holding the run's other canaries) before
// failing on the mount point, and its first message would be about some
// read-only entry below the root, not the root.
//
// Linux refuses both as busy (or the read-only filesystem the mount
// point is in), but which error comes first is the isolation's: gVisor's
// VFS refuses an rmdir of a mount point that holds anything as not empty,
// and a rename of the root that holds the writable part with ENOENT, from
// its rootfs overlay's copy-up of what is below it, before it finds
// either busy. So the attempt is judged against the mount itself: the
// root's line of the sandbox's mount table (and the part's below it) must
// be there, once, before and after, unchanged, and the canaries on the
// host as they were; only then do those errors count as the refusal. A
// root that is a plain directory, not empty, says "not empty" too: there
// the mount table has no line for it, which is a FAIL.
func r3() fileCheck {
	renameRoot := viaRoot("rename-root", c.Rename)
	removeRoot := viaRoot("rmdir-root", c.Rmdir)
	mk := func(name string, op c.Op, target string, z zone, verified []c.ErrClass) part {
		return withControl(part{name: name, op: op, target: target, tz: z, tkind: c.File, expect: expBusy,
			mount: atRoot, verified: verified})
	}
	return fileCheck{
		id: "R3", title: "a root itself cannot be replaced", control: "a root's mount point cannot be moved or removed",
		class: c.Destructive, rootIn: []string{"vm"},
		parts: []part{
			mk("mv-ro", renameRoot, "ro-file", zRO, expRenameMount),
			mk("rmdir-ro", removeRoot, "ro-file", zRO, expRmdirMount),
			mk("mv-dev", renameRoot, "parent-file", zDev, expRenameMount),
			mk("rmdir-dev", removeRoot, "parent-file", zDev, expRmdirMount),
		},
	}
}

func r4() fileCheck {
	mountOp := func(name string, op c.Op, verified []c.ErrClass) part {
		return withControl(part{name: name, op: op, target: "wr-keep", tz: zPart, tkind: c.Dir, expect: expBusy,
			mount: atPart, verified: verified})
	}
	return fileCheck{
		id: "R4", title: "a writable part's parent stays read-only", control: "a writable part's parent is read-only",
		class: c.Destructive, rootIn: []string{"vm"},
		parts: []part{
			dev("mkdir", c.OpMkdirIn, "sibling", c.Dir),
			dev("symlink", c.OpSymlinkIn, "sibling", c.Dir),
			dev("append", c.OpAppend, "parent-file", c.File),
			dev("rm", c.OpRemove, "rm-parent-file", c.File),
			dev("rename", c.OpMoveIn, "mv-parent-tree", c.Tree),
			mountOp("mv-part", viaPart("rename-part", c.Rename), expRenameMount),
			mountOp("rmdir-part", viaPart("rmdir-part", c.Rmdir), expRmdirMount),
		},
	}
}

func r6() fileCheck {
	through := func(name, target string, z zone) part {
		return part{name: name, op: c.OpAppendThrough, target: target, tz: z, tkind: c.File, allowed: c.File, scratch: true, expect: expErofs}
	}
	return fileCheck{
		id: "R6", title: "a symlink is not a way out", control: "a symbolic link does not unprotect its target",
		class: c.Write, rootIn: []string{"vm"},
		parts: []part{
			through("via-parent", "parent-file", zDev),
			through("via-ro", "ro-file", zRO),
		},
	}
}

// watchRoles are the host canaries no mount reaches.
var watchRoles = []string{"outside", "daemon", "grants", "ckpt", "cfg", "cred"}

// leaveTheMounts tries to read and write the watched host canaries by
// paths that go up out of the mounts. Not reaching them (no such file) is
// the expected answer; success is a failure.
func leaveTheMounts(ctx context.Context, env c.Env) (fails, errs, lines []string) {
	ro, err := env.Canary("ro-file")
	if err != nil {
		return nil, []string{"canary ro-file is missing: " + err.Error()}, nil
	}
	sp, err := c.SandboxPath(ro)
	if err != nil {
		return nil, []string{err.Error()}, nil
	}
	root, err := sp.Root()
	if err != nil {
		return nil, []string{err.Error()}, nil
	}
	for _, role := range watchRoles {
		cn, err := env.Canary(role)
		if err != nil {
			continue // reported by the watch
		}
		for _, form := range []string{c.ViaRootDotDot, c.ViaWorkDotDot, c.ViaProcRoot} {
			for _, mk := range []struct {
				name string
				f    func(c.SPath, string, string) (c.Cmd, error)
			}{{"read", c.ReadHost}, {"write", c.AppendHost}} {
				cmd, err := mk.f(root, cn.Rel, form)
				if err != nil {
					errs = append(errs, err.Error())
					continue
				}
				res, err := env.RunCmd(ctx, env.SessionUser(), cmd)
				tag := role + "/" + form + "/" + mk.name
				switch {
				case err != nil:
					errs = append(errs, tag+" could not be run: "+err.Error())
				case res.Class == c.ClassOK:
					fails = append(fails, tag+": the sandbox reached the host canary")
					lines = append(lines, tag+": REACHED")
				case res.RC < 0 || res.Class == c.ClassOther:
					errs = append(errs, tag+" ended other: the shell may not have run")
				default:
					lines = append(lines, tag+": "+string(res.Class))
				}
			}
		}
	}
	return
}

func r7() fileCheck {
	return fileCheck{
		id: "R7", title: "traversal does not reach outside", control: "a path with .. or /proc/self/root does not unprotect a canary",
		class: c.Write, rootIn: []string{"vm"},
		watch: watchRoles,
		extra: leaveTheMounts,
		parts: []part{
			ro("dotdot-ro", c.OpAppendDotDot, "ro-file", c.File),
			dev("dotdot-dev", c.OpAppendDotDot, "parent-file", c.File),
			ro("proc-ro", c.OpAppendProc, "ro-file", c.File),
			dev("proc-dev", c.OpAppendProc, "parent-file", c.File),
		},
	}
}

func r8() fileCheck {
	return fileCheck{
		id: "R8", title: "deletion outside the writable parts is refused", control: "rm -r outside the writable parts is refused",
		class: c.Destructive, rootIn: []string{"vm"},
		parts: []part{
			ro("rm-ro-tree", c.OpRemoveTree, "del-ro-tree", c.Tree),
			dev("rm-sibling", c.OpRemoveTree, "del-sibling", c.Dir),
			dev("rm-parent", c.OpRemoveTree, "del-parent-file", c.File),
		},
	}
}

// r10 makes the mount calls themselves, through the suite's probe, and
// not through a mount binary, which an image may not have: a missing
// binary proves nothing. Without CAP_SYS_ADMIN, which no user of a
// container or gVisor sandbox holds (gVisor's root included: its
// capabilities are the container's), the kernel refuses each with EPERM
// before it looks at anything else; any other answer is not that
// refusal, and a call that succeeds is a FAIL. The mount points' lines of
// the mount table are read before and after each call too, so a call
// that is said refused and still changed one is a FAIL. The unmounts
// come last, since what is unmounted stays so for the checks after.
// Under vm the guest's root holds CAP_SYS_ADMIN by design: R9 is its
// check.
func r10() fileCheck {
	call := func(name, op, target string, z zone, at mountAt) part {
		return withControl(part{name: name, probeOp: op, target: target, tz: z, tkind: c.File, expect: expPerm, mount: at})
	}
	return fileCheck{
		id: "R10", title: "no capability to remount", control: "the sandbox cannot remount or unmount",
		class: c.Destructive, isolations: []string{"container", "gvisor"}, rootIn: []string{"gvisor"},
		extra: r10UserNS,
		parts: []part{
			call("bind-rw-ro", probe.MountBindRW, "ro-file", zRO, atRoot),
			call("rw-ro", probe.MountRW, "ro-file", zRO, atRoot),
			call("bind-ro-ro", probe.MountBindRO, "ro-file", zRO, atRoot),
			call("bind-rw-dev", probe.MountBindRW, "parent-file", zDev, atRoot),
			call("rw-dev", probe.MountRW, "parent-file", zDev, atRoot),
			call("bind-rw-part", probe.MountBindRW, "um-parent-file", zDev, atPart),
			call("detach-ro", probe.UmountDetach, "ro-file", zRO, atRoot),
			call("umount-part", probe.Umount, "um-parent-file", zDev, atPart),
			call("detach-part", probe.UmountDetach, "um-parent-file", zDev, atPart),
		},
	}
}

func r11() fileCheck {
	return fileCheck{
		id: "R11", title: "a nested writable part is served correctly", control: "the writable part is writable and its parent is not",
		class: c.Write, isolations: []string{"vm"}, rootIn: []string{"vm"},
		parts: []part{
			withControl(dev("parent", c.OpAppend, "nest-parent-file", c.File)),
		},
	}
}

func r12() fileCheck {
	man := func(name string, op c.Op, target string, k c.Kind) part {
		return part{name: name, op: op, target: target, tz: zManaged, tkind: k, allowed: k, expect: expErofs}
	}
	return fileCheck{
		id: "R12", title: "the managed mount is read-only", control: "the managed directory is read-only",
		class: c.Destructive, rootIn: []string{"vm"},
		parts: []part{
			man("append", c.OpAppend, "managed", c.File),
			man("truncate", c.OpTruncate, "managed-trunc", c.File),
			man("rm", c.OpRemove, "managed-rm", c.File),
			man("rename", c.OpMoveIn, "managed-mv", c.Tree),
		},
	}
}

// fileChecks are the checks made of refusals, in the order they run
// after R5.
func fileChecks() []fileCheck {
	return []fileCheck{r1(), r6(), r7(), r11(), r2(), r3(), r4(), r8(), r10(), r12()}
}
