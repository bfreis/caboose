package fake

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

// PlaceProbe places the suite's probe in the writable part, as the suite
// does, for the checks that make their calls through it.
func (f *Fake) PlaceProbe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.newNode(kFile, "probe")
	n.mode = 0o755
	f.nodes[f.Part()+"/"+conformance.ProbeName] = n
	f.probeSum = hashHex(n.content)
}

// Nonce is the run's nonce as the model's canaries hold it.
func (f *Fake) Nonce() string { return f.nonce }

// mountinfo is the sandbox's /proc/self/mountinfo: its own root, the two
// roots, the writable part (until an unmount took it) and the managed
// directory, read-only as the mutants leave them.
func (f *Fake) mountinfo() []string {
	opts := func(ro bool) string {
		if ro {
			return "ro,relatime"
		}
		return "rw,relatime"
	}
	out := []string{"1 0 0:1 / / rw,relatime - overlay overlay rw"}
	devID := 1
	for i, m := range f.mountsLocked() {
		id, parent := 20+i, 1
		rel := strings.TrimPrefix(m.Source, "<B>/")
		switch {
		case f.mutants[RootsNotMounts] && f.isRoot(rel):
			continue
		case rel == f.Part():
			if f.unmount {
				continue
			}
			parent = devID
		case rel == f.Dev():
			devID = id
		}
		out = append(out, fmt.Sprintf("%d %d 0:%d / %s %s - fakefs %s rw", id, parent, 40+i, m.Target, opts(m.ReadOnly), "fake:"+rel))
	}
	return out
}

// mountsLocked are the mounts as Mounts says them, with f.mu held.
func (f *Fake) mountsLocked() []conformance.Mount {
	return []conformance.Mount{
		{Source: "<B>/" + f.RO(), Target: "/work/conf-" + f.n8 + "-ro", ReadOnly: !f.mutants[RootsWritable]},
		{Source: "<B>/" + f.Dev(), Target: "/work/conf-" + f.n8 + "-dev", ReadOnly: !f.mutants[ParentWritable]},
		{Source: "<B>/" + f.Part(), Target: "/work/conf-" + f.n8 + "-dev/part"},
		{Source: "<B>/" + managedDir, Target: "/etc/claude-code", ReadOnly: !f.mutants[ManagedWritable]},
	}
}

// probeMount is the probe's mount and userns modes, as the real one
// answers in a sandbox without CAP_SYS_ADMIN. The last argument is
// "1>&2"; the rest are --flag value pairs.
func (f *Fake) probeMount(_ string, a []string, out *[]string) int {
	if len(a) < 1 || a[len(a)-1] != "1>&2" || (len(a)-1)%2 != 0 {
		*out = append(*out, "conformance-probe: usage")
		return probe.ExitUsage
	}
	flags := map[string]string{}
	for i := 0; i+1 < len(a)-1; i += 2 {
		flags[a[i]] = a[i+1]
	}
	if flags["--nonce"] != f.nonce {
		*out = append(*out, "conformance-probe: the gate refused: the nonce is not the run's")
		return probe.ExitGate
	}
	mps := []string{"/work/conf-" + f.n8 + "-ro", "/work/conf-" + f.n8 + "-dev", "/work/conf-" + f.n8 + "-dev/part"}
	if flags["--mount"] == probe.MountUserNS {
		return f.probeUserNS(flags, mps, out)
	}
	op, target := flags["--mount"], flags["--target"]
	if !slices.Contains(probe.MountOps(), op) || !slices.Contains(mps, target) || flags["--part"] != "" || flags["--witness"] != "" {
		*out = append(*out, "conformance-probe: usage")
		return probe.ExitUsage
	}
	if strings.HasPrefix(op, "umount") && target == mps[2] && f.mutants[UnmountWritesThrough] && !f.unmount {
		f.unmount = true
		return 0
	}
	*out = append(*out, fmt.Sprintf("conformance-probe: %s %s: %s", op, target, ePerm))
	return probe.ExitRefused
}

// probeUserNS models the user-namespace attempt. Under a container the
// namespace is refused (the engine's seccomp blocks unshare), which is
// held. Under gVisor the namespace is allowed but the mount is locked
// read-only, so the remount, the unmount and the witness write are all
// refused, which is held. Under the mutant UsernsRemountLands the remount
// succeeds and the write lands on the read-only canary (its node
// changes), which the suite catches on the host.
func (f *Fake) probeUserNS(flags map[string]string, mps []string, out *[]string) int {
	target, part, witness := flags["--target"], flags["--part"], flags["--witness"]
	if target != mps[0] || part != mps[2] || !strings.HasPrefix(witness, target+"/") {
		*out = append(*out, "conformance-probe: usage")
		return probe.ExitUsage
	}
	if f.isolate == "container" || f.usernsRefused {
		*out = append(*out, "conformance-probe: namespace: "+ePerm, "conformance-probe: userns: held")
		return probe.ExitRefused
	}
	*out = append(*out, "conformance-probe: namespace: ok")
	if f.mutants[UsernsUnmountReachesOut] {
		f.unmount = true
	}
	if f.mutants[UsernsRemountLands] {
		rel, ok := f.hostRel(witness)
		if ok && f.nodes[rel] != nil {
			f.nodes[rel].content += "x\n"
			f.touchNode(f.nodes[rel])
			f.logMut("append", rel)
		}
		*out = append(*out, "conformance-probe: remount-bind-rw "+target+": ok",
			"conformance-probe: witness "+witness+": wrote",
			"conformance-probe: userns: a call or the write succeeded")
		return 0
	}
	*out = append(*out, "conformance-probe: remount-bind-rw "+target+": "+ePerm,
		"conformance-probe: umount "+part+": "+ePerm,
		"conformance-probe: witness "+witness+": "+eRofs,
		"conformance-probe: userns: held")
	return probe.ExitRefused
}

// RefuseUserNS makes the probe's user namespace refused under any
// isolation: under gVisor, where it is expected to be made, the suite has
// then tried nothing.
func (f *Fake) RefuseUserNS() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usernsRefused = true
}
