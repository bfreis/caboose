package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	c "github.com/bfreis/caboose/internal/conformance"
)

func errResult(format string, args ...any) c.CheckResult {
	return c.CheckResult{Status: c.Error, Reason: fmt.Sprintf(format, args...)}
}

// R5: the writable part is writable. It is the positive control the other
// checks carry each in its own way, and a check of its own: create, write,
// rename and remove inside the part, as every user.

type step5 struct {
	name string
	kind c.Kind
	ops  []c.Op
}

func steps5() []step5 {
	return []step5{
		{"file", c.File, []c.Op{c.OpAppend}},
		{"mkdir", c.Dir, []c.Op{c.OpMkdirIn}},
		{"symlink", c.Dir, []c.Op{c.OpSymlinkIn}},
		{"rename", c.Tree, []c.Op{c.OpMoveIn, removeMoved}},
	}
}

func role5(s step5, user string) string { return "w5-" + s.name + "-" + user[:1] }

func r5() c.Check {
	var roles []string
	for _, s := range steps5() {
		for _, u := range allUsers {
			roles = append(roles, role5(s, u))
		}
	}
	return c.Check{
		ID: "R5", Title: "the writable part is writable", Control: "the writable part is writable",
		Step: stepName, Group: groupFiles, Class: c.Destructive, Roles: roles,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			if st := env.State(); st != c.StateRunning {
				return errResult("the sandbox is not running (%s)", st)
			}
			users, err := sessionUsers(env, []string{"vm"})
			if err != nil {
				return errResult("%v", err)
			}
			lines := []string{"ran as: " + strings.Join(users, ", ")}
			for _, u := range users {
				for _, s := range steps5() {
					cn, err := env.Canary(role5(s, u))
					if err != nil {
						return errResult("canary %s is missing: %v", role5(s, u), err)
					}
					sp, err := c.SandboxPath(cn)
					if err != nil {
						return errResult("%v", err)
					}
					for _, op := range s.ops {
						before, err := env.Snapshot(cn)
						if err != nil || len(before) == 0 {
							return errResult("canary %s is not on the host", cn.Role)
						}
						cmd, err := op.Build(sp, c.SPath{})
						if err != nil {
							return errResult("%v", err)
						}
						res, err := env.RunCmd(ctx, u, cmd)
						if err != nil {
							return errResult("%s/%s could not be run: %v", u, op.Name, err)
						}
						if res.Class != c.ClassOK {
							return errResult("%s/%s in the writable part ended %s", u, op.Name, res.Class)
						}
						after, err := env.Snapshot(cn)
						if err != nil {
							return errResult("canary %s could not be read back: %v", cn.Role, err)
						}
						if len(changedPaths(before, after)) == 0 {
							return errResult("%s/%s was reported done but the canary did not change on the host", u, op.Name)
						}
					}
					lines = append(lines, u+"/"+s.name+": PASS")
				}
			}
			return c.CheckResult{Status: c.Pass, Lines: lines}
		},
	}
}

// R9: a rootful guest cannot write through a read-only mount. As root it
// unmounts what it can, then writes and deletes beneath; the verdict is
// the host's files, never the guest's mounts.

func r9() c.Check {
	roles := []string{"um-ro", "um-parent", "um-wr", "um-scratch"}
	return c.Check{
		ID: "R9", Title: "a rootful guest cannot write through a read-only mount", Control: "the host refuses writes to a read-only share",
		Step: stepName, Group: groupFiles, Class: c.Destructive, Isolations: []string{"vm"}, Roles: roles,
		Run: runR9,
	}
}

