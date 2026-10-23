package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/conformance"
)

var (
	registryMu sync.Mutex
	registered = map[Mutant]bool{}
)

type nodeKind int

const (
	kFile nodeKind = iota
	kDir
	kLink
)

type node struct {
	kind    nodeKind
	content string // a file's contents, a link's target
	mode    fs.FileMode
	ino     uint64
	nlink   uint64
	mtime   int64
}

// Fake is a sandbox, a host, a clock and a network, over one in-memory
// filesystem. Paths in it are the host's, relative to B (roots/..,
// canary/..); the sandbox sees roots/<name> as /work/<name>.
type Fake struct {
	mu     sync.Mutex
	nonce  string
	n8     string
	nodes  map[string]*node
	canary map[string]conformance.Canary // by role
	hidden map[string]bool               // roles whose canary is missing
	ino    uint64
	tick   int64

	mutants map[Mutant]bool
	down    bool
	shellNo bool
	partBad bool // the writable part refuses too: a positive control fails
	stray   string
	unmount bool
	// usernsRefused refuses the user namespace under any isolation
	// (RefuseUserNS), as a gVisor that refused it would.
	usernsRefused bool
	events        []string
	muts          []conformance.Mutation
	isolate       string
	rootRun       bool    // under gVisor: the agent cannot write its mounts, so the sandbox runs as root
	userLbl       *string // LabelUser as SetUserLabel set it; nil is caboose's
	probeSum      string  // the sha256 of the probe as placed, "" for none
	restarts      int     // how many times Restart ran
	// rec is the host's record of each canary path, as the real guard
	// keeps it: as made, then as each post-check found the paths of the
	// check it was handed to.
	rec      map[string]record
	listener int
}

// New is a world with the two roots, the writable part and canary/, as
// the suite lays them out, under nonce n8 (eight hex characters).
func New(n8 string) *Fake {
	f := &Fake{
		nonce: n8 + "-fake-nonce", n8: n8, nodes: map[string]*node{}, canary: map[string]conformance.Canary{},
		hidden: map[string]bool{}, mutants: map[Mutant]bool{}, isolate: "container", rootRun: true,
		rec: map[string]record{},
	}
	for _, d := range []string{"roots", f.RO(), f.Dev(), f.Part(), "canary", "data", managedDir} {
		f.nodes[d] = f.newNode(kDir, "")
	}
	return f
}

// RO, Dev and Part are the host paths of the two roots and the writable part.
func (f *Fake) RO() string   { return "roots/conf-" + f.n8 + "-ro" }
func (f *Fake) Dev() string  { return "roots/conf-" + f.n8 + "-dev" }
func (f *Fake) Part() string { return f.Dev() + "/part" }

// SetIsolation sets the isolation the sandbox's labels report.
func (f *Fake) SetIsolation(s string) { f.isolate = s }

// SetRunsAsRoot says whether a gVisor sandbox runs as root, as caboose
// makes it on an engine where the agent cannot write its mounts (OrbStack's,
// for one): the model's default, since it is the stricter for the suite.
// false is an engine where the agent can. A vm sandbox always runs as root.
func (f *Fake) SetRunsAsRoot(b bool) { f.mu.Lock(); f.rootRun = b; f.mu.Unlock() }

// ProbeHash is the sha256 of the probe as the model placed it.
func (f *Fake) ProbeHash() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeSum
}

// AgentRuns is how many commands were run as the agent user in a sandbox
// that runs as root, which agentShut refuses: a run of the suite never
// sends one, so a test holds every model to none.
func (f *Fake) AgentRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e == "exec-as-agent" {
			n++
		}
	}
	return n
}

// NoAgentRuns fails t, at its end, when f ran a command as the agent in a
// sandbox that runs as root: there the model refuses whatever the agent
// does, which a refusal check could take for the control it tests.
func NoAgentRuns(t testing.TB, f *Fake) {
	t.Cleanup(func() {
		if n := f.AgentRuns(); n > 0 {
			t.Errorf("%d command(s) ran as the agent user in a sandbox that runs as root", n)
		}
	})
}

// SetUserLabel makes the sandbox's LabelUser v, as a sandbox caboose did
// not make could have it; drop leaves it out.
func (f *Fake) SetUserLabel(v string, drop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if drop {
		v = "\x00drop"
	}
	f.userLbl = &v
}

// labelUser sets LabelUser in labels as the sandbox has it.
func (f *Fake) labelUser(labels map[string]string) {
	switch {
	case f.userLbl == nil && f.runsAsRoot():
		labels[assets.LabelUser] = conformance.RootUser
	case f.userLbl == nil:
		labels[assets.LabelUser] = ""
	case *f.userLbl != "\x00drop":
		labels[assets.LabelUser] = *f.userLbl
	}
}

