package checks_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

func init() {
	for _, m := range []fake.Mutant{fake.MountPointsReplaceable, fake.ManagedWritable, fake.LabelsDrift, fake.OutsideReachable, fake.RootsNotMounts, fake.WipeReachesOutside} {
		fake.Register(m)
	}
}

const (
	isoContainer = "container"
	isoGvisor    = "gvisor"
	isoVM        = "vm"
)

var paths = func(f *fake.Fake) checks.Paths {
	return checks.Paths{RO: f.RO(), Dev: f.Dev(), Part: f.Part(), Managed: "data/claude-code", Canary: "canary"}
}

func setup(f *fake.Fake) error {
	f.PlaceProbe()
	specs, err := checks.Layout(paths(f))
	if err != nil {
		return err
	}
	for _, s := range specs {
		if _, err := f.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			return err
		}
	}
	return nil
}

func world(t testing.TB, iso string) *fake.Fake {
	t.Helper()
	f := fake.New("0a1b2c3d")
	f.SetIsolation(iso)
	if err := setup(f); err != nil {
		t.Fatal(err)
	}
	fake.NoAgentRuns(t, f)
	return f
}

func anchor(group string) c.Check {
	return c.Check{ID: "anchor", Group: group, Class: c.Observe,
		Run: func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Pass} }}
}

// runWith runs checks (after a harmless first step) in the model through
// sb, which may wrap it.
func runWith(f *fake.Fake, sb c.Sandbox, iso string, cks ...c.Check) ([]c.Result, *c.Collector) {
	env := c.NewEnv(sb, f, f, f, iso, "fallible")
	r := &c.Runner{Env: &fake.Envelope{F: f}, Wipe: true, Restart: f.Restart}
	// A failed destructive check ends the run with an error; the results
	// are still there.
	res, col, _ := c.RunChecks(context.Background(), r, append([]c.Check{anchor("files")}, cks...), env)
	return res, col
}

func status(res []c.Result, id string) (c.Status, string) {
	for _, r := range res {
		if r.ID == id {
			return r.Status, r.Reason
		}
	}
	return "", "absent"
}

func byID(t testing.TB, id string) c.Check {
	t.Helper()
	for _, ck := range checks.All() {
		if ck.ID == id {
			return ck
		}
	}
	t.Fatalf("no check %s", id)
	return c.Check{}
}

func TestAllHasEveryCheckOnce(t *testing.T) {
	var ids []string
	for _, ck := range checks.All() {
		ids = append(ids, ck.ID)
		if ck.Run == nil || ck.Control == "" || ck.Title == "" {
			t.Errorf("%s: incomplete", ck.ID)
		}
	}
	slices.Sort(ids)
	want := []string{"R1", "R10", "R11", "R12", "R13", "R14", "R2", "R3", "R4", "R5", "R6", "R7", "R8", "R9"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}
	if got := byID(t, "R13").Class; got != c.Observe {
		t.Errorf("R13 class %v", got)
	}
	for _, id := range []string{"R2", "R3", "R4", "R5", "R8", "R9", "R10", "R12"} {
		if byID(t, id).Class != c.Destructive {
			t.Errorf("%s is not destructive", id)
		}
	}
	// Applicability.
	for id, want := range map[string][3]bool{
		// container, gvisor, vm
		"R1": {true, true, true}, "R9": {false, false, true}, "R11": {false, false, true},
		"R10": {true, true, false}, "R13": {true, true, true},
	} {
		ck := byID(t, id)
		for i, iso := range []string{isoContainer, isoGvisor, isoVM} {
			if got := ck.Applies(iso, "fallible"); got != want[i] {
				t.Errorf("%s on %s: applies %v", id, iso, got)
			}
		}
	}
}

// sandboxWorlds are the isolations, with gVisor as each engine makes it:
// run as root (the agent cannot write its mounts) and as the agent.
var sandboxWorlds = []struct {
	iso         string
	agentWrites bool
}{{isoContainer, false}, {isoGvisor, false}, {isoGvisor, true}, {isoVM, false}}

func TestHonestSandboxPasses(t *testing.T) {
	for _, w := range sandboxWorlds {
		iso := w.iso
		f := world(t, iso)
		f.SetRunsAsRoot(!w.agentWrites)
		res, _ := runWith(f, f, iso, checks.All()...)
		n := 0
		for _, ck := range checks.All() {
			if !ck.Applies(iso, "fallible") {
				continue
			}
			n++
			st, why := status(res, ck.ID)
			if want := c.Pass; st != want {
				t.Errorf("%s %s: %s %s", iso, ck.ID, st, why)
			}
		}
		if len(res) != n+1 {
			t.Errorf("%s: %d results for %d checks", iso, len(res), n)
		}
		if err := fake.CheckOrder(f.Events()); err != nil {
			t.Errorf("%s: %v", iso, err)
		}
	}
}