func runR9(ctx context.Context, env c.Env) c.CheckResult {
	if st := env.State(); st != c.StateRunning {
		return errResult("the sandbox is not running (%s)", st)
	}
	cn := map[string]c.Canary{}
	sp := map[string]c.SPath{}
	snap := map[string]map[string]c.Identity{}
	for _, role := range []string{"um-ro", "um-parent", "um-wr", "um-scratch"} {
		x, err := env.Canary(role)
		if err != nil {
			return errResult("canary %s is missing: %v", role, err)
		}
		p, err := c.SandboxPath(x)
		if err != nil {
			return errResult("%v", err)
		}
		s, err := env.Snapshot(x)
		if err != nil || len(s) == 0 {
			return errResult("canary %s is not on the host", role)
		}
		cn[role], sp[role], snap[role] = x, p, s
	}
	lines := []string{"ran as: root"}

	// The positive control: root writes in the writable part.
	cmd, err := c.Append(sp["um-wr"])
	if err != nil {
		return errResult("%v", err)
	}
	res, err := env.RunCmd(ctx, "root", cmd)
	if err != nil || res.Class != c.ClassOK {
		return errResult("positive control failed: a write in the writable part ended %s", classOf(res, err))
	}
	if after, err := env.Snapshot(cn["um-wr"]); err != nil || len(changedPaths(snap["um-wr"], after)) == 0 {
		return errResult("positive control failed: the writable canary did not change on the host")
	}

	part, err := sp["um-scratch"].MountPoint()
	if err != nil {
		return errResult("%v", err)
	}
	roRoot, err := sp["um-ro"].Root()
	if err != nil {
		return errResult("%v", err)
	}
	devRoot, err := sp["um-parent"].Root()
	if err != nil {
		return errResult("%v", err)
	}
	type attempt struct {
		name string
		mk   func() (c.Cmd, error)
	}
	run := func(a attempt) (c.ExecResult, error) {
		cmd, err := a.mk()
		if err != nil {
			return c.ExecResult{}, err
		}
		return env.RunCmd(ctx, "root", cmd)
	}
	// The write and delete attempts run before the unmount as well as
	// after it, so that a successful unmount cannot send the later writes
	// to the guest's own filesystem and leave nothing tested.
	attempts := []attempt{
		{"append beside the part", func() (c.Cmd, error) { return c.Append(sp["um-parent"]) }},
		{"append in the read-only root", func() (c.Cmd, error) { return c.Append(sp["um-ro"]) }},
		{"remount the part", func() (c.Cmd, error) { return c.RemountRW(part) }},
		{"remount the read-only root", func() (c.Cmd, error) { return c.RemountRW(roRoot) }},
		{"mount the share again, read-write", func() (c.Cmd, error) { return c.MountVirtiofs(roRoot) }},
		{"bind over the read-only root", func() (c.Cmd, error) { return c.BindOver(roRoot) }},
		{"delete beside the part", func() (c.Cmd, error) { return c.Remove(sp["um-parent"]) }},
		{"delete in the read-only root", func() (c.Cmd, error) { return c.Remove(sp["um-ro"]) }},
	}
	runAttempts := func(when string) error {
		for _, a := range attempts {
			res, err := run(a)
			if err != nil {
				return fmt.Errorf("%s could not be run: %v", a.name, err)
			}
			lines = append(lines, when+" "+a.name+": "+string(res.Class))
		}
		return nil
	}
	if err := runAttempts("before unmount:"); err != nil {
		return errResult("%v", err)
	}
	unmounted := false
	for _, a := range []attempt{
		{"umount part", func() (c.Cmd, error) { return c.Umount(part) }},
		{"umount read-only root", func() (c.Cmd, error) { return c.Umount(roRoot) }},
		{"umount parent root", func() (c.Cmd, error) { return c.Umount(devRoot) }},
	} {
		res, err := run(a)
		if err != nil {
			return errResult("%s could not be run: %v", a.name, err)
		}
		if res.Class == c.ClassOK {
			unmounted = true
		}
	}
	if unmounted {
		lines = append(lines, "unmount: possible")
	} else {
		lines = append(lines, "unmount: not possible")
	}
	if err := runAttempts("after unmount:"); err != nil {
		return errResult("%v", err)
	}
	var fails []string
	for _, role := range []string{"um-ro", "um-parent"} {
		after, err := env.Snapshot(cn[role])
		if err != nil {
			return errResult("canary %s could not be read back: %v", role, err)
		}
		if len(changedPaths(snap[role], after)) > 0 {
			fails = append(fails, fmt.Sprintf("canary %s changed on the host", role))
		}
	}
	return verdict(fails, nil, lines)
}

func classOf(r c.ExecResult, err error) string {
	if err != nil {
		return "an error"
	}
	return string(r.Class)
}

// R13: the roots listed in the container's labels are the roots mounted,
// each with the access its label gives. There is no sandbox command: the
// labels and mounts are the engine's.

func r13() c.Check {
	return c.Check{
		ID: "R13", Title: "the roots listed are the roots mounted", Control: "the container's labels describe its mounts",
		Step: stepName, Group: groupFiles, Class: c.Observe,
		Run: runR13,
	}
}