// runsAsRoot reports whether the sandbox runs as root: its LabelUser is
// conformance.RootUser, and the agent user can do nothing there that the
// suite needs.
func (f *Fake) runsAsRoot() bool { return f.isolate == "vm" || f.isolate == "gvisor" && f.rootRun }

// agentShut is what a command run as the agent user comes to in a sandbox
// that runs as root, ok false otherwise: the mounts and the link's socket
// are root's, so every write and every link command is refused. The
// model refuses whatever the agent runs there, and records it.
func (f *Fake) agentShut(user string) (rc int, stdout, stderr string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user == "root" || !f.runsAsRoot() || f.down {
		return 0, "", "", false
	}
	f.event("exec-as-agent")
	return 0, "\nrc=1\n", "sh: line 0: can't create /work: " + eAcces, true
}

func (f *Fake) newNode(k nodeKind, content string) *node {
	f.ino++
	f.tick++
	n := &node{kind: k, content: content, ino: f.ino, nlink: 1, mtime: f.tick}
	switch k {
	case kDir:
		n.mode = fs.ModeDir | 0o755
	case kLink:
		n.mode = fs.ModeSymlink | 0o777
	default:
		n.mode = 0o644
	}
	return n
}

// Break turns the mutant on; Fix turns every mutant off.
func (f *Fake) Break(m Mutant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m != Enforcing {
		f.mutants[m] = true
	}
}

func (f *Fake) Fix() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutants = map[Mutant]bool{}
}

// Active reports whether mutant m is on.
func (f *Fake) Active(m Mutant) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutants[m]
}

// BreakAll turns on every mutant the model acts on.
func (f *Fake) BreakAll() {
	for m := range interpretable {
		f.Break(m)
	}
}

// Faults of the setup, not of a control.
func (f *Fake) SetDown(b bool)         { f.mu.Lock(); f.down = b; f.mu.Unlock() }
func (f *Fake) SetShellMissing(b bool) { f.mu.Lock(); f.shellNo = b; f.mu.Unlock() }
func (f *Fake) SetPartReadOnly(b bool) { f.mu.Lock(); f.partBad = b; f.mu.Unlock() }
func (f *Fake) HideCanary(role string) { f.mu.Lock(); f.hidden[role] = true; f.mu.Unlock() }
func (f *Fake) InjectStray(rel string) { f.mu.Lock(); f.stray = rel; f.mu.Unlock() }
func (f *Fake) Events() []string       { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.events) }
func (f *Fake) event(s string)         { f.events = append(f.events, s) }
func (f *Fake) logMut(op, rel string) {
	f.muts = append(f.muts, conformance.Mutation{Op: op, Rel: rel})
}

// Sandbox side.

func (f *Fake) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return "exited"
	}
	return conformance.StateRunning
}

func (f *Fake) Labels() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ro, dev := "/work/conf-"+f.n8+"-ro", "/work/conf-"+f.n8+"-dev"
	part := `["part"]`
	if f.mutants[LabelsDrift] {
		part = `[]`
	}
	labels := map[string]string{
		"caboose.isolation":  f.isolate,
		assets.LabelRoots:    `["` + ro + `","` + dev + `"]`,
		assets.LabelWritable: `{"` + ro + `":[],"` + dev + `":` + part + `}`,
	}
	f.labelUser(labels)
	return labels
}

func (f *Fake) Mounts() []conformance.Mount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mountsLocked()
}

// Exec runs the wrapped command in the model.
func (f *Fake) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if rc, o, e, ok := f.agentShut(user); ok {
		return rc, o, e, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return -1, "", "", errors.New("the sandbox is not running")
	}
	if f.shellNo {
		return 127, "", "exec: the shell is not there: no such file or directory", nil
	}
	body, ok := conformance.Unwrap(argv)
	if !ok {
		return 2, "", "sh: not the suite's wrapper", nil
	}
	if f.stray != "" {
		if _, there := f.nodes[f.stray]; !there {
			f.nodes[f.stray] = f.newNode(kFile, "stray")
			f.logMut("stray", f.stray)
		}
	}
	if isDestructive(body) {
		f.event("exec-destructive")
	} else {
		f.event("exec")
	}
	var out []string
	rc := f.runList(user, body, &out, 0)
	return 0, fmt.Sprintf("\nrc=%d\n", rc), strings.Join(out, "\n"), nil
}

func isDestructive(body string) bool {
	for _, t := range tokens(body) {
		switch t {
		case "rm", "mv", "umount", "remount,rw", "rmdir", "--bind", "--mount":
			return true
		}
	}
	return false
}

