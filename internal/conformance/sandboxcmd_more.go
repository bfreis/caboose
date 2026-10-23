package conformance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// More builders, on the same terms as the ones in sandboxcmd.go: literal
// canary paths (or the mount point of the root or part one is in) and
// nothing else.

var rootRe = regexp.MustCompile(`^/work/conf-[0-9a-f]{8}-(ro|dev)`)

// Root is the root's mount point that p is in.
func (p SPath) Root() (SPath, error) {
	if !p.valid() {
		return SPath{}, errors.New("conformance: not a path the suite may name")
	}
	r := rootRe.FindString(p.p)
	if r == "" {
		return SPath{}, fmt.Errorf("conformance: %s is not in a root", p.p)
	}
	return SPath{r}, nil
}

func Rmdir(p SPath) (Cmd, error) { return build(true, "rmdir %s", p) }

// Rename renames p to a sibling name.
func Rename(p SPath) (Cmd, error) {
	if !p.valid() {
		return Cmd{}, errors.New("conformance: a command needs canary paths made by the suite")
	}
	return Cmd{body: fmt.Sprintf("mv %s %s.moved", p.p, p.p), destructive: true}, nil
}

// BindOver mounts the sandbox's /tmp over the mount point mp.
func BindOver(mp SPath) (Cmd, error) { return build(true, "mount --bind /tmp %s", mp) }

// AppendDotDot appends to p by a path that leaves its root by ".." and
// comes back in.
func AppendDotDot(p SPath) (Cmd, error) {
	r, err := p.Root()
	if err != nil {
		return Cmd{}, err
	}
	rest := strings.TrimPrefix(p.p, r.p)
	name := strings.TrimPrefix(r.p, "/work/")
	return Cmd{body: fmt.Sprintf("echo x >> %s/../%s%s", r.p, name, rest)}, nil
}

// AppendProc appends to p by way of /proc/self/root.
func AppendProc(p SPath) (Cmd, error) {
	if !p.valid() {
		return Cmd{}, errors.New("conformance: a command needs canary paths made by the suite")
	}
	return Cmd{body: "echo x >> /proc/self/root" + p.p}, nil
}

var (
	OpRmdir        = opOf("rmdir", Rmdir)
	OpRename       = opOf("rename", Rename)
	OpAppendDotDot = opOf("append-dotdot", AppendDotDot)
	OpAppendProc   = opOf("append-proc", AppendProc)
)

var hostRelRe = regexp.MustCompile(`^(canary|home)(/[a-z0-9.-]+)*/conf-[0-9a-f]{8}-[a-z0-9-]+$`)

// Ways to name a host canary from inside the sandbox, by leaving the
// mounts upward.
const (
	ViaRootDotDot = "root-dotdot" // <root>/../<rel>
	ViaWorkDotDot = "work-dotdot" // /work/../<rel>
	ViaProcRoot   = "proc-root"   // /proc/self/root/../<rel>
)

func hostVia(root SPath, rel, form string) (string, error) {
	if !root.valid() || root.p != rootRe.FindString(root.p) || !hostRelRe.MatchString(rel) {
		return "", errors.New("conformance: not a root and a canary path of the suite")
	}
	switch form {
	case ViaRootDotDot:
		return root.p + "/../" + rel, nil
	case ViaWorkDotDot:
		return "/work/../" + rel, nil
	case ViaProcRoot:
		return "/proc/self/root/../" + rel, nil
	}
	return "", fmt.Errorf("conformance: unknown form %q", form)
}

// ReadHost reads the host canary at rel (relative to B) by leaving the
// mounts through root.
func ReadHost(root SPath, rel, form string) (Cmd, error) {
	p, err := hostVia(root, rel, form)
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: "cat " + p}, nil
}

// AppendHost appends to the host canary at rel the same way.
func AppendHost(root SPath, rel, form string) (Cmd, error) {
	p, err := hostVia(root, rel, form)
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: "echo x >> " + p}, nil
}

// MountVirtiofs mounts the share of mp's root again, read-write.
func MountVirtiofs(mp SPath) (Cmd, error) {
	if !mp.valid() {
		return Cmd{}, errors.New("conformance: a command needs canary paths made by the suite")
	}
	tag := strings.TrimPrefix(mp.p, "/work/")
	tag = strings.ReplaceAll(tag, "/", "-")
	return Cmd{body: fmt.Sprintf("mount -t virtiofs %s %s", tag, mp.p), destructive: true}, nil
}