func TestMutants(t *testing.T) {
	var all []c.Check
	for _, ck := range checks.All() {
		// R5 is the positive control itself: when it fails it is an
		// ERROR (TestPositiveControlFailureIsError), never a FAIL.
		if ck.ID != "R5" {
			all = append(all, ck)
		}
	}
	table := func(extra fake.Table) fake.Table {
		out := fake.Table{
			fake.SymlinkFollowed:        {"R6"},
			fake.MountPointsReplaceable: {"R3", "R4"},
			fake.ManagedWritable:        {"R12", "R14"},
			fake.WipeReachesOutside:     {"R14"},
			fake.OutsideReachable:       {"R7"},
			fake.LabelsDrift:            {"R13"},
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	docker := table(fake.Table{
		fake.RootsWritable:        {"R1", "R2", "R6", "R7", "R8", "R13", "R14"},
		fake.ParentWritable:       {"R4", "R6", "R7", "R8", "R13", "R14"},
		fake.UnmountWritesThrough: {"R10"},
		fake.RootsNotMounts:       {"R3", "R10"},
	})
	// gVisor allows the user namespace, so a remount that lands there is a
	// breach R10 must catch; a container's seccomp refuses the namespace,
	// so the mutant breaks nothing under container.
	gvisor := table(fake.Table{
		fake.RootsWritable:           {"R1", "R2", "R6", "R7", "R8", "R13", "R14"},
		fake.ParentWritable:          {"R4", "R6", "R7", "R8", "R13", "R14"},
		fake.UnmountWritesThrough:    {"R10"},
		fake.RootsNotMounts:          {"R3", "R10"},
		fake.UsernsRemountLands:      {"R10"},
		fake.UsernsUnmountReachesOut: {"R10"},
	})
	for _, w := range []struct {
		name, iso   string
		agentWrites bool
		tbl         fake.Table
	}{
		{isoContainer, isoContainer, false, docker},
		{"gvisor-root", isoGvisor, false, gvisor},
		{"gvisor-agent", isoGvisor, true, gvisor},
		{isoVM, isoVM, false, table(fake.Table{
			fake.RootsWritable:        {"R1", "R2", "R6", "R7", "R8", "R9", "R13", "R14"},
			fake.ParentWritable:       {"R4", "R6", "R7", "R8", "R9", "R11", "R13", "R14"},
			fake.UnmountWritesThrough: {"R9"},
			fake.RootsNotMounts:       {"R3"},
		})},
	} {
		t.Run(w.name, func(t *testing.T) {
			fake.CheckMutants(t, all, w.tbl, fake.Config{Isolation: w.iso, Trust: "fallible", AgentWrites: w.agentWrites, Setup: setup})
		})
	}
}

func TestPositiveControlFailureIsError(t *testing.T) {
	for _, iso := range []string{isoContainer, isoVM} {
		for _, ck := range checks.All() {
			if !ck.Applies(iso, "fallible") || ck.ID == "R13" {
				continue
			}
			f := world(t, iso)
			f.Break(fake.PartReadOnly)
			res, _ := runWith(f, f, iso, ck)
			if st, why := status(res, ck.ID); st != c.Error {
				t.Errorf("%s %s with a writable part that refuses: %s %s", iso, ck.ID, st, why)
			}
		}
	}
}

func TestNoVacuousPass(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(f *fake.Fake, ck c.Check)
	}{
		{"the sandbox is down", func(f *fake.Fake, _ c.Check) { f.SetDown(true) }},
		{"the shell is not there", func(f *fake.Fake, _ c.Check) { f.SetShellMissing(true) }},
		{"the ro-file canary is missing", func(f *fake.Fake, _ c.Check) { f.HideCanary("ro-file") }},
		{"the outside canary is missing", func(f *fake.Fake, _ c.Check) { f.HideCanary("outside") }},
	} {
		for _, ck := range checks.All() {
			if !ck.Applies(isoVM, "fallible") {
				continue
			}
			if ck.ID == "R13" && tc.name == "the shell is not there" {
				continue // it sends no command
			}
			if strings.HasPrefix(tc.name, "the ro-file") && !slices.Contains(ck.Roles, "ro-file") && ck.ID != "R7" && ck.ID != "R13" {
				continue // a check that does not use it
			}
			if strings.HasPrefix(tc.name, "the outside") && ck.ID != "R7" {
				continue
			}
			f := world(t, isoVM)
			tc.mod(f, ck)
			res, _ := runWith(f, f, isoVM, ck)
			if st, why := status(res, ck.ID); st == c.Pass {
				t.Errorf("%s: %s passed (%s)", tc.name, ck.ID, why)
			}
		}
	}
	// Each role a check uses, missing alone, keeps it from passing.
	for _, ck := range checks.All() {
		if !ck.Applies(isoVM, "fallible") {
			continue
		}
		for _, role := range ck.Roles {
			if !roleUsedIn(ck, role, isoVM) {
				continue
			}
			f := world(t, isoVM)
			f.HideCanary(role)
			res, _ := runWith(f, f, isoVM, ck)
			if st, why := status(res, ck.ID); st == c.Pass {
				t.Errorf("canary %s missing: %s passed (%s)", role, ck.ID, why)
			}
		}
	}
}

// roleUsedIn reports whether check ck uses its role under iso: the roles
// of a user it does not run as there (the agent's, under vm) are made
// for the other isolations.
func roleUsedIn(ck c.Check, role, iso string) bool {
	return iso != isoVM || !strings.HasSuffix(role, "-a")
}

func TestR9RecordsTheUnmount(t *testing.T) {
	for _, tc := range []struct {
		mut  fake.Mutant
		want string
		st   c.Status
	}{
		{fake.Enforcing, "unmount: not possible", c.Pass},
		{fake.UnmountWritesThrough, "unmount: possible", c.Fail},
	} {
		f := world(t, isoVM)
		f.Break(tc.mut)
		res, col := runWith(f, f, isoVM, byID(t, "R9"))
		if st, why := status(res, "R9"); st != tc.st {
			t.Errorf("%q: %s %s", tc.mut, st, why)
		}
		if !slices.Contains(col.Lines("R9"), tc.want) {
			t.Errorf("%q: lines %v lack %q", tc.mut, col.Lines("R9"), tc.want)
		}
		for _, l := range col.Lines("R9") {
			if strings.Contains(l, "stays") {
				t.Errorf("a claim about the guest's mount: %q", l)
			}
		}
	}
}

// The checks run as the user the sandbox's sessions are, read off its
// LabelUser, and as root too where they say so: under vm, and under a
// gVisor that runs as root, the sessions are root, and the agent user,
// who can write none of the mounts, is never tried.
func TestRanAs(t *testing.T) {
	for _, tc := range []struct {
		iso    string
		asRoot bool
		id     string
		want   string
	}{
		{isoContainer, false, "R1", "ran as: agent"},
		{isoGvisor, false, "R1", "ran as: agent"},
		{isoGvisor, true, "R1", "ran as: root"},
		{isoVM, false, "R1", "ran as: root"},
		{isoGvisor, false, "R10", "ran as: agent, root"},
		{isoGvisor, true, "R10", "ran as: root"},
		{isoVM, false, "R5", "ran as: root"},
		{isoContainer, false, "R5", "ran as: agent"},
	} {
		f := world(t, tc.iso)
		f.SetRunsAsRoot(tc.asRoot)
		res, col := runWith(f, f, tc.iso, byID(t, tc.id))
		if !slices.Contains(col.Lines(tc.id), tc.want) {
			t.Errorf("%s root=%v %s: %v", tc.iso, tc.asRoot, tc.id, col.Lines(tc.id))
		}
		if st, why := status(res, tc.id); st != c.Pass && st != c.Skip {
			t.Errorf("%s root=%v %s: %s %s", tc.iso, tc.asRoot, tc.id, st, why)
		}
		if slices.Contains(f.Events(), "exec-as-agent") {
			t.Errorf("%s root=%v %s: ran as the agent in a sandbox that runs as root", tc.iso, tc.asRoot, tc.id)
		}
	}
}

// recorder keeps every command a check sent.
type recorder struct {
	*fake.Fake
	cmds []string
}

func (r *recorder) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if body, ok := c.Unwrap(argv); ok {
		r.cmds = append(r.cmds, body)
	}
	return r.Fake.Exec(ctx, user, argv, stdin)
}

