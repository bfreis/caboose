package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Op is the operation a refusal check attempts. Build makes its command
// for a canary's path p (and a scratch directory's, for the operations
// that need one).
type Op struct {
	Name  string
	Build func(p, scratch SPath) (Cmd, error)
}

// One-path operations.
func opOf(name string, f func(SPath) (Cmd, error)) Op {
	return Op{Name: name, Build: func(p, _ SPath) (Cmd, error) { return f(p) }}
}

var (
	OpAppend     = opOf("append", Append)
	OpTruncate   = opOf("truncate", Truncate)
	OpTouch      = opOf("touch", Touch)
	OpChmod      = opOf("chmod", Chmod)
	OpRemove     = opOf("remove", Remove)
	OpRemoveTree = opOf("remove-tree", RemoveTree)
	OpMkdirIn    = opOf("mkdir-in", MkdirIn)
	OpSymlinkIn  = opOf("symlink-in", SymlinkIn)
	OpHardlinkIn = opOf("hardlink-in", HardlinkIn)
	OpMoveIn     = opOf("move-in", MoveIn)
	// OpAppendThrough appends to p through a symbolic link made in the
	// scratch directory.
	OpAppendThrough = Op{Name: "append-through-symlink", Build: AppendThrough}
	// OpUmountThenAppend unmounts the writable part the scratch directory is
	// in, then appends to p.
	OpUmountThenAppend = Op{Name: "umount-then-append", Build: func(p, scratch SPath) (Cmd, error) {
		mp, err := scratch.MountPoint()
		if err != nil {
			return Cmd{}, err
		}
		return UmountThenAppend(mp, p)
	}}
)

// Refusal describes a check that a sandbox operation on a protected canary
// is refused: a positive control first (the same operation on a canary
// where it is allowed must work), then the attempt, judged from the host.
type Refusal struct {
	ID, Title, Control, Step, Group string
	Isolations, Trusts              []string
	// Class is Write or Destructive.
	Class Class
	// User is the sandbox user: "agent" or "root", or "" for the one the
	// sandbox's sessions run as (Env.SessionUser).
	User string
	// Target is the role of the protected canary, Allowed that of the
	// canary on which the same operation is allowed, Scratch (optional) a
	// writable directory canary for operations that need one.
	Target, Allowed, Scratch string
	Attempt                  Op
	// ControlOp, when set, is the operation of the positive control on the
	// allowed canary, for an attempt that is refused everywhere (an
	// unmount). Left nil, the control is the attempt itself.
	ControlOp *Op
	// Expect are the classes that count as the refusal. enoent, other and
	// ok are never accepted.
	Expect []ErrClass
	// State, when set, reads what the attempt aims at as the sandbox sees
	// it (a mount point: its line of the mount table): state describes
	// it, bad says why it is not what it must be (no mount there), err
	// that it could not be read. It is read before the attempt and after
	// it: bad either time, or a state that changed, is a FAIL, whatever
	// the attempt said, and an attempt is judged only once both reads
	// held.
	State func(ctx context.Context, env Env, user string, target SPath) (state, bad string, err error)
	// ExpectRC, when set, is the exit status the attempt must end with for
	// its refusal to count: a check that runs the suite's probe sets it to
	// the probe's refused status, so a refusal message with any other
	// status (a killed probe, a gate or usage exit) is not taken as the
	// refusal.
	ExpectRC *int
	// Verified are classes that count as the refusal only with a State
	// that held: an isolation's own order of checks can refuse a mount
	// point as not empty, or with ENOENT, before it finds it busy, which
	// proves nothing alone, since a plain directory that is not empty
	// says the same, but proves the refusal once the mount is shown to be
	// there, the same, before and after. other and ok are never accepted.
	Verified []ErrClass
}

// Validate reports why r cannot be a check.
func (r Refusal) Validate() error {
	switch {
	case r.ID == "" || r.Target == "" || r.Allowed == "" || r.Target == r.Allowed:
		return errors.New("a refusal check needs an id, a protected canary and a different allowed one")
	case r.Attempt.Build == nil:
		return errors.New("no operation")
	case r.Class != Write && r.Class != Destructive:
		return errors.New("a refusal check is a write or destructive check")
	case len(r.Expect) == 0:
		return errors.New("no expected class")
	}
	for _, c := range r.Expect {
		if !refusalClass(c) {
			return fmt.Errorf("%s is not a class that can stand for a refusal", c)
		}
	}
	if len(r.Verified) > 0 && r.State == nil {
		return errors.New("classes that need a verified state, and no state to verify")
	}
	for _, c := range r.Verified {
		if !refusalClass(c) && c != ClassEnoent {
			return fmt.Errorf("%s is not a class that can stand for a refusal", c)
		}
	}
	return nil
}

// Check builds the check.
func (r Refusal) Check() Check {
	roles := []string{r.Target, r.Allowed}
	if r.Scratch != "" {
		roles = append(roles, r.Scratch)
	}
	return Check{
		ID: r.ID, Title: r.Title, Control: r.Control, Step: r.Step, Group: r.Group,
		Isolations: r.Isolations, Trusts: r.Trusts, Class: r.Class, Roles: roles,
		Run: r.run,
	}
}

func errorf(format string, args ...any) CheckResult {
	return CheckResult{Status: Error, Reason: fmt.Sprintf(format, args...)}
}