func runR13(_ context.Context, env c.Env) c.CheckResult {
	if st := env.State(); st != c.StateRunning {
		return errResult("the sandbox is not running (%s)", st)
	}
	rootOf := func(role string) (string, error) {
		cn, err := env.Canary(role)
		if err != nil {
			return "", err
		}
		p, err := c.SandboxPath(cn)
		if err != nil {
			return "", err
		}
		r, err := p.Root()
		return r.String(), err
	}
	roRoot, err := rootOf("ro-file")
	if err != nil {
		return errResult("canary ro-file is missing: %v", err)
	}
	devRoot, err := rootOf("parent-file")
	if err != nil {
		return errResult("canary parent-file is missing: %v", err)
	}
	labels := env.Labels()
	if len(labels) == 0 {
		return errResult("the container's labels could not be read")
	}
	mounts := env.Mounts()
	if len(mounts) == 0 {
		return errResult("the container's mounts could not be read")
	}
	var fails []string
	lines := []string{"configured: " + roRoot + " read-only; " + devRoot + " read-only with the part " + devRoot + "/part writable"}

	var roots []string
	if err := json.Unmarshal([]byte(labels[assets.LabelRoots]), &roots); err != nil {
		fails = append(fails, "the roots label is missing or unreadable")
	} else {
		slices.Sort(roots)
		if want := []string{devRoot, roRoot}; !slices.Equal(roots, want) {
			fails = append(fails, fmt.Sprintf("the roots label lists %s, the configuration %s", strings.Join(roots, ","), strings.Join(want, ",")))
		}
	}
	var writable map[string][]string
	if err := json.Unmarshal([]byte(labels[assets.LabelWritable]), &writable); err != nil {
		fails = append(fails, "the writable label is missing or unreadable")
	} else {
		if len(writable) != 2 || len(writable[roRoot]) != 0 || !slices.Equal(writable[devRoot], []string{"part"}) {
			fails = append(fails, "the writable label differs from the configuration")
		}
	}

	byTarget := map[string]c.Mount{}
	for _, m := range mounts {
		byTarget[m.Target] = m
	}
	need := []struct {
		target   string
		readOnly bool
	}{{roRoot, true}, {devRoot, true}, {devRoot + "/part", false}}
	known := map[string]bool{}
	for _, n := range need {
		known[n.target] = true
		m, ok := byTarget[n.target]
		switch {
		case !ok:
			fails = append(fails, n.target+" is not mounted")
		case m.ReadOnly != n.readOnly:
			fails = append(fails, fmt.Sprintf("%s is mounted %s, the configuration says %s", n.target, access(m.ReadOnly), access(n.readOnly)))
		}
	}
	for _, m := range mounts {
		if known[m.Target] {
			continue
		}
		for _, r := range []string{roRoot, devRoot} {
			if strings.HasPrefix(m.Target, r+"/") {
				fails = append(fails, m.Target+" is mounted under a root and is not in the configuration")
			}
		}
	}
	return verdict(fails, nil, lines)
}

func access(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "writable"
}

// R14: the wipe. After a fresh restart, and last of every check of the
// run, the sandbox's root runs one rm -rf of everything of the suite's it
// sees (Env.Wipe: the two roots and the managed canaries, by literal
// path, behind the gate). Then, from the host and never from what the
// sandbox said, every canary outside the writable part must be as it was:
// in the read-only root, in the writable root beside its part, in the
// managed directory, and in every place no mount reaches. The writable
// part's canaries going is the positive control: the wipe ran, as a user
// who could remove them. rm's own exit status is no verdict, since the
// read-only roots refuse it by design.

// wipeTimeout bounds the wipe's run in the sandbox.
const wipeTimeout = 120 * time.Second

func r14() c.Check {
	return c.Check{
		ID: "R14", Title: "a recursive removal as root of the suite's roots reaches no host file outside the writable part",
		Control: "no host file outside the writable parts is destroyed",
		Step:    stepName, Group: groupFiles, Class: c.Destructive, Wipe: true, Fresh: true,
		Run: runWipe,
	}
}

// wipeZone is where a canary is, as the wipe's verdict names it.
func wipeZone(cn c.Canary) string {
	if c.InWritablePart(cn) {
		return "writable part"
	}
	p, err := c.SandboxPath(cn)
	switch {
	case err != nil:
		return "outside every mount"
	case strings.HasPrefix(p.String(), "/etc/claude-code/"):
		return "managed directory"
	case strings.Contains(p.String(), "-ro/"):
		return "read-only root"
	}
	return "writable root, outside its part"
}

// recordDiff compares a canary's paths, as now is (its snapshot), with
// the host's record of each: the paths that differ, and whether the
// record says every one was removed by a check it was handed to.
func recordDiff(env c.Env, cn c.Canary, now map[string]c.Identity) (removed bool, diff []string, err error) {
	removed = true
	for _, p := range cn.Paths {
		id, gone, err := env.Recorded(p)
		if err != nil {
			return false, nil, err
		}
		removed = removed && gone
		a, there := now[p]
		if there == gone || there && !c.SameIdentity(a, id) {
			diff = append(diff, p)
		}
	}
	return removed, diff, nil
}