var allowedArg = regexp.MustCompile(`^(/work/conf-[0-9a-f]{8}-(ro|dev)(/part)?(/\.\./conf-[0-9a-f]{8}-(ro|dev)(/part)?)?(/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]*)*)?(\.moved)?|/etc/claude-code/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]*)*|/proc/self/root/work/conf-[0-9a-f]{8}-(ro|dev)(/part)?/conf-[0-9a-f]{8}-[a-z0-9-]+|/tmp|/(work|proc/self/root)/\\.\\./canary/conf-[0-9a-f]{8}-[a-z0-9-]+|/work/conf-[0-9a-f]{8}-(ro|dev)/\\.\\./canary/conf-[0-9a-f]{8}-[a-z0-9-]+|conf-[0-9a-f]{8}-(ro|dev)|/work/conf-[0-9a-f]{8}-dev/part/\.conformance-probe|[0-9a-f]{8}-fake-nonce|(-|cat|-t|virtiofs|>>|&&|echo|x|mkdir|mv|rm|rmdir|umount|mount|ln|touch|chmod|truncate|0|600|--bind|remount,rw|-o|-s|-sf|-r|head|-n|2000|/proc/self/mountinfo|1>&2|--nonce|--mount|--target|remount-bind-rw|remount-rw|remount-bind-ro|umount-detach|userns-remount|--part|--witness))$`)