func (r Refusal) run(ctx context.Context, env Env) CheckResult {
	if err := r.Validate(); err != nil {
		return errorf("%v", err)
	}
	if st := env.State(); st != StateRunning {
		return errorf("the sandbox is not running (%s)", st)
	}
	if r.User == "" {
		r.User = env.SessionUser()
	}
	target, err := env.Canary(r.Target)
	if err != nil {
		return errorf("canary %s is missing: %v", r.Target, err)
	}
	allowed, err := env.Canary(r.Allowed)
	if err != nil {
		return errorf("canary %s is missing: %v", r.Allowed, err)
	}
	var scratch Canary
	var sScratch SPath
	if r.Scratch != "" {
		if scratch, err = env.Canary(r.Scratch); err != nil {
			return errorf("canary %s is missing: %v", r.Scratch, err)
		}
		if sScratch, err = SandboxPath(scratch); err != nil {
			return errorf("%v", err)
		}
	}
	sTarget, err := SandboxPath(target)
	if err != nil {
		return errorf("%v", err)
	}
	sAllowed, err := SandboxPath(allowed)
	if err != nil {
		return errorf("%v", err)
	}
	snap := func(c Canary) (map[string]Identity, error) {
		s, err := env.Snapshot(c)
		if err == nil && len(s) == 0 {
			err = fmt.Errorf("canary %s is not on the host", c.Role)
		}
		return s, err
	}
	allowedBefore, err := snap(allowed)
	if err != nil {
		return errorf("%v", err)
	}
	targetBefore, err := snap(target)
	if err != nil {
		return errorf("%v", err)
	}

	// The positive control.
	ctrl := r.Attempt
	if r.ControlOp != nil {
		ctrl = *r.ControlOp
	}
	pc, err := ctrl.Build(sAllowed, sScratch)
	if err != nil {
		return errorf("%v", err)
	}
	res, err := env.RunCmd(ctx, r.User, pc)
	if err != nil {
		return errorf("positive control failed: %v", err)
	}
	if res.Class != ClassOK {
		return errorf("positive control failed: %s%s", res.Class, msgOf(res))
	}
	allowedAfter, err := env.Snapshot(allowed)
	if err != nil {
		return errorf("positive control failed: %v", err)
	}
	if len(diffTrees(allowedBefore, allowedAfter)) == 0 {
		return errorf("positive control failed: the allowed canary did not change on the host")
	}

	// The attempt, judged from the host.
	at, err := r.Attempt.Build(sTarget, sScratch)
	if err != nil {
		return errorf("%v", err)
	}
	var before string
	if r.State != nil {
		s, bad, err := r.State(ctx, env, r.User, sTarget)
		switch {
		case err != nil:
			return errorf("before the attempt: %v", err)
		case bad != "":
			return CheckResult{Status: Fail, Reason: "before the attempt: " + bad}
		}
		before = s
	}
	res, runErr := env.RunCmd(ctx, r.User, at)
	targetAfter, snapErr := env.Snapshot(target)
	if snapErr != nil {
		return errorf("the protected canary could not be read back: %v", snapErr)
	}
	if changed := diffTrees(targetBefore, targetAfter); len(changed) > 0 {
		return CheckResult{
			Status: Fail,
			Reason: fmt.Sprintf("the protected canary changed on the host (the sandbox saw %s); %d path(s)", classOrErr(res, runErr), len(changed)),
			Lines:  []string{"look at: " + strings.Join(describe(changed), ", ")},
		}
	}
	if runErr != nil {
		return errorf("the attempt could not be run: %v", runErr)
	}
	verified := false
	if r.State != nil {
		s, bad, err := r.State(ctx, env, r.User, sTarget)
		switch {
		case err != nil:
			return errorf("after the attempt (which ended %s%s): %v", res.Class, msgOf(res), err)
		case bad != "":
			return CheckResult{Status: Fail, Reason: fmt.Sprintf("after the attempt (which ended %s): %s", res.Class, bad)}
		case s != before:
			return CheckResult{Status: Fail, Reason: fmt.Sprintf("the mount changed in the sandbox (the attempt ended %s)", res.Class),
				Lines: []string{"before: " + oneLine(before), "after: " + oneLine(s)}}
		}
		verified = true
	}
	if r.ExpectRC != nil && res.RC != *r.ExpectRC && res.Class != ClassOK {
		return errorf("the attempt exited %d, not the refused status %d (it said %s%s)", res.RC, *r.ExpectRC, res.Class, msgOf(res))
	}
	switch {
	case res.Class == ClassOK:
		return CheckResult{Status: Fail, Reason: "the sandbox reported success"}
	case res.Class == ClassOther:
		return errorf("the attempt ended %s%s, which is not a refusal", res.Class, msgOf(res))
	case slices.Contains(r.Expect, res.Class):
		return CheckResult{Status: Pass}
	case verified && slices.Contains(r.Verified, res.Class):
		return CheckResult{Status: Pass, Lines: []string{fmt.Sprintf("refused as %s%s, with the mount there and unchanged before and after", res.Class, msgOf(res))}}
	case res.Class == ClassEnoent:
		return errorf("the attempt ended %s%s, which is not a refusal", res.Class, msgOf(res))
	}
	return CheckResult{Status: Fail, Reason: fmt.Sprintf("refused as %s, expected %s%s", res.Class, joinClasses(append(slices.Clone(r.Expect), r.Verified...)), msgOf(res))}
}

// oneLine joins a state's lines for a results line, printable and
// bounded: the sandbox wrote its mount table.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

func msgOf(r ExecResult) string {
	if r.Msg == "" {
		return ""
	}
	return ": " + r.Msg
}

func classOrErr(r ExecResult, err error) string {
	if err != nil {
		return "an error"
	}
	return string(r.Class)
}

func joinClasses(cs []ErrClass) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, " or ")
}

// describe shows paths as the results file does, under <B>.
func describe(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "<B>/" + p
	}
	return out
}