func runWipe(ctx context.Context, env c.Env) c.CheckResult {
	cmd, err := env.WipeCommand()
	if err != nil {
		return errResult("the wipe cannot be built: %v", err)
	}
	cans, err := env.Canaries()
	if err != nil {
		return errResult("the host's canaries cannot be listed: %v", err)
	}
	type held struct {
		cn   c.Canary
		zone string
		snap map[string]c.Identity
	}
	var kept []held
	var early []string
	zones := map[string]int{}
	for _, cn := range cans {
		snap, err := env.Snapshot(cn)
		if err != nil {
			return errResult("canary %s could not be read: %v", cn.Role, err)
		}
		z := wipeZone(cn)
		if z == "writable part" {
			if len(snap) == 0 {
				continue // removed in the part, where that is allowed
			}
		} else {
			// Every canary outside the part is held to the host's record
			// of it, which only a post-check of a check it was handed to
			// moves: not to what it is now, which a restart or anything an
			// earlier check left running may already have changed.
			removed, d, err := recordDiff(env, cn, snap)
			switch {
			case err != nil:
				return errResult("canary %s has no record: %v", cn.Role, err)
			case len(d) > 0:
				early = append(early, fmt.Sprintf("canary %s (%s) changed on the host since the last check, before the wipe", cn.Role, z))
				continue
			case removed:
				continue // removed by a check it was handed to
			}
		}
		zones[z]++
		kept = append(kept, held{cn, z, snap})
	}
	if len(early) > 0 {
		// Something changed the host's files outside every check: the wipe
		// is not run on top of it.
		return verdict(early, nil, []string{"the wipe was not run"})
	}
	for _, z := range []string{"writable part", "read-only root", "writable root, outside its part", "outside every mount"} {
		if zones[z] == 0 {
			return errResult("the run has no canary in the %s: the wipe would prove nothing", z)
		}
	}
	lines := []string{"ran as: " + c.WipeUser, "command: " + cmd.Body()}
	if u := env.SessionUser(); u != "" {
		lines = append(lines, "the sandbox's sessions run as: "+u)
	}
	var fails, errs []string
	wctx, cancel := context.WithTimeout(ctx, wipeTimeout)
	res, xerr := env.Wipe(wctx)
	timedOut := wctx.Err() != nil && ctx.Err() == nil
	cancel()
	switch {
	case xerr != nil && (timedOut || errors.Is(xerr, context.DeadlineExceeded)):
		// Only the exec's client is stopped: rm may still be running in
		// the sandbox while the host compares, and the teardown removes
		// the sandbox after.
		errs = append(errs, fmt.Sprintf("the wipe did not end within %s; the command may still be running in the sandbox, so the host's comparison below may be early", wipeTimeout))
	case xerr != nil:
		errs = append(errs, "the wipe could not be run: "+xerr.Error())
	case res.RC < 0:
		errs = append(errs, "the wipe ended other: the shell may not have run ("+res.Msg+")")
	default:
		l := fmt.Sprintf("rm exited %d", res.RC)
		if res.Msg != "" {
			l += ": " + res.Msg
		}
		lines = append(lines, l)
	}
	// The host's side, whatever the sandbox said or did not say.
	gone, protected := 0, 0
	var left []string
	for _, h := range kept {
		after, err := env.Snapshot(h.cn)
		if err != nil {
			errs = append(errs, fmt.Sprintf("canary %s could not be read back: %v", h.cn.Role, err))
			continue
		}
		if h.zone == "writable part" {
			if len(after) == 0 {
				gone++
			} else {
				left = append(left, h.cn.Role)
			}
			continue
		}
		protected++
		_, rd, err := recordDiff(env, h.cn, after)
		if err != nil {
			errs = append(errs, fmt.Sprintf("canary %s has no record: %v", h.cn.Role, err))
			continue
		}
		switch d := append(changedPaths(h.snap, after), rd...); {
		case len(after) == 0:
			fails = append(fails, fmt.Sprintf("canary %s (%s) is gone from the host", h.cn.Role, h.zone))
			lines = append(lines, h.cn.Role+": GONE")
		case len(d) > 0:
			fails = append(fails, fmt.Sprintf("canary %s (%s) changed on the host", h.cn.Role, h.zone))
			lines = append(lines, fmt.Sprintf("%s: CHANGED (%d paths)", h.cn.Role, len(d)))
		}
	}
	if xerr == nil && res.RC >= 0 && len(left) > 0 {
		slices.Sort(left)
		errs = append(errs, fmt.Sprintf("the wipe left %d canaries of the writable part (%s first): it did not run, or not as a user who could remove them", len(left), left[0]))
	}
	summary := fmt.Sprintf("writable part: %d canaries gone, as expected; outside it: %d canaries as they were", gone, protected)
	lines = append(lines, summary)
	v := verdict(fails, errs, lines)
	if v.Status == c.Pass {
		v.Reason = fmt.Sprintf("rm -rf of the suite's roots ran as %s; %s; the sandbox is destroyed by design, and teardown removes it", c.WipeUser, summary)
	}
	return v
}
