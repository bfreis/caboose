package checks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

// What the checks on mount points read of the sandbox's mount table, and
// the probe's mount calls.

// mountState reads the mount table as user and describes the mount point
// at of the target and every mount below it. bad when the mount point is
// not mounted at all, or more than once (something covers it).
func mountState(at mountAt) func(context.Context, c.Env, string, c.SPath) (string, string, error) {
	return func(ctx context.Context, env c.Env, user string, target c.SPath) (string, string, error) {
		mp, err := at.of(target)
		if err != nil {
			return "", "", err
		}
		res, err := env.RunCmd(ctx, user, c.MountTable())
		switch {
		case err != nil:
			return "", "", fmt.Errorf("the mount table could not be read: %v", err)
		case res.RC != 0:
			return "", "", fmt.Errorf("the mount table could not be read: %s%s", res.Class, msgOf(res))
		}
		es, err := c.ParseMountinfo(strings.NewReader(res.Out))
		if err != nil {
			return "", "", fmt.Errorf("the mount table could not be read: %v", err)
		}
		var lines []string
		n := 0
		for _, e := range es {
			switch {
			case e.Target == mp.String():
				n++
			case strings.HasPrefix(e.Target, mp.String()+"/"):
			default:
				continue
			}
			lines = append(lines, fmt.Sprintf("%d %d %s %s %s %s %s %s", e.ID, e.Parent, e.Root, e.Target, strings.Join(e.Options, ","), e.FSType, e.Source, strings.Join(e.SuperOptions, ",")))
		}
		switch {
		case n == 0:
			return "", mp.String() + " is not a mount point in the sandbox", nil
		case n > 1:
			return "", fmt.Sprintf("%s is mounted %d times in the sandbox", mp, n), nil
		}
		return strings.Join(lines, "\n"), "", nil
	}
}

// probeMountOp is the probe's mount call op on the mount point at of the
// target.
func probeMountOp(nonce, op string, at mountAt) c.Op {
	return c.Op{Name: op, Build: func(p, _ c.SPath) (c.Cmd, error) {
		mp, err := at.of(p)
		if err != nil {
			return c.Cmd{}, err
		}
		return c.ProbeMount(nonce, op, mp)
	}}
}

// probeReady is the run's nonce when the suite's probe is in the
// writable part as the suite placed it: the sandbox can write there, and
// a probe it had replaced would answer for itself. Without the probe the
// mount calls are not made at all: no mount binary stands in for it.
func probeReady(env c.Env) (string, error) {
	nonce, err := env.Nonce()
	if err != nil {
		return "", err
	}
	rel, err := c.ProbeRel(nonce[:8])
	if err != nil {
		return "", err
	}
	want, err := env.ProbeHash()
	if err != nil {
		return "", fmt.Errorf("the mount calls are made only by the suite's probe, which is not placed: %v", err)
	}
	id, err := env.Identity(rel)
	if err != nil || !id.Mode.IsRegular() || id.Mode.Perm()&0o100 == 0 {
		return "", fmt.Errorf("the suite's probe is not in place in the writable part (%v)", err)
	}
	if id.Hash != want {
		return "", errors.New("the probe in the writable part is not the one the suite placed")
	}
	return nonce, nil
}

