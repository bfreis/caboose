// Package checks holds the conformance checks of the file protections:
// read-only roots, the writable parts inside them, the symlink and
// traversal limits, the managed directory and the labels that describe
// the mounts. Each check is a conformance.Check; All returns them in the
// order they run.
package checks

import (
	"context"
	"fmt"
	"slices"
	"strings"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

const (
	groupFiles = "files"
	stepName   = "read-only roots with writable parts"
)

// zone says where a canary lives, which fixes its parent directory.
type zone int

const (
	zRO      zone = iota // a root with no writable part
	zDev                 // the root that holds the writable part, beside it
	zPart                // inside the writable part
	zManaged             // the managed directory
)

// part is one operation of a check on one protected canary. The same
// operation is first tried on a canary of its own in the writable part
// (the positive control), which is made for each user.
type part struct {
	name string
	op   c.Op
	// target is the protected canary, of kind tkind in zone tz.
	target string
	tz     zone
	tkind  c.Kind
	// allowed is the kind of the canary the control works on.
	allowed c.Kind
	// scratch asks for a writable directory canary for the operation.
	scratch bool
	// ctrl, when set, is the control's operation instead of op.
	ctrl   *c.Op
	expect []c.ErrClass
	// mount, when set, is the mount point the operation aims at (the
	// target's root, or the writable part): its line of the sandbox's
	// mount table is read before the attempt and after it, and must be
	// there and unchanged (c.Refusal.State). verified are the classes
	// that count as the refusal only then.
	mount    mountAt
	verified []c.ErrClass
	// probeOp, when set, makes the attempt the probe's mount call of
	// that name on the mount point, in place of op; the probe's refused
	// status is then required (c.Refusal.ExpectRC).
	probeOp string
}

// mountAt is which mount point a part's operation aims at.
type mountAt int

const (
	atNone mountAt = iota
	atRoot         // the root the target is in
	atPart         // the writable part (of the root the target is in)
)

// of is the mount point for the target canary's path p.
func (m mountAt) of(p c.SPath) (c.SPath, error) {
	switch m {
	case atRoot:
		return p.Root()
	case atPart:
		if mp, err := p.MountPoint(); err == nil {
			return mp, nil
		}
		r, err := p.Root()
		if err != nil {
			return c.SPath{}, err
		}
		return r.Child("part")
	}
	return c.SPath{}, fmt.Errorf("checks: no mount point for %s", p)
}

// fileCheck is a check made of refusals: every part, as every user.
type fileCheck struct {
	id, title, control string
	class              c.Class
	isolations         []string
	// rootIn are the isolations in which the check also tries as root.
	rootIn []string
	// watch are canaries of other roles (outside the mounts) that must
	// stay as they are; those not made are noted and skipped.
	watch []string
	// extra runs after the parts and adds to the failures and errors.
	extra func(ctx context.Context, env c.Env) (fails, errs, lines []string)
	parts []part
}

var (
	expErofs = []c.ErrClass{c.ClassErofs}
	expBusy  = []c.ErrClass{c.ClassEbusy, c.ClassErofs}
	expPerm  = []c.ErrClass{c.ClassEperm}
	// expRenameMount and expRmdirMount count as a mount point's refusal
	// only once its mount is shown there, unchanged, before and after
	// (part.verified): gVisor refuses a rename of the root that holds the
	// part with ENOENT (its overlay copies up what is below before the
	// busy check) and an rmdir of a non-empty mount point as not empty,
	// each before it is found busy; which one is the operation's, so a
	// rename never accepts "not empty" nor an rmdir "no such file".
	expRenameMount = []c.ErrClass{c.ClassEnoent}
	expRmdirMount  = []c.ErrClass{c.ClassEnotempty}
)

var allUsers = []string{"agent", "root"}

func (fc fileCheck) users(env c.Env) ([]string, error) { return sessionUsers(env, fc.rootIn) }

// sessionUsers are the users a check made of refusals tries as: the one
// the sandbox's sessions run as, and root as well under the isolations
// in rootIn. Where the sessions run as root (vm, and gVisor on an engine
// where the agent cannot write its mounts) the agent user is no one: the
// mounts are root's, so its positive controls could not pass, and what it
// could not do root is tried for. None, with the reason, when the
// sandbox's label does not say who its sessions are.
func sessionUsers(env c.Env, rootIn []string) ([]string, error) {
	u := env.SessionUser()
	if u == "" {
		return nil, env.SessionUserError()
	}
	if u != "root" && slices.Contains(rootIn, env.Isolation()) {
		return []string{u, "root"}, nil
	}
	return []string{u}, nil
}

func (fc fileCheck) allowedRole(p part, user string) string {
	return "w-" + strings.ToLower(fc.id) + "-" + p.name + "-" + user[:1]
}

func (fc fileCheck) scratchRole(p part, user string) string {
	return "s-" + strings.ToLower(fc.id) + "-" + p.name + "-" + user[:1]
}

func (fc fileCheck) roles() []string {
	var out []string
	for _, p := range fc.parts {
		out = append(out, p.target)
		for _, u := range allUsers {
			out = append(out, fc.allowedRole(p, u))
			if p.scratch {
				out = append(out, fc.scratchRole(p, u))
			}
		}
	}
	return dedupe(append(out, fc.watch...))
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func (fc fileCheck) check() c.Check {
	return c.Check{
		ID: fc.id, Title: fc.title, Control: fc.control, Step: stepName, Group: groupFiles,
		Isolations: fc.isolations, Class: fc.class, Roles: fc.roles(), Run: fc.run,
	}
}

func (fc fileCheck) run(ctx context.Context, env c.Env) c.CheckResult {
	users, err := fc.users(env)
	if err != nil {
		return c.CheckResult{Status: c.Error, Reason: err.Error()}
	}
	lines := []string{"ran as: " + strings.Join(users, ", ")}
	type watched struct {
		role string
		cn   c.Canary
		snap map[string]c.Identity
	}
	var ws []watched
	for _, role := range fc.watch {
		cn, err := env.Canary(role)
		if err != nil {
			return c.CheckResult{Status: c.Error, Reason: fmt.Sprintf("canary %s is missing: %v", role, err), Lines: lines}
		}
		s, err := env.Snapshot(cn)
		if err != nil {
			return c.CheckResult{Status: c.Error, Reason: fmt.Sprintf("canary %s could not be read: %v", role, err), Lines: lines}
		}
		ws = append(ws, watched{role, cn, s})
	}
	var nonce string
	if slices.ContainsFunc(fc.parts, func(p part) bool { return p.probeOp != "" }) {
		n, err := probeReady(env)
		if err != nil {
			return c.CheckResult{Status: c.Error, Reason: err.Error(), Lines: lines}
		}
		nonce = n
	}
	var fails, errs []string
	for _, u := range users {
		for _, p := range fc.parts {
			attempt := p.op
			if p.probeOp != "" {
				attempt = probeMountOp(nonce, p.probeOp, p.mount)
			}
			r := c.Refusal{
				ID: fc.id, Title: fc.title, Control: fc.control, Step: stepName, Group: groupFiles,
				Class: fc.class, User: u, Target: p.target, Allowed: fc.allowedRole(p, u),
				Attempt: attempt, ControlOp: p.ctrl, Expect: p.expect, Verified: p.verified,
			}
			if p.mount != atNone {
				r.State = mountState(p.mount)
			}
			if p.probeOp != "" {
				rc := probe.ExitRefused
				r.ExpectRC = &rc
			}
			if p.scratch {
				r.Scratch = fc.scratchRole(p, u)
			}
			tag := u + "/" + p.name
			res := r.Check().Run(ctx, env)
			switch res.Status {
			case c.Pass:
				lines = append(lines, tag+": PASS")
				for _, l := range res.Lines {
					lines = append(lines, tag+": "+l)
				}
			case c.Error:
				errs = append(errs, tag+": "+res.Reason)
				lines = append(lines, tag+": ERROR "+res.Reason)
			case c.Fail:
				fails = append(fails, tag+": "+res.Reason)
				lines = append(lines, tag+": FAIL "+res.Reason)
				for _, l := range res.Lines {
					lines = append(lines, tag+": "+l)
				}
			default:
				errs = append(errs, tag+": "+res.Reason)
				lines = append(lines, tag+": "+string(res.Status)+" "+res.Reason)
			}
		}
	}
	if fc.extra != nil {
		f, e, l := fc.extra(ctx, env)
		fails, errs, lines = append(fails, f...), append(errs, e...), append(lines, l...)
	}
	if nonce != "" {
		if err := probeStillPlaced(env, nonce); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, w := range ws {
		after, err := env.Snapshot(w.cn)
		if err != nil {
			errs = append(errs, fmt.Sprintf("canary %s could not be read back: %v", w.role, err))
		} else if d := changedPaths(w.snap, after); len(d) > 0 {
			fails = append(fails, fmt.Sprintf("canary %s changed on the host", w.role))
		}
	}
	return verdict(fails, errs, lines)
}

// verdict: a canary that changed is a FAIL even when something else went
// wrong; a control that failed, or anything that could not be run, is an
// ERROR; otherwise PASS.
func verdict(fails, errs, lines []string) c.CheckResult {
	switch {
	case len(fails) > 0:
		r := fails[0]
		if len(fails) > 1 {
			r += fmt.Sprintf(" (and %d more)", len(fails)-1)
		}
		return c.CheckResult{Status: c.Fail, Reason: r, Lines: lines}
	case len(errs) > 0:
		r := errs[0]
		if len(errs) > 1 {
			r += fmt.Sprintf(" (and %d more)", len(errs)-1)
		}
		return c.CheckResult{Status: c.Error, Reason: r, Lines: lines}
	}
	return c.CheckResult{Status: c.Pass, Lines: lines}
}

func sameID(a, b c.Identity) bool {
	if a.Mode.IsDir() && b.Mode.IsDir() {
		return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode
	}
	return a == b
}

// changedPaths lists the paths that differ between two snapshots of one
// canary: changed, gone or new.
func changedPaths(before, after map[string]c.Identity) []string {
	var out []string
	for p, b := range before {
		if a, ok := after[p]; !ok || !sameID(a, b) {
			out = append(out, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// Operations on the root or the writable part that a canary is in.

func viaRoot(name string, f func(c.SPath) (c.Cmd, error)) c.Op {
	return c.Op{Name: name, Build: func(p, _ c.SPath) (c.Cmd, error) {
		r, err := p.Root()
		if err != nil {
			return c.Cmd{}, err
		}
		return f(r)
	}}
}

func viaPart(name string, f func(c.SPath) (c.Cmd, error)) c.Op {
	return c.Op{Name: name, Build: func(p, _ c.SPath) (c.Cmd, error) {
		m, err := p.MountPoint()
		if err != nil {
			return c.Cmd{}, err
		}
		return f(m)
	}}
}

// removeMoved removes what OpMoveIn moved.
var removeMoved = c.Op{Name: "remove-moved", Build: func(p, _ c.SPath) (c.Cmd, error) {
	m, err := p.Child("moved")
	if err != nil {
		return c.Cmd{}, err
	}
	return c.Remove(m)
}}

var appendOp = c.OpAppend