var hostArg = regexp.MustCompile(`^(/work/conf-[0-9a-f]{8}-(ro|dev)|/work|/proc/self/root)/\.\./canary/conf-[0-9a-f]{8}-[a-z0-9-]+$|^/work/\.\./canary/conf-[0-9a-f]{8}-[a-z0-9-]+$|^conf-[0-9a-f]{8}-(ro|dev)$`)

func TestCommandsAreLiteralAndValid(t *testing.T) {
	seen := map[string]bool{}
	for _, iso := range []string{isoContainer, isoVM} {
		for _, ck := range checks.All() {
			if !ck.Applies(iso, "fallible") {
				continue
			}
			f := world(t, iso)
			rec := &recorder{Fake: f}
			runWith(f, rec, iso, ck)
			for _, body := range rec.cmds {
				if seen[body] {
					continue
				}
				seen[body] = true
				if strings.ContainsAny(body, "*?[]`~$;|(){}\\\"") {
					t.Errorf("%s: %q has a glob, expansion or metacharacter", ck.ID, body)
				}
				if strings.Contains(body, "-rf") != (ck.ID == "R14") {
					t.Errorf("%s: %q: only the wipe removes with -rf, and it removes nothing else", ck.ID, body)
				}
				for _, tok := range strings.Fields(body) {
					if tok == "-rf" && ck.ID == "R14" {
						continue
					}
					if !allowedArg.MatchString(tok) && !hostArg.MatchString(tok) {
						t.Errorf("%s: %q: argument %q is not a literal canary path", ck.ID, body, tok)
					}
				}
				if out, err := exec.Command("sh", "-n", "-c", body).CombinedOutput(); err != nil {
					t.Errorf("%s: %q is not valid sh: %v %s", ck.ID, body, err, out)
				}
			}
		}
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct commands were seen", len(seen))
	}
}

// TestChecksTouchOnlyCanaries runs every check alone with every mutant on:
// whatever the sandbox managed to change on the host side of the model is
// a canary or below one.
func TestChecksTouchOnlyCanaries(t *testing.T) {
	for _, iso := range []string{isoContainer, isoVM} {
		for _, ck := range checks.All() {
			if !ck.Applies(iso, "fallible") {
				continue
			}
			for _, broken := range []bool{false, true} {
				f := world(t, iso)
				if broken {
					f.BreakAll()
					for _, m := range fake.Registered() {
						if m != fake.PartReadOnly {
							f.Break(m)
						}
					}
					f.Break(fake.UsernsRemountLands)
				}
				runWith(f, f, iso, ck)
				canaries := f.CanaryPaths()
				for _, m := range f.Mutations() {
					// The wipe removes the writable part, the probe in it too.
					ok := ck.Wipe && c.IsProbeRel("0a1b2c3d", m.Rel)
					for _, p := range canaries {
						if m.Rel == p || strings.HasPrefix(m.Rel, p+"/") {
							ok = true
							break
						}
					}
					if !ok {
						t.Errorf("%s %s (broken=%v): %s touched %s, which is no canary", iso, ck.ID, broken, m.Op, m.Rel)
					}
				}
			}
		}
	}
}

func TestLayoutIsConsistent(t *testing.T) {
	f := fake.New("0a1b2c3d")
	specs, err := checks.Layout(paths(f))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range specs {
		if seen[s.Role] {
			t.Errorf("role %s listed twice", s.Role)
		}
		seen[s.Role] = true
		if len(s.Role) > 24 {
			t.Errorf("role %s is too long", s.Role)
		}
	}
	// Every role a check may change is made.
	for _, ck := range checks.All() {
		for _, r := range ck.Roles {
			if !seen[r] {
				t.Errorf("%s: role %s is not in the layout", ck.ID, r)
			}
		}
	}
	for _, r := range []string{"ro-file", "parent-file"} { // R13 reads these
		if !seen[r] {
			t.Errorf("role %s is not in the layout", r)
		}
	}
}

// TestSourceHasNoRemovals: the checks delete nothing themselves.
func TestSourceHasNoRemovals(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"os.Remove", "os.Rename", "exec.Command", "RemoveAll", "/bin/sh", "SocketPath"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s names %s", f, bad)
			}
		}
	}
}