// r10UserNS is R10's extra: after the single mount calls, it enters a
// user namespace through the probe and tries to make the read-only root
// writable, to unmount the part, and to write a read-only canary from
// inside it. A container's seccomp refuses the namespace; gVisor allows
// it, and the mount is still locked read-only, so every call and the
// write must be refused. The probe reports held; the suite also reads the
// witness canary on the host, so a write that landed is a FAIL whatever
// the probe said.
func r10UserNS(ctx context.Context, env c.Env) (fails, errs, lines []string) {
	nonce, err := probeReady(env)
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	users, err := sessionUsers(env, []string{"gvisor"})
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	roCn, err := env.Canary("ro-file")
	if err != nil {
		return nil, []string{"userns: canary ro-file is missing: " + err.Error()}, nil
	}
	witness, err := c.SandboxPath(roCn)
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	roRoot, err := witness.Root()
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	partCn, err := env.Canary("um-parent-file")
	if err != nil {
		return nil, []string{"userns: canary um-parent-file is missing: " + err.Error()}, nil
	}
	partSP, err := c.SandboxPath(partCn)
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	part, err := atPart.of(partSP)
	if err != nil {
		return nil, []string{"userns: " + err.Error()}, nil
	}
	for _, u := range users {
		tag := u + "/userns"
		cmd, err := c.ProbeUserNS(nonce, roRoot, part, witness)
		if err != nil {
			errs = append(errs, tag+": "+err.Error())
			continue
		}
		before, err := env.Snapshot(roCn)
		if err != nil {
			errs = append(errs, tag+": the witness could not be read: "+err.Error())
			continue
		}
		// The sandbox's own mount table of the root and the part, before and
		// after: a namespaced unmount or remount that reached the sandbox's
		// mounts is a FAIL whatever the probe says.
		roBefore, roBad, roErr := mountState(atRoot)(ctx, env, u, witness)
		partBefore, partBad, partErr := mountState(atPart)(ctx, env, u, partSP)
		if roErr != nil || partErr != nil || roBad != "" || partBad != "" {
			errs = append(errs, tag+": the mount table before the attempt: "+strings.Join(nonEmpty(errText(roErr), roBad, errText(partErr), partBad), "; "))
			continue
		}
		res, runErr := env.RunCmd(ctx, u, cmd)
		after, snapErr := env.Snapshot(roCn)
		if snapErr != nil {
			errs = append(errs, tag+": the witness could not be read back: "+snapErr.Error())
			continue
		}
		if len(changedPaths(before, after)) > 0 {
			fails = append(fails, tag+": a write landed on the read-only canary")
			lines = append(lines, tag+": FAIL the witness changed on the host")
			continue
		}
		roAfter, roBad, roErr := mountState(atRoot)(ctx, env, u, witness)
		partAfter, partBad, partErr := mountState(atPart)(ctx, env, u, partSP)
		switch {
		case roErr != nil || partErr != nil:
			errs = append(errs, tag+": the mount table after the attempt: "+strings.Join(nonEmpty(errText(roErr), errText(partErr)), "; "))
			continue
		case roBad != "" || partBad != "" || roAfter != roBefore || partAfter != partBefore:
			fails = append(fails, tag+": the sandbox's mounts changed: "+strings.Join(nonEmpty(roBad, partBad), "; "))
			lines = append(lines, tag+": FAIL the mount table changed")
			continue
		}
		switch {
		case runErr != nil:
			errs = append(errs, tag+": the attempt could not be run: "+runErr.Error())
		case res.RC == 0:
			fails = append(fails, tag+": a namespaced remount, unmount or write succeeded")
			lines = append(lines, tag+": FAIL "+probeSteps(res))
		case res.RC == probe.ExitRefused && env.Isolation() == "gvisor" && !strings.Contains(res.Out, "namespace: ok"):
			// gVisor runs without the engine's seccomp, so the namespace is
			// expected to be made there: a refusal of it (or an EPERM from
			// the id maps or the exec, which look the same) tried nothing.
			errs = append(errs, tag+": the namespace was not made under gVisor, so nothing was tried: "+probeSteps(res))
		case res.RC == probe.ExitRefused:
			lines = append(lines, tag+": held ("+probeSteps(res)+")")
		default:
			errs = append(errs, tag+": the probe exited "+fmt.Sprint(res.RC)+", nothing proven: "+probeSteps(res))
		}
	}
	return fails, errs, lines
}

// probeSteps is the probe's step lines and its verdict, each without the
// probe's prefix, printable and bounded, for a results line.
func probeSteps(res c.ExecResult) string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(res.Out, "\n"), "\n") {
		l = strings.TrimPrefix(l, "conformance-probe: ")
		if len(l) > 160 {
			l = l[:160]
		}
		out = append(out, l)
		if len(out) == 8 {
			break
		}
	}
	return strings.Join(out, "; ")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// probeStillPlaced re-reads the probe's hash after a check used it: the
// sandbox can write the part, so a probe it swapped mid-check would
// answer for itself. nonce is the run's.
func probeStillPlaced(env c.Env, nonce string) error {
	rel, err := c.ProbeRel(nonce[:8])
	if err != nil {
		return err
	}
	want, err := env.ProbeHash()
	if err != nil {
		return fmt.Errorf("the probe's hash could not be read back: %v", err)
	}
	id, err := env.Identity(rel)
	if err != nil || !id.Mode.IsRegular() {
		return fmt.Errorf("the probe is not in place after the check (%v)", err)
	}
	if id.Hash != want {
		return errors.New("the probe in the writable part was replaced during the check")
	}
	return nil
}
