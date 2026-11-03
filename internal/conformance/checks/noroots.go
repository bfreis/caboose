package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	c "github.com/bfreis/caboose/internal/conformance"
)

// The check of a sandbox made with no roots, N1. A sandbox has its mounts
// for life, and every other check of the suite needs the two roots, so it
// is a run of its own (caboose conformance run --no-roots): the same
// envelope, the same canaries on the host, and an empty [roots]. It only
// reads: the verdict that nothing changed is the runner's post-check.

const (
	groupNoRoots = "no roots"
	stepNoRoots  = "a sandbox with no roots"

	// workDir is where a root is mounted unless config.toml says otherwise.
	workDir = "/work"
	// managedMount is the mount that is there whatever the roots: the
	// positive control of the mount table.
	managedMount = "/etc/claude-code"
	// roleManaged is the canary the positive control of the read reads.
	roleManaged = "managed"
)

// noRootsRoles are canaries in each place a root would show them: the
// read-only root, the other root and its writable part.
var noRootsRoles = []string{"um-ro", "um-parent", "um-wr"}

// NoRootsChecks are the checks of a run with no roots.
func NoRootsChecks() []c.Check { return []c.Check{n1()} }

func n1() c.Check {
	return c.Check{
		ID: "N1", Title: "a sandbox with no roots mounts no host directory as a project", Control: "no roots, no project mount",
		Step: stepNoRoots, Group: groupNoRoots, Class: c.Observe,
		Run: runN1,
	}
}

func runN1(ctx context.Context, env c.Env) c.CheckResult {
	if st := env.State(); st != c.StateRunning {
		return errResult("the sandbox is not running (%s)", st)
	}
	user := env.SessionUser()
	if user == "" {
		return errResult("%v", env.SessionUserError())
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

	// The canaries are on the host, so that not reading them says something.
	n8 := ""
	paths := map[string]c.SPath{}
	for _, role := range noRootsRoles {
		cn, err := env.Canary(role)
		if err != nil {
			return errResult("canary %s is missing: %v", role, err)
		}
		if s, err := env.Snapshot(cn); err != nil || len(s) == 0 {
			return errResult("canary %s is not on the host", role)
		}
		m := n8Of.FindStringSubmatch(cn.Rel)
		if m == nil {
			return errResult("canary %s has no nonce in its name", role)
		}
		n8 = m[1]
		if paths[role], err = c.SandboxPath(cn); err != nil {
			return errResult("%v", err)
		}
	}

	// What the labels say.
	var roots []string
	if err := json.Unmarshal([]byte(labels[assets.LabelRoots]), &roots); err != nil || roots == nil {
		fails = append(fails, "the roots label is missing or is no list")
	} else if len(roots) != 0 {
		fails = append(fails, "the roots label lists "+strings.Join(roots, ", "))
	}
	var writable map[string][]string
	if err := json.Unmarshal([]byte(labels[assets.LabelWritable]), &writable); err != nil || writable == nil {
		fails = append(fails, "the writable label is missing or is no table")
	} else if len(writable) != 0 {
		fails = append(fails, "the writable label names a root")
	}

	// What it was created with: nothing at /work or below it, whatever
	// the root's name, and nothing from the roots the suite laid out, at
	// whatever path.
	hostRoots := "/roots/conf-" + n8 + "-"
	for _, m := range mounts {
		switch {
		case m.Target == workDir || strings.HasPrefix(m.Target, workDir+"/"):
			fails = append(fails, m.Target+" is mounted")
		case strings.Contains(m.Source, hostRoots):
			fails = append(fails, m.Target+" is mounted from a root's directory")
		}
	}

	// What the sandbox itself shows.
	res, err := env.RunCmd(ctx, user, c.MountTable())
	switch {
	case err != nil:
		return errResult("the mount table could not be read: %v", err)
	case res.RC != 0:
		return errResult("the mount table could not be read: %s%s", res.Class, msgOf(res))
	}
	entries, err := c.ParseMountinfo(strings.NewReader(res.Out))
	if err != nil {
		return errResult("the mount table could not be read: %v", err)
	}
	sawManaged := false
	for _, e := range entries {
		switch {
		case e.Target == managedMount:
			sawManaged = true
		case e.Target == workDir || strings.HasPrefix(e.Target, workDir+"/"):
			fails = append(fails, e.Target+" is in the sandbox's mount table")
		}
	}
	if !sawManaged {
		return errResult("positive control failed: the sandbox's mount table does not list %s", managedMount)
	}

	// A read: the same command reads what is mounted, and finds nothing
	// where a canary would be.
	manCn, err := env.Canary(roleManaged)
	if err != nil {
		return errResult("canary %s is missing: %v", roleManaged, err)
	}
	manPath, err := c.SandboxPath(manCn)
	if err != nil {
		return errResult("%v", err)
	}
	cmd, err := c.Read(manPath)
	if err != nil {
		return errResult("%v", err)
	}
	if res, err = env.RunCmd(ctx, user, cmd); err != nil || res.Class != c.ClassOK {
		return errResult("positive control failed: a read of the managed canary ended %s", classOf(res, err))
	}
	for _, role := range noRootsRoles {
		cmd, err := c.Read(paths[role])
		if err != nil {
			return errResult("%v", err)
		}
		res, err := env.RunCmd(ctx, user, cmd)
		switch {
		case err != nil:
			return errResult("a read at canary %s's place could not be run: %v", role, err)
		case res.RC < 0 || res.RC == exitUnknown:
			return errResult("a read at canary %s's place did not run%s", role, msgOf(res))
		case res.Class == c.ClassEnoent:
		case res.Class == c.ClassOK:
			fails = append(fails, fmt.Sprintf("canary %s can be read in the sandbox", role))
		default:
			fails = append(fails, fmt.Sprintf("canary %s's place in the sandbox answers %s, not that nothing is there%s", role, res.Class, msgOf(res)))
		}
	}

	// And nothing at all in /work, whatever its name. A /work that is not
	// there holds nothing either.
	res, err = env.RunCmd(ctx, user, c.ListWork())
	switch {
	case err != nil:
		return errResult("%s could not be listed: %v", workDir, err)
	case res.RC == 0 && res.Msg == "":
	case res.RC == 0:
		fails = append(fails, fmt.Sprintf("%s holds %q and more", workDir, res.Msg))
	case res.Class != c.ClassEnoent:
		return errResult("%s could not be listed: %s%s", workDir, res.Class, msgOf(res))
	}
	return verdict(fails, nil, []string{"configured: an empty [roots]"})
}