func TestR13ReadsLabelsAndMounts(t *testing.T) {
	f := world(t, isoContainer)
	res, _ := runWith(f, f, isoContainer, byID(t, "R13"))
	if st, why := status(res, "R13"); st != c.Pass {
		t.Fatalf("%s %s", st, why)
	}
	// No sandbox command: R13 sends none.
	if slices.Contains(f.Events(), "exec") {
		t.Errorf("R13 ran a command: %v", f.Events())
	}
}

// say replaces a sandbox's answer to some commands.
type liar struct {
	*fake.Fake
	f func(user, body string) (rc int, out, errs string, ok bool)
}

func (l *liar) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if body, ok := c.Unwrap(argv); ok {
		if rc, out, e, ok := l.f(user, body); ok {
			return rc, out, e, nil
		}
	}
	return l.Fake.Exec(ctx, user, argv, stdin)
}

// R10 makes the mount calls through the suite's probe, never a mount
// binary: an image without one (caboose's default apko image has none)
// answered "mount: not found", which proves nothing.
func TestR10MakesTheCallsThroughTheProbe(t *testing.T) {
	for _, w := range sandboxWorlds {
		if w.iso == isoVM {
			continue
		}
		f := world(t, w.iso)
		f.SetRunsAsRoot(!w.agentWrites)
		rec := &recorder{Fake: f}
		res, col := runWith(f, rec, w.iso, byID(t, "R10"))
		if st, why := status(res, "R10"); st != c.Pass {
			t.Errorf("%s agent-writes=%v: R10 %s %s (%v)", w.iso, w.agentWrites, st, why, col.Lines("R10"))
		}
		calls := 0
		for _, b := range rec.cmds {
			if strings.HasPrefix(b, "mount ") || strings.HasPrefix(b, "umount ") || strings.Contains(b, "&& umount") {
				t.Errorf("%s: R10 ran a mount binary: %q", w.iso, b)
			}
			if strings.Contains(b, "/part/.conformance-probe --nonce ") && strings.Contains(b, " --mount ") {
				calls++
			}
		}
		if calls == 0 {
			t.Errorf("%s: R10 made no call through the probe", w.iso)
		}
	}
}