// tokens splits a command on spaces, keeping single-quoted text together;
// && and >> are tokens of their own.
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	in, has := false, false
	flush := func() {
		if has {
			out = append(out, cur.String())
		}
		cur.Reset()
		has = false
	}
	for _, r := range s {
		switch {
		case r == '\'':
			in = !in
			has = true
		case r == ' ' && !in:
			flush()
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}

func (f *Fake) runList(user, body string, out *[]string, depth int) int {
	if depth > 3 {
		*out = append(*out, "sh: too deeply nested")
		return 2
	}
	var cur []string
	rc := 0
	toks := tokens(body)
	run := func() {
		if len(cur) > 0 {
			rc = f.runCmd(user, cur, out, depth)
		}
		cur = nil
	}
	for i := 0; i <= len(toks); i++ {
		if i == len(toks) {
			run()
			break
		}
		if toks[i] == "&&" {
			run()
			if rc != 0 {
				return rc
			}
			continue
		}
		cur = append(cur, toks[i])
	}
	return rc
}

const (
	eRofs    = "Read-only file system"
	eNoent   = "No such file or directory"
	eBusy    = "Device or resource busy"
	ePerm    = "Operation not permitted"
	eExist   = "File exists"
	eIsDir   = "Is a directory"
	eNotDir  = "Not a directory"
	eNotMt   = "not mounted"
	eNotEmpt = "Directory not empty"
	eAcces   = "Permission denied"
)

// managedDir is the managed directory the sandbox has at /etc/claude-code.
const managedDir = "data/claude-code"

// sandbox path to host path: /proc/self/root is the root itself, and ".."
// is resolved as the kernel does for a path without links.
func (f *Fake) hostRel(p string) (string, bool) {
	p = strings.TrimPrefix(p, "/proc/self/root")
	if strings.Contains(p, "//") {
		return "", false
	}
	if rest, ok := strings.CutPrefix(path.Clean(p), "/etc/claude-code/"); ok && rest != "" {
		return managedDir + "/" + rest, true
	}
	if f.mutants[OutsideReachable] {
		// The host's canary directories are reachable from the sandbox.
		for _, top := range []string{"canary", "home"} {
			if rest, ok := strings.CutPrefix(path.Clean(p), "/"+top+"/"); ok && rest != "" {
				return top + "/" + rest, true
			}
		}
	}
	rest, ok := strings.CutPrefix(path.Clean(p), "/work/")
	if !ok || rest == "" || rest == ".." || strings.HasPrefix(rest, "../") {
		return "", false
	}
	return "roots/" + rest, true
}

func sandboxPath(rel string) string {
	if rest, ok := strings.CutPrefix(rel, managedDir); ok {
		return "/etc/claude-code" + rest
	}
	return "/work/" + strings.TrimPrefix(rel, "roots/")
}

type zone int

const (
	zNone  zone = iota
	zMount      // a root or the part, as mount points
	zRO
	zParent
	zPart
	zManaged
	zOutside
)

func (f *Fake) zoneOf(rel string) zone {
	ps := strings.Split(rel, "/")
	if len(ps) >= 2 && ps[0] == "data" && ps[1] == "claude-code" {
		if len(ps) == 2 {
			return zMount
		}
		return zManaged
	}
	if ps[0] == "canary" || ps[0] == "home" {
		return zOutside
	}
	if len(ps) < 2 || ps[0] != "roots" {
		return zNone
	}
	switch ps[1] {
	case "conf-" + f.n8 + "-ro":
		if len(ps) == 2 {
			return zMount
		}
		return zRO
	case "conf-" + f.n8 + "-dev":
		switch {
		case len(ps) == 2:
			return zMount
		case ps[2] == "part" && len(ps) == 3:
			return zMount
		case ps[2] == "part":
			return zPart
		}
		return zParent
	}
	return zNone
}

func (f *Fake) writableZone(z zone) bool {
	switch z {
	case zOutside:
		return f.mutants[OutsideReachable]
	case zManaged:
		return f.mutants[ManagedWritable]
	case zPart:
		return !f.partBad && !f.mutants[PartReadOnly]
	case zRO:
		return f.mutants[RootsWritable]
	case zParent:
		return f.mutants[ParentWritable] || f.mutants[UnmountWritesThrough] && f.unmount
	}
	return false
}

// writeTarget follows symlinks from rel the way a write does and says
// whose policy applies: the link's place when SymlinkFollowed is on, else
// the final target's.
func (f *Fake) writeTarget(rel string) (final, policy string) {
	final, policy = rel, rel
	for i := 0; i < 8; i++ {
		n := f.nodes[final]
		if n == nil || n.kind != kLink {
			return
		}
		t, ok := f.hostRel(n.content)
		if !ok {
			return n.content, final
		}
		final = t
		if !f.mutants[SymlinkFollowed] {
			policy = t
		}
	}
	return
}

func (f *Fake) parentOK(rel string) bool {
	n := f.nodes[path.Dir(rel)]
	return n != nil && n.kind == kDir
}

// refusal says why rel may not be modified, "" when it may.
func (f *Fake) refusal(policy string) string {
	z := f.zoneOf(policy)
	if z == zMount {
		if f.mutants[MountPointsReplaceable] || f.mutants[RootsNotMounts] && f.isRoot(policy) {
			return ""
		}
		return eBusy
	}
	if !f.writableZone(z) {
		return eRofs
	}
	return ""
}

func (f *Fake) touchNode(n *node) { f.tick++; n.mtime = f.tick }

func (f *Fake) runCmd(user string, a []string, out *[]string, depth int) int {
	say := func(format string, args ...any) int {
		*out = append(*out, fmt.Sprintf(format, args...))
		return 1
	}
	switch a[0] {
	case "sh":
		if len(a) == 3 && a[1] == "-c" {
			return f.runList(user, a[2], out, depth+1)
		}
	case "echo":
		if len(a) == 4 && a[2] == ">>" {
			return f.appendTo(a[3], out)
		}
	case "truncate":
		if len(a) == 4 && a[1] == "-s" && a[2] == "0" {
			return f.modify("truncate", a[3], out, fmt.Sprintf("truncate: cannot open '%s' for writing: ", a[3]), func(n *node) { n.content = "" })
		}
	case "touch":
		if len(a) == 2 {
			rel, ok := f.hostRel(a[1])
			if !ok {
				return say("touch: cannot touch '%s': %s", a[1], eNoent)
			}
			if f.nodes[rel] == nil {
				return f.create(rel, kFile, "", fmt.Sprintf("touch: cannot touch '%s': ", a[1]), out)
			}
			return f.modify("touch", a[1], out, fmt.Sprintf("touch: cannot touch '%s': ", a[1]), func(*node) {})
		}
	case "chmod":
		if len(a) == 3 && a[1] == "600" {
			return f.modify("chmod", a[2], out, fmt.Sprintf("chmod: changing permissions of '%s': ", a[2]), func(n *node) { n.mode = n.mode&^0o777 | 0o600 })
		}
	case "cat":
		if len(a) == 2 {
			rel, ok := f.hostRel(a[1])
			n := f.nodes[rel]
			if !ok || n == nil {
				return say("cat: %s: %s", a[1], eNoent)
			}
			if n.kind == kDir {
				return say("cat: %s: %s", a[1], eIsDir)
			}
			*out = append(*out, strings.TrimRight(n.content, "\n"))
			return 0
		}
	case "find":
		if len(a) == 2 {
			rel, ok := f.hostRel(a[1])
			if !ok || f.nodes[rel] == nil {
				return say("find: '%s': %s", a[1], eNoent)
			}
			for _, p := range f.sortedKeys() {
				if p == rel || strings.HasPrefix(p, rel+"/") {
					*out = append(*out, sandboxPath(p))
				}
			}
			return 0
		}
	case "mkdir":
		if len(a) == 2 {
			return f.create2(a[1], kDir, "", "mkdir: cannot create directory '%s': ", out)
		}
	case "ln":
		switch {
		case len(a) == 4 && a[1] == "-s":
			return f.create2(a[3], kLink, a[2], "ln: failed to create symbolic link '%s': ", out)
		case len(a) == 4 && a[1] == "-sf":
			rel, ok := f.hostRel(a[3])
			if ok && f.nodes[rel] != nil && f.nodes[rel].kind == kLink {
				if f.refusal(rel) == "" {
					f.nodes[rel].content = a[2]
					f.touchNode(f.nodes[rel])
					f.logMut("symlink", rel)
					return 0
				}
			}
			return f.create2(a[3], kLink, a[2], "ln: failed to create symbolic link '%s': ", out)
		case len(a) == 3:
			return f.hardlink(a[1], a[2], out)
		}
	case "mv":
		if len(a) == 3 {
			return f.move(a[1], a[2], out)
		}
	case "rm":
		return f.remove(user, a[1:], out)
	case "umount":
		if len(a) == 2 {
			return f.umount(user, a[1], out)
		}
	case "rmdir":
		if len(a) == 2 {
			return f.rmdir(a[1], out)
		}
	case "head":
		if len(a) == 5 && a[1] == "-n" && a[2] == "2000" && a[3] == "/proc/self/mountinfo" && a[4] == "1>&2" {
			*out = append(*out, f.mountinfo()...)
			return 0
		}
	case "/work/conf-" + f.n8 + "-dev/part/" + conformance.ProbeName:
		if f.nodes[f.Part()+"/"+conformance.ProbeName] != nil {
			return f.probeMount(user, a[1:], out)
		}
	case "mount":
		if len(a) == 4 && a[1] == "-o" && a[2] == "remount,rw" {
			return say("mount: %s: %s", a[3], ePerm)
		}
		if len(a) == 5 && a[1] == "-t" && a[2] == "virtiofs" {
			return say("mount: %s: %s", a[4], ePerm)
		}
		if len(a) == 4 && a[1] == "--bind" && a[2] == "/tmp" {
			if user != "root" {
				return say("mount: %s: %s", a[3], ePerm)
			}
			return 0 // a change of the mount table, not of any file
		}
	}
	say("sh: 1: %s: not found", a[0])
	return 127
}

func (f *Fake) sortedKeys() []string {
	ks := make([]string, 0, len(f.nodes))
	for k := range f.nodes {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// cantCreate is what the base's /bin/sh, BusyBox's ash, prints for a
// redirection that cannot create or open p for writing: strerror's
// message, but for ENOENT and ENOTDIR, which it says as "nonexistent
// directory".
func cantCreate(p, why string) string {
	if why == eNoent || why == eNotDir {
		why = "nonexistent directory"
	}
	return fmt.Sprintf("sh: line 0: can't create %s: %s", p, why)
}

func (f *Fake) appendTo(p string, out *[]string) int {
	rel, ok := f.hostRel(p)
	if !ok {
		*out = append(*out, cantCreate(p, eNoent))
		return 2
	}
	final, policy := f.writeTarget(rel)
	n := f.nodes[final]
	if n == nil {
		if !f.parentOK(final) {
			*out = append(*out, cantCreate(p, eNoent))
			return 2
		}
		if why := f.refusal(policy); why != "" {
			*out = append(*out, cantCreate(p, why))
			return 2
		}
		nn := f.newNode(kFile, "x\n")
		f.nodes[final] = nn
		f.logMut("create", final)
		return 0
	}
	if why := f.refusal(policy); why != "" {
		*out = append(*out, cantCreate(p, why))
		return 2
	}
	if n.kind == kDir {
		*out = append(*out, cantCreate(p, eIsDir))
		return 2
	}
	n.content += "x\n"
	f.touchNode(n)
	f.logMut("append", final)
	return 0
}

// modify applies fn to the node at p when policy allows.
func (f *Fake) modify(op, p string, out *[]string, prefix string, fn func(*node)) int {
	rel, ok := f.hostRel(p)
	if !ok || f.nodes[rel] == nil {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	final, policy := f.writeTarget(rel)
	n := f.nodes[final]
	if n == nil {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if why := f.refusal(policy); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	fn(n)
	f.touchNode(n)
	f.logMut(op, final)
	return 0
}

func (f *Fake) create2(p string, k nodeKind, content, format string, out *[]string) int {
	rel, ok := f.hostRel(p)
	prefix := fmt.Sprintf(format, p)
	if !ok {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	return f.create(rel, k, content, prefix, out)
}

func (f *Fake) create(rel string, k nodeKind, content, prefix string, out *[]string) int {
	if f.nodes[rel] != nil {
		*out = append(*out, prefix+eExist)
		return 1
	}
	if !f.parentOK(rel) {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if why := f.refusal(rel); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	f.nodes[rel] = f.newNode(k, content)
	f.logMut("create", rel)
	return 0
}

func (f *Fake) hardlink(src, dst string, out *[]string) int {
	prefix := fmt.Sprintf("ln: failed to create hard link '%s' => '%s': ", dst, src)
	s, ok1 := f.hostRel(src)
	d, ok2 := f.hostRel(dst)
	if !ok1 || !ok2 || f.nodes[s] == nil || !f.parentOK(d) {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if f.nodes[d] != nil {
		*out = append(*out, prefix+eExist)
		return 1
	}
	if why := f.refusal(d); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	n := f.nodes[s]
	n.nlink++
	f.nodes[d] = n
	f.logMut("hardlink", d)
	return 0
}

// replaceable reports whether rel is a mount point and the sandbox may
// appear to replace it: the mutant that does so changes no file.
func (f *Fake) replaceable(rel string) bool {
	return f.zoneOf(rel) == zMount && (f.mutants[MountPointsReplaceable] || f.mutants[RootsNotMounts] && f.isRoot(rel))
}

// isRoot reports whether rel is one of the two roots.
func (f *Fake) isRoot(rel string) bool { return rel == f.RO() || rel == f.Dev() }

// gvisor is gVisor's flavour of the errors: its VFS checks in an order of
// its own, so a mount point that holds anything is refused an rmdir as
// not empty before it is refused as busy, and a rename of a mount point
// that holds another mount (the root that holds the writable part) fails
// with ENOENT, from the rootfs overlay's copy-up of what is below it,
// before the mount point is found busy. Neither changes anything.
func (f *Fake) gvisor() bool { return f.isolate == "gvisor" }

func (f *Fake) move(src, dst string, out *[]string) int {
	prefix := fmt.Sprintf("mv: cannot move '%s' to '%s': ", src, dst)
	s, ok1 := f.hostRel(src)
	d, ok2 := f.hostRel(dst)
	if !ok1 || !ok2 || f.nodes[s] == nil || !f.parentOK(d) {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if f.gvisor() && s == f.Dev() && !f.replaceable(s) && !f.unmount {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if why := f.refusal(s); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	if f.replaceable(s) {
		return 0
	}
	if why := f.refusal(d); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	for _, k := range f.sortedKeys() {
		if k == s || strings.HasPrefix(k, s+"/") {
			f.nodes[d+strings.TrimPrefix(k, s)] = f.nodes[k]
			delete(f.nodes, k)
			f.logMut("move", k)
		}
	}
	return 0
}

func (f *Fake) remove(user string, a []string, out *[]string) int {
	if len(a) >= 2 && a[0] == "-rf" {
		return f.wipe(a[1:], out)
	}
	recursive := false
	if len(a) == 2 && a[0] == "-r" {
		recursive, a = true, a[1:]
	}
	if len(a) != 1 {
		*out = append(*out, "rm: usage")
		return 1
	}
	prefix := fmt.Sprintf("rm: cannot remove '%s': ", a[0])
	rel, ok := f.hostRel(a[0])
	n := f.nodes[rel]
	if !ok || n == nil {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if n.kind == kDir && !recursive {
		*out = append(*out, prefix+eIsDir)
		return 1
	}
	if f.replaceable(rel) {
		return 0
	}
	if n.kind != kDir {
		if why := f.refusal(rel); why != "" {
			*out = append(*out, prefix+why)
			return 1
		}
		f.removeTree(rel)
		return 0
	}
	return f.removeDepthFirst(rel, out)
}

// removeDepthFirst is rm -r of a directory as a real one runs it: depth
// first, each entry removed when the policy lets it and it holds nothing
// any more, a message for each one that stays. So a tree whose top is
// refused (a read-only root, a mount point) still loses whatever below it
// is writable: rm -r of a root that holds a writable part empties the part.
func (f *Fake) removeDepthFirst(top string, out *[]string) int {
	keys := f.sortedKeys()
	slices.Reverse(keys)
	rc := 0
	for _, k := range keys {
		if k != top && !strings.HasPrefix(k, top+"/") {
			continue
		}
		if f.replaceable(k) {
			continue // the mutant says it went, and nothing changes
		}
		if why := f.refusal(k); why != "" {
			*out = append(*out, fmt.Sprintf("rm: cannot remove '%s': %s", sandboxPath(k), why))
			rc = 1
			continue
		}
		if f.nodes[k].kind == kDir && f.hasKids(k) {
			*out = append(*out, fmt.Sprintf("rm: cannot remove '%s': %s", sandboxPath(k), eNotEmpt))
			rc = 1
			continue
		}
		f.nodes[k].nlink--
		delete(f.nodes, k)
		f.logMut("remove", k)
	}
	return rc
}

func (f *Fake) hasKids(dir string) bool {
	for p := range f.nodes {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func (f *Fake) rmdir(p string, out *[]string) int {
	prefix := fmt.Sprintf("rmdir: failed to remove '%s': ", p)
	rel, ok := f.hostRel(p)
	n := f.nodes[rel]
	if !ok || n == nil {
		*out = append(*out, prefix+eNoent)
		return 1
	}
	if n.kind != kDir {
		*out = append(*out, prefix+eNotDir)
		return 1
	}
	if f.mutants[RootsNotMounts] && f.isRoot(rel) || f.gvisor() && f.zoneOf(rel) == zMount && !f.replaceable(rel) {
		if f.hasKids(rel) {
			*out = append(*out, prefix+eNotEmpt)
			return 1
		}
	}
	if why := f.refusal(rel); why != "" {
		*out = append(*out, prefix+why)
		return 1
	}
	if f.replaceable(rel) {
		return 0
	}
	for k := range f.nodes {
		if strings.HasPrefix(k, rel+"/") {
			*out = append(*out, prefix+eNotEmpt)
			return 1
		}
	}
	f.removeTree(rel)
	return 0
}

func (f *Fake) removeTree(rel string) {
	for _, k := range f.sortedKeys() {
		if k == rel || strings.HasPrefix(k, rel+"/") {
			f.nodes[k].nlink--
			delete(f.nodes, k)
			f.logMut("remove", k)
		}
	}
}

// wipe is rm -rf of each of paths in turn, as a real one runs it: what
// is not there is no error, and each tree is removed depth first, so what
// the policy lets go goes and the rest stays, with a message each.
func (f *Fake) wipe(paths []string, out *[]string) int {
	rc := 0
	if f.mutants[WipeReachesOutside] {
		for _, c := range f.canary {
			if f.zoneOf(c.Rel) == zOutside {
				f.removeTree(c.Rel)
			}
		}
	}
	for _, p := range paths {
		rel, ok := f.hostRel(p)
		if !ok || f.nodes[rel] == nil {
			continue
		}
		if f.nodes[rel].kind != kDir {
			if why := f.refusal(rel); why != "" {
				*out = append(*out, fmt.Sprintf("rm: cannot remove '%s': %s", p, why))
				rc = 1
				continue
			}
			f.removeTree(rel)
			continue
		}
		if f.removeDepthFirst(rel, out) != 0 {
			rc = 1
		}
	}
	return rc
}

func (f *Fake) umount(user, p string, out *[]string) int {
	prefix := fmt.Sprintf("umount: %s: ", p)
	rel, ok := f.hostRel(p)
	if !ok || rel != f.Part() || f.unmount {
		*out = append(*out, prefix+eNotMt)
		return 32
	}
	if f.mutants[UnmountWritesThrough] {
		f.unmount = true // a change of the mount table, not of any file
		return 0
	}
	if user == "root" {
		*out = append(*out, prefix+eBusy)
	} else {
		*out = append(*out, prefix+ePerm)
	}
	return 32
}

// Host side.

func (f *Fake) identity(n *node) conformance.Identity {
	id := conformance.Identity{Dev: 1, Ino: n.ino, Nlink: n.nlink, Mode: n.mode, MtimeNs: n.mtime}
	switch n.kind {
	case kFile:
		id.Size = int64(len(n.content))
		id.Hash = hashHex(n.content)
	case kLink:
		id.Size = int64(len(n.content))
		id.Hash = hashHex(n.content)
	}
	return id
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// MakeCanary makes the canary under parent (a directory of the model),
// as the suite does from the host.
func (f *Fake) MakeCanary(parent, role string, kind conformance.Kind) (conformance.Canary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.nodes[parent]; p == nil || p.kind != kDir {
		return conformance.Canary{}, fmt.Errorf("parent %s is not a directory", parent)
	}
	if _, dup := f.canary[role]; dup {
		return conformance.Canary{}, fmt.Errorf("canary %q made twice", role)
	}
	name := "conf-" + f.n8 + "-" + role
	rel := parent + "/" + name
	c := conformance.Canary{Role: role, Kind: kind, Rel: rel, Paths: []string{rel}}
	content := "caboose-conformance " + f.nonce + " " + role + "\n"
	mk := func(p string, k nodeKind) {
		f.nodes[p] = f.newNode(k, map[bool]string{true: content}[k == kFile])
		f.logMut("make", p)
	}
	switch kind {
	case conformance.File:
		mk(rel, kFile)
	case conformance.Dir:
		mk(rel, kDir)
	case conformance.Tree:
		dirs := []string{rel, rel + "/" + name + "-a", rel + "/" + name + "-a/" + name + "-b"}
		for i, d := range dirs {
			mk(d, kDir)
			if i > 0 {
				c.Paths = append(c.Paths, d)
			}
			for j := 1; j <= 2; j++ {
				p := fmt.Sprintf("%s/%s-%d", d, name, 2*i+j)
				mk(p, kFile)
				c.Paths = append(c.Paths, p)
			}
		}
	}
	f.canary[role] = c
	for _, p := range c.Paths {
		f.rec[p] = record{id: f.identity(f.nodes[p])}
	}
	return c, nil
}

// record is what the host last recorded of a canary path.
type record struct {
	id   conformance.Identity
	gone bool
}

// Recorded is the host's record of the canary path rel.
func (f *Fake) Recorded(rel string) (conformance.Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rec[rel]
	if !ok {
		return conformance.Identity{}, false, fmt.Errorf("conformance: %s is not a canary of this run", rel)
	}
	return r.id, r.gone, nil
}

// recordAllowed records the canary paths at or below allowed as they are
// now, as the real post-check does after a check; f.mu held.
func (f *Fake) recordAllowed(allowed []string) {
	for p := range f.rec {
		for _, a := range allowed {
			if p == a || strings.HasPrefix(p, a+"/") {
				if n := f.nodes[p]; n != nil {
					f.rec[p] = record{id: f.identity(n)}
				} else {
					f.rec[p] = record{gone: true}
				}
			}
		}
	}
}

// Vanish takes the host file rel away and Revive puts a file back at it,
// each as something outside the sandbox would: faults for tests.
func (f *Fake) Vanish(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodes[rel] != nil {
		f.removeTree(rel)
	}
}

func (f *Fake) Revive(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodes[rel] == nil {
		f.nodes[rel] = f.newNode(kFile, "revived\n")
		f.logMut("revive", rel)
	}
}

// Tamper changes the contents of the host file rel, as something outside
// any check (a process left running, a restart) would: a fault for tests.
func (f *Fake) Tamper(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.nodes[rel]; n != nil && n.kind == kFile {
		n.content += "tampered\n"
		f.touchNode(n)
		f.logMut("tamper", rel)
	}
}

func (f *Fake) Canary(role string) (conformance.Canary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.canary[role]
	if !ok || f.hidden[role] {
		return conformance.Canary{}, fmt.Errorf("no canary %q", role)
	}
	return c, nil
}

// Identity is rel's identity, for a canary path, a path below one or the
// probe only, as the real host's: a check that asks of anything else
// errs here as in a real run.
func (f *Fake) Identity(rel string) (conformance.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inCanary(rel) && !conformance.IsProbeRel(f.n8, rel) {
		return conformance.Identity{}, fmt.Errorf("conformance: %s is not a canary of this run", rel)
	}
	n := f.nodes[rel]
	if n == nil {
		return conformance.Identity{}, fs.ErrNotExist
	}
	return f.identity(n), nil
}

// inCanary reports whether rel is a canary's path or below one.
func (f *Fake) inCanary(rel string) bool {
	for _, c := range f.canary {
		if rel == c.Rel || strings.HasPrefix(rel, c.Rel+"/") {
			return true
		}
	}
	return false
}

func (f *Fake) Snapshot(c conformance.Canary) (map[string]conformance.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]conformance.Identity{}
	for p, n := range f.nodes {
		if p == c.Rel || strings.HasPrefix(p, c.Rel+"/") {
			out[p] = f.identity(n)
		}
	}
	return out, nil
}

// Mutations is every change made to the model, by anyone: the host's
// canaries and whatever the sandbox's commands did.
func (f *Fake) Mutations() []conformance.Mutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.muts)
}

// Canaries are every canary made, as the real host lists them, but for
// those HideCanary hid.
func (f *Fake) Canaries() []conformance.Canary {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []conformance.Canary
	for role, c := range f.canary {
		if !f.hidden[role] {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// Restart is the sandbox restarted, as the runner asks before a Fresh
// step: what an unmount took is mounted again.
func (f *Fake) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("the sandbox is not running")
	}
	f.unmount = false
	f.restarts++
	f.event("restart")
	return nil
}

// Restarts is how many times Restart ran.
func (f *Fake) Restarts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.restarts }

// CanaryPaths is every path of every canary made, sorted.
func (f *Fake) CanaryPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.canary {
		out = append(out, c.Paths...)
	}
	sort.Strings(out)
	return out
}

// Clock and Net.

func (f *Fake) Now() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

type listener struct{ closed bool }

func (l *listener) Addr() string  { return "127.0.0.1:0" }
func (l *listener) Accepted() int { return 0 }
func (l *listener) Close() error  { l.closed = true; return nil }
func (f *Fake) Listen(context.Context) (conformance.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listener++
	return &listener{}, nil
}

// Env is a conformance.Env over the fake.
func (f *Fake) Env(isolation, trust string) conformance.Env {
	return conformance.NewEnv(f, f, f, f, isolation, trust)
}

// AddPlainFiles puts n files that are no canary below dir, as a git
// repository's own files are below the canary that holds it: named
// file-1 .. file-n, holding neither the run's names nor its nonce.
func (f *Fake) AddPlainFiles(dir string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.nodes[dir]; p == nil || p.kind != kDir {
		return fmt.Errorf("%s is not a directory", dir)
	}
	for i := 1; i <= n; i++ {
		f.nodes[fmt.Sprintf("%s/file-%d", dir, i)] = f.newNode(kFile, "plain\n")
	}
	return nil
}