func TestR10WithoutTheProbeIsError(t *testing.T) {
	f := fake.New("0a1b2c3d")
	f.SetIsolation(isoContainer)
	specs, err := checks.Layout(paths(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if _, err := f.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := runWith(f, f, isoContainer, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Error || !strings.Contains(why, "probe") {
		t.Errorf("no probe: R10 %s %s", st, why)
	}
}

func TestR10Answers(t *testing.T) {
	probeCall := func(body string) bool { return strings.Contains(body, " --mount ") }
	for _, tc := range []struct {
		name string
		lie  func(user, body string) (int, string, string, bool)
		want c.Status
		why  string
	}{
		{"the old mount binary, missing", func(_, body string) (int, string, string, bool) {
			if probeCall(body) {
				return 0, "\nrc=127\n", "/bin/sh: line 0: mount: not found", true
			}
			return 0, "", "", false
		}, c.Error, "not the refused status"},
		{"a call that succeeds", func(_, body string) (int, string, string, bool) {
			if probeCall(body) && strings.Contains(body, "remount-bind-rw") {
				return 0, "\nrc=0\n", "", true
			}
			return 0, "", "", false
		}, c.Fail, "reported success"},
		{"busy: the capability was there", func(_, body string) (int, string, string, bool) {
			if probeCall(body) && strings.Contains(body, " umount ") {
				return 0, "\nrc=1\n", "conformance-probe: umount /work/x: Device or resource busy", true
			}
			return 0, "", "", false
		}, c.Fail, "refused as ebusy"},
		{"the gate refuses", func(_, body string) (int, string, string, bool) {
			if probeCall(body) {
				return 0, "\nrc=3\n", "conformance-probe: the gate refused: /work lists 3 entries", true
			}
			return 0, "", "", false
		}, c.Error, "not the refused status"},
	} {
		f := world(t, isoContainer)
		res, _ := runWith(f, &liar{f, tc.lie}, isoContainer, byID(t, "R10"))
		if st, why := status(res, "R10"); st != tc.want || !strings.Contains(why, tc.why) {
			t.Errorf("%s: R10 %s %s", tc.name, st, why)
		}
	}
}

func TestR10ProbeSwappedIsError(t *testing.T) {
	f := world(t, isoContainer)
	// The sandbox can write the part: a probe it replaced answers for itself.
	if _, _, _, err := f.Exec(context.Background(), "agent", mustArgv(t, "/work/conf-0a1b2c3d-dev/part/.conformance-probe"), nil); err != nil {
		t.Fatal(err)
	}
	res, _ := runWith(f, f, isoContainer, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Error || !strings.Contains(why, "not the one the suite placed") {
		t.Errorf("a swapped probe: R10 %s %s", st, why)
	}
}

// mustArgv is an append to p, wrapped, which the model runs as a write.
func mustArgv(t *testing.T, p string) []string {
	t.Helper()
	return []string{"/bin/sh", "-c", "LC_ALL=C; export LC_ALL; { echo x >> " + p + "; } >/dev/null; printf '\\nrc=%s\\n' \"$?\""}
}

// A call said refused that still changed the mount is a FAIL: the mount
// table is read before and after each one.
func TestR10SeesAChangedMount(t *testing.T) {
	changed := false
	lie := func(_, body string) (int, string, string, bool) {
		switch {
		case strings.Contains(body, " --mount remount-bind-rw "):
			changed = true
			return 0, "\nrc=1\n", "conformance-probe: remount-bind-rw /work/x: Operation not permitted", true
		case changed && body == "head -n 2000 /proc/self/mountinfo 1>&2":
			return 0, "\nrc=0\n", "1 0 0:1 / / rw - overlay overlay rw\n20 1 0:40 / /work/conf-0a1b2c3d-ro rw,relatime - fakefs fake:roots/conf-0a1b2c3d-ro rw", true
		}
		return 0, "", "", false
	}
	f := world(t, isoContainer)
	res, _ := runWith(f, &liar{f, lie}, isoContainer, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Fail || !strings.Contains(why, "the mount changed") {
		t.Errorf("R10 %s %s", st, why)
	}
}

func TestR5IgnoresALyingSandbox(t *testing.T) {
	for name, lie := range map[string]func(user, body string) (int, string, string, bool){
		"success that did not happen": func(_, body string) (int, string, string, bool) {
			return 0, "\nrc=0\n", "", true
		},
		"a refusal": func(_, body string) (int, string, string, bool) {
			return 0, "\nrc=1\n", "sh: 1: cannot create x: Read-only file system", true
		},
	} {
		f := world(t, isoContainer)
		res, _ := runWith(f, &liar{f, lie}, isoContainer, byID(t, "R5"))
		if st, why := status(res, "R5"); st != c.Error {
			t.Errorf("%s: R5 %s %s", name, st, why)
		}
	}
}

func TestR7WatchCompareSeesAChange(t *testing.T) {
	f := world(t, isoContainer)
	f.Break(fake.OutsideReachable)
	res, col := runWith(f, f, isoContainer, byID(t, "R7"))
	st, why := status(res, "R7")
	if st != c.Fail {
		t.Fatalf("%s %s", st, why)
	}
	found := false
	for _, l := range col.Lines("R7") {
		found = found || strings.Contains(l, "REACHED")
	}
	if !found {
		t.Errorf("lines: %v", col.Lines("R7"))
	}
	// The watch alone: the sandbox hides what it did.
	f = world(t, isoContainer)
	f.Break(fake.OutsideReachable)
	res, _ = runWith(f, &hider{Fake: f}, isoContainer, byID(t, "R7"))
	if st, why := status(res, "R7"); st != c.Fail || !strings.Contains(why, "changed on the host") {
		t.Errorf("R7 with hidden writes: %s %s", st, why)
	}
}

// hider runs every command for real and then reports a refusal for those
// that reach the host's canaries.
type hider struct{ *fake.Fake }

func (h *hider) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	rc, out, e, err := h.Fake.Exec(ctx, user, argv, stdin)
	if body, ok := c.Unwrap(argv); ok && strings.Contains(body, "/canary/") {
		return 0, "\nrc=1\n", "sh: 1: cannot create x: Read-only file system", nil
	}
	return rc, out, e, err
}

// A sandbox whose LabelUser is missing, says no user caboose makes, or a
// user the isolation never runs as, has no session user: every check that
// runs a command is an ERROR that says so, never a panic or a pass.
func TestSessionUserLabel(t *testing.T) {
	for _, tc := range []struct {
		iso, label string
		drop       bool
	}{
		{isoContainer, "", true},
		{isoContainer, "1000", false},
		{isoContainer, c.RootUser, false},
		{isoVM, "", false},
		{isoGvisor, "root", false},
	} {
		f := world(t, tc.iso)
		f.SetUserLabel(tc.label, tc.drop)
		res, _ := runWith(f, f, tc.iso, checks.All()...)
		for _, r := range res {
			if strings.Contains(r.Reason, "panicked") {
				t.Errorf("%s %q drop=%v %s: %s", tc.iso, tc.label, tc.drop, r.ID, r.Reason)
			}
		}
		for _, id := range []string{"R1", "R6", "R7"} {
			st, why := status(res, id)
			if st != c.Error || !strings.Contains(why, "dev.bfreis.caboose.user") {
				t.Errorf("%s %q drop=%v %s: %s %s", tc.iso, tc.label, tc.drop, id, st, why)
			}
		}
	}
}

// rm -r of a root, as a real rm runs it, empties the writable part inside
// it before failing on the mount point: the part is writable by design,
// and holds the run's other canaries. R3 tries the mount point itself
// (mv, rmdir), so it passes in a model whose rm -r is the real one's and
// leaves every other canary of the part as it was.
func TestR3LeavesThePartAlone(t *testing.T) {
	for _, w := range sandboxWorlds {
		f := world(t, w.iso)
		f.SetRunsAsRoot(!w.agentWrites)
		part := f.Part() + "/conf-0a1b2c3d-um-wr"
		before := f.CanaryPaths()
		res, _ := runWith(f, f, w.iso, byID(t, "R3"))
		if st, why := status(res, "R3"); st != c.Pass {
			t.Errorf("%s R3: %s %s", w.iso, st, why)
		}
		for _, m := range f.Mutations() {
			if m.Op == "remove" {
				t.Errorf("%s: R3 removed %s", w.iso, m.Rel)
			}
		}
		if !slices.Contains(before, part) {
			t.Fatalf("no canary %s", part)
		}
		// The model's rm -r is the real one's: of the dev root it removes
		// what the part holds, and refuses the root.
		g := world(t, w.iso)
		g.SetRunsAsRoot(!w.agentWrites)
		pf, err := g.Canary("parent-file")
		if err != nil {
			t.Fatal(err)
		}
		sp, err := c.SandboxPath(pf)
		if err != nil {
			t.Fatal(err)
		}
		root, err := sp.Root()
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := c.RemoveTree(root)
		if err != nil {
			t.Fatal(err)
		}
		_, out, _, err := g.Exec(context.Background(), "root", cmd.Argv(), nil)
		if err != nil || strings.Contains(out, "rc=0") {
			t.Fatalf("%s: rm -r of the root: %q %v", w.iso, out, err)
		}
		removed := false
		for _, m := range g.Mutations() {
			if m.Op == "remove" && m.Rel == part {
				removed = true
			}
		}
		if !removed {
			t.Errorf("%s: the model's rm -r of the root left the part's canaries", w.iso)
		}
	}
}

// gVisor's VFS refuses a root's rmdir as not empty and the rename of the
// root that holds the part with ENOENT, before either is found busy, as a
// real run showed. With the mount there and unchanged before and after
// each attempt, both are the refusal R3 and R4 owe.
func TestR3UnderGVisorsOrder(t *testing.T) {
	for _, agentWrites := range []bool{false, true} {
		f := world(t, isoGvisor)
		f.SetRunsAsRoot(!agentWrites)
		res, col := runWith(f, f, isoGvisor, byID(t, "R3"), byID(t, "R4"))
		for _, id := range []string{"R3", "R4"} {
			if st, why := status(res, id); st != c.Pass {
				t.Errorf("agent-writes=%v: %s %s %s (%v)", agentWrites, id, st, why, col.Lines(id))
			}
		}
		u := "root"
		if agentWrites {
			u = "agent"
		}
		lines := strings.Join(col.Lines("R3"), "\n")
		for _, want := range []string{
			u + "/mv-ro: PASS",
			u + "/rmdir-ro: refused as enotempty: rmdir: failed to remove '/work/conf-0a1b2c3d-ro': Directory not empty, with the mount there and unchanged before and after",
			u + "/mv-dev: refused as enoent: mv: cannot move '/work/conf-0a1b2c3d-dev' to '/work/conf-0a1b2c3d-dev.moved': No such file or directory, with the mount there",
			u + "/rmdir-dev: refused as enotempty",
		} {
			if !strings.Contains(lines, want) {
				t.Errorf("agent-writes=%v: no line %q in\n%s", agentWrites, want, lines)
			}
		}
		if !strings.Contains(strings.Join(col.Lines("R4"), "\n"), u+"/rmdir-part: refused as enotempty") {
			t.Errorf("R4 lines: %v", col.Lines("R4"))
		}
	}
}

// A root that is no mount point is refused an rmdir as not empty too:
// that alone is no refusal of the mount's, and R3 fails it.
func TestR3NotAMountFails(t *testing.T) {
	for _, iso := range []string{isoContainer, isoGvisor, isoVM} {
		f := world(t, iso)
		f.Break(fake.RootsNotMounts)
		res, _ := runWith(f, f, iso, byID(t, "R3"))
		if st, why := status(res, "R3"); st != c.Fail || !strings.Contains(why, "is not a mount point in the sandbox") {
			t.Errorf("%s: R3 %s %s", iso, st, why)
		}
	}
}

// An attempt the sandbox says was refused, after which the root's mount
// is gone or another, is a FAIL, whatever the error: the mount table is
// read after it as well as before.
func TestR3MountGoneAfterTheAttempt(t *testing.T) {
	for _, tc := range []struct{ name, table string }{
		{"gone", "1 0 0:1 / / rw - overlay overlay rw"},
		{"covered", "1 0 0:1 / / rw - overlay overlay rw\n20 1 0:40 / /work/conf-0a1b2c3d-ro ro,relatime - fakefs fake:roots/conf-0a1b2c3d-ro rw\n90 20 0:9 / /work/conf-0a1b2c3d-ro rw - tmpfs tmpfs rw"},
	} {
		tried := false
		lie := func(_, body string) (int, string, string, bool) {
			switch {
			case strings.HasPrefix(body, "rmdir /work/conf-0a1b2c3d-ro"):
				tried = true
				return 0, "\nrc=1\n", "rmdir: failed to remove '/work/conf-0a1b2c3d-ro': Directory not empty", true
			case tried && body == "head -n 2000 /proc/self/mountinfo 1>&2":
				return 0, "\nrc=0\n", tc.table, true
			}
			return 0, "", "", false
		}
		f := world(t, isoGvisor)
		res, _ := runWith(f, &liar{f, lie}, isoGvisor, byID(t, "R3"))
		if st, why := status(res, "R3"); st != c.Fail || !strings.Contains(why, "after the attempt") {
			t.Errorf("%s: R3 %s %s", tc.name, st, why)
		}
	}
}

// Without the mount table there is nothing to judge an rmdir's "not
// empty" against: ERROR, never PASS.
func TestR3NoMountTableIsError(t *testing.T) {
	lie := func(_, body string) (int, string, string, bool) {
		if body == "head -n 2000 /proc/self/mountinfo 1>&2" {
			return 0, "\nrc=1\n", "head: /proc/self/mountinfo: Permission denied", true
		}
		return 0, "", "", false
	}
	f := world(t, isoGvisor)
	res, _ := runWith(f, &liar{f, lie}, isoGvisor, byID(t, "R3"))
	if st, why := status(res, "R3"); st != c.Error || !strings.Contains(why, "mount table") {
		t.Errorf("R3 %s %s", st, why)
	}
}

// R10's userns extra: under container the namespace is refused (held);
// under gVisor it is allowed and the mount stays read-only (held); a
// mutant that lets the remount land and the write through is a FAIL.
func TestR10UserNS(t *testing.T) {
	for _, w := range sandboxWorlds {
		if w.iso == isoVM {
			continue
		}
		f := world(t, w.iso)
		f.SetRunsAsRoot(!w.agentWrites)
		res, col := runWith(f, f, w.iso, byID(t, "R10"))
		if st, why := status(res, "R10"); st != c.Pass {
			t.Errorf("%s agent-writes=%v: R10 %s %s (%v)", w.iso, w.agentWrites, st, why, col.Lines("R10"))
		}
		held := false
		for _, l := range col.Lines("R10") {
			held = held || strings.Contains(l, "/userns: held")
		}
		if !held {
			t.Errorf("%s: no userns line: %v", w.iso, col.Lines("R10"))
		}
	}
	// gVisor with the remount landing and the write going through: FAIL,
	// and the host's witness is what caught it.
	f := world(t, isoGvisor)
	f.Break(fake.UsernsRemountLands)
	res, col := runWith(f, f, isoGvisor, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Fail || !strings.Contains(why, "read-only canary") {
		t.Errorf("userns remount lands: R10 %s %s (%v)", st, why, col.Lines("R10"))
	}
	// gVisor with the namespaced unmount reaching the sandbox's own mounts
	// while the probe says held: FAIL, and the mount table is what caught it.
	f = world(t, isoGvisor)
	f.Break(fake.UsernsUnmountReachesOut)
	res, col = runWith(f, f, isoGvisor, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Fail || !strings.Contains(why, "mounts changed") {
		t.Errorf("userns unmount reaches out: R10 %s %s (%v)", st, why, col.Lines("R10"))
	}
	// gVisor refusing the namespace: nothing was tried, an ERROR, not held.
	f = world(t, isoGvisor)
	f.RefuseUserNS()
	res, col = runWith(f, f, isoGvisor, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Error || !strings.Contains(why, "not made under gVisor") {
		t.Errorf("userns refused under gVisor: R10 %s %s (%v)", st, why, col.Lines("R10"))
	}
}

// The userns extra needs the probe in place, and re-checks it was not
// swapped after the calls.
func TestR10UserNSNeedsTheProbe(t *testing.T) {
	f := fake.New("0a1b2c3d")
	f.SetIsolation(isoGvisor)
	specs, err := checks.Layout(paths(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if _, err := f.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := runWith(f, f, isoGvisor, byID(t, "R10"))
	if st, why := status(res, "R10"); st != c.Error || !strings.Contains(why, "probe") {
		t.Errorf("no probe: R10 %s %s", st, why)
	}
}
