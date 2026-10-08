package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// checkerEnv is a proposalChecker on a temp data dir, with a fake
// configuration and a fake apkobuild.Check: each package is 1 MB, and
// "graphvis" is unknown.
type checkerEnv struct {
	t     *testing.T
	dd    string
	c     *proposalChecker
	image config.ImageProfile
	specs []apkobuild.Spec
}

func newCheckerEnv(t *testing.T) *checkerEnv {
	e := &checkerEnv{t: t, dd: t.TempDir(), image: config.ImageProfile{Kind: config.ImageKindApko, Name: "default", Packages: []string{"yq"}}}
	e.c = &proposalChecker{
		dataDir: e.dd,
		load:    func() (*config.Config, error) { return &config.Config{ImageProfile: e.image}, nil },
		check: func(_ context.Context, s apkobuild.Spec, o apkobuild.Options) (apkobuild.CheckResult, error) {
			if o.Arch != "x86_64" {
				t.Errorf("checked for %q", o.Arch)
			}
			e.specs = append(e.specs, s)
			for _, p := range s.Packages {
				if p == "graphvis" {
					return apkobuild.CheckResult{}, &apkobuild.ResolutionError{Err: errors.New(`no package named "graphvis" (did you mean graphviz?)`)}
				}
			}
			l, _ := s.List()
			return apkobuild.CheckResult{Packages: len(l), InstalledBytes: int64(len(l)) * 1_000_000}, nil
		},
		opts:    func(*config.Config) apkobuild.Options { return apkobuild.Options{Arch: "x86_64"} },
		timeout: time.Minute,
		logf:    func(string, ...any) {},
	}
	return e
}

func (e *checkerEnv) write(file, body string, mod time.Time) {
	e.t.Helper()
	writeProposalAt(e.t, e.dd, file, body, mod)
}

// polls polls twice: a file is checked once it holds still.
func (e *checkerEnv) polls() {
	e.c.poll(context.Background())
	e.c.poll(context.Background())
}

func (e *checkerEnv) checkFile(file string) string {
	b, err := os.ReadFile(filepath.Join(e.dd, proposal.Dir, proposal.CheckFile(file)))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestProposalCheckWritesTheOutcome(t *testing.T) {
	e := newCheckerEnv(t)
	t0 := time.Unix(1_000_000, 0)
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\", \"ruby\"]\n", t0)
	e.c.poll(context.Background())
	if len(e.specs) != 0 {
		t.Fatal("checked on the first poll that saw it")
	}
	e.c.poll(context.Background())
	base, _ := e.image.Spec().List()
	want := "ok\nresolves to " + strconv.Itoa(len(base)+2) + " packages, " + sizeString(int64(len(base)+2)*1_000_000) + " installed (+2 packages, +2.0 MB)\n"
	if got := e.checkFile("dot.toml"); got != want {
		t.Errorf("check file %q, want %q", got, want)
	}
	// The proposal's spec, then the profile's as it stands, for the deltas.
	if len(e.specs) != 2 || strings.Join(e.specs[0].Packages, " ") != "yq graphviz ruby" || strings.Join(e.specs[1].Packages, " ") != "yq" {
		t.Errorf("specs %+v", e.specs)
	}

	// Polled again, rewritten with the same contents: not checked again.
	e.polls()
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\", \"ruby\"]\n", t0.Add(time.Minute))
	e.polls()
	if len(e.specs) != 2 {
		t.Errorf("checked the same contents again: %d checks", len(e.specs))
	}
	// Other contents: checked again, the current spec's check remembered.
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphvis\"]\n", t0.Add(2*time.Minute))
	e.polls()
	if len(e.specs) != 3 {
		t.Errorf("%d checks", len(e.specs))
	}
	if got := e.checkFile("dot.toml"); got != "error\nno package named \"graphvis\" (did you mean graphviz?)\n" {
		t.Errorf("check file %q", got)
	}
}

func TestProposalCheckRefusals(t *testing.T) {
	e := newCheckerEnv(t)
	t0 := time.Unix(1_000_000, 0)
	e.write("bad.toml", "title = \"t\"\n[packages]\nadd = [\"Bad\"]\n", t0)
	e.write("own.toml", "title = \"t\"\n[packages]\nadd = [\"yq\"]\n", t0)
	e.write("root.toml", rootBody("Mount"), t0)
	if err := os.WriteFile(filepath.Join(e.dd, proposal.Dir, "root.check"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Older than the proposal it is beside: from a version before.
	if err := os.Chtimes(filepath.Join(e.dd, proposal.Dir, "root.check"), t0.Add(-time.Hour), t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.polls()
	if got := e.checkFile("bad.toml"); !strings.HasPrefix(got, "error\ncaboose apply would refuse this proposal: packages.add: package name \"Bad\"") {
		t.Errorf("bad: %q", got)
	}
	if got := e.checkFile("own.toml"); got != "error\nAlready installed: yq (this profile's packages).\n" {
		t.Errorf("own: %q", got)
	}
	if got := e.checkFile("root.toml"); got != "" {
		t.Errorf("a proposal with no packages has a check file: %q", got)
	}
	if len(e.specs) != 0 {
		t.Errorf("checked %+v", e.specs)
	}

	// Under another kind of image, a [packages] proposal is refused.
	e = newCheckerEnv(t)
	e.image = config.ImageProfile{Kind: config.ImageKindDockerfile, Name: "default"}
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n", t0)
	e.polls()
	if got := e.checkFile("dot.toml"); !strings.HasPrefix(got, "error\nThis environment's image is dockerfile.default") || !strings.Contains(got, "a [section] is what applies") {
		t.Errorf("dockerfile: %q", got)
	}
}

// The check file is the sandbox's to replace too: a symlink there is
// replaced, never written through.
func TestProposalCheckDoesNotFollowSymlinks(t *testing.T) {
	e := newCheckerEnv(t)
	t0 := time.Unix(1_000_000, 0)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n", t0)
	if err := os.Symlink(target, filepath.Join(e.dd, proposal.Dir, "dot.check")); err != nil {
		t.Fatal(err)
	}
	e.polls()
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("wrote through the symlink: %q", got)
	}
	fi, err := os.Lstat(filepath.Join(e.dd, proposal.Dir, "dot.check"))
	if err != nil || !fi.Mode().IsRegular() || !strings.HasPrefix(e.checkFile("dot.toml"), "ok\n") {
		t.Errorf("check file %v %v %q", fi, err, e.checkFile("dot.toml"))
	}
}

// A proposal with a check file newer than it when the link starts was
// checked by the link before: it is not checked again.
func TestProposalCheckSkipsWhatWasChecked(t *testing.T) {
	e := newCheckerEnv(t)
	t0 := time.Unix(1_000_000, 0)
	e.write("old.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n", t0)
	if err := proposal.WriteCheck(e.dd, "old.toml", proposal.CheckOK, []string{"from before"}); err != nil {
		t.Fatal(err)
	}
	e.write("new.toml", "title = \"t\"\n[packages]\nadd = [\"ruby\"]\n", t0)
	if err := proposal.WriteCheck(e.dd, "new.toml", proposal.CheckOK, []string{"from before"}); err != nil {
		t.Fatal(err)
	}
	// Older than its proposal: checked again.
	if err := os.Chtimes(filepath.Join(e.dd, proposal.Dir, "new.check"), t0.Add(-time.Hour), t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.polls()
	if got := e.checkFile("old.toml"); got != "ok\nfrom before\n" {
		t.Errorf("old checked again: %q", got)
	}
	if got := e.checkFile("new.toml"); !strings.HasPrefix(got, "ok\nresolves to") {
		t.Errorf("new not checked: %q", got)
	}
}

// A check that takes too long says so.
func TestProposalCheckTimesOut(t *testing.T) {
	e := newCheckerEnv(t)
	e.c.timeout = time.Millisecond
	e.c.check = func(ctx context.Context, _ apkobuild.Spec, _ apkobuild.Options) (apkobuild.CheckResult, error) {
		<-ctx.Done()
		return apkobuild.CheckResult{}, ctx.Err()
	}
	e.write("dot.toml", "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n", time.Unix(1_000_000, 0))
	e.polls()
	if got := e.checkFile("dot.toml"); !strings.HasPrefix(got, "unchecked\nchecking took longer than 1ms") {
		t.Errorf("check file %q", got)
	}
}

func TestSizeString(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{{0, "0 B"}, {999, "999 B"}, {1500, "1.5 kB"}, {210_000_000, "210 MB"}, {2_345_000_000, "2.3 GB"}, {-3_000_000, "-3.0 MB"}} {
		if got := sizeString(tc.n); got != tc.want {
			t.Errorf("sizeString(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	if got := signedSize(210_000_000); got != "+210 MB" {
		t.Errorf("signedSize = %q", got)
	}
}

// The sandbox decides how often it writes a proposal, so the link checks
// at most proposalCheckBudget in an hour, and leaves the rest to apply.
func TestProposalCheckBudget(t *testing.T) {
	e := newCheckerEnv(t)
	now := time.Unix(2_000_000, 0)
	e.c.now = func() time.Time { return now }
	t0 := time.Unix(1_000_000, 0)
	for i := range proposalCheckBudget + 1 {
		e.write("p.toml", "title = \"t\"\n[packages]\nadd = [\"pkg"+strconv.Itoa(i)+"\"]\n", t0.Add(time.Duration(i)*time.Minute))
		e.polls()
	}
	if got := e.checkFile("p.toml"); !strings.HasPrefix(got, "unchecked\nthe host has checked 30 proposals in the last hour") {
		t.Errorf("past the budget: %q", got)
	}
	checks := len(e.specs)
	// An hour later there is budget again.
	now = now.Add(time.Hour)
	e.write("p.toml", "title = \"t\"\n[packages]\nadd = [\"late\"]\n", t0.Add(time.Hour))
	e.polls()
	if len(e.specs) == checks || !strings.HasPrefix(e.checkFile("p.toml"), "ok\n") {
		t.Errorf("an hour later: %d checks, %q", len(e.specs)-checks, e.checkFile("p.toml"))
	}
}

// What kept the host from checking says unchecked, not error: nothing is
// known to be wrong with the proposal.
func TestProposalCheckUnchecked(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	body := "title = \"t\"\n[packages]\nadd = [\"graphviz\"]\n"

	e := newCheckerEnv(t)
	e.c.load = func() (*config.Config, error) { return nil, errors.New("config.toml: bad") }
	e.write("dot.toml", body, t0)
	e.polls()
	if got := e.checkFile("dot.toml"); !strings.HasPrefix(got, "unchecked\nthe host cannot read its configuration") {
		t.Errorf("configuration unreadable: %q", got)
	}

	e = newCheckerEnv(t)
	e.c.check = func(context.Context, apkobuild.Spec, apkobuild.Options) (apkobuild.CheckResult, error) {
		return apkobuild.CheckResult{}, errors.New("reading the repository indexes: connection refused")
	}
	e.write("dot.toml", body, t0)
	e.polls()
	if got := e.checkFile("dot.toml"); !strings.HasPrefix(got, "unchecked\nthe host could not check this now (reading the repository indexes: connection refused)") {
		t.Errorf("index unreachable: %q", got)
	}
}

// writeMany writes n proposals caboose apply would refuse, a minute apart
// from t0, named in the reverse of that order.
func (e *checkerEnv) writeMany(n int, t0 time.Time) []string {
	var files []string
	for i := range n {
		f := "p" + strconv.Itoa(1000-i) + ".toml"
		e.write(f, "not toml", t0.Add(time.Duration(i)*time.Minute))
		files = append(files, f)
	}
	return files
}

func (e *checkerEnv) checked(files []string) int {
	n := 0
	for _, f := range files {
		if e.checkFile(f) != "" {
			n++
		}
	}
	return n
}

// One poll reads at most proposalPollMax files, oldest first; the rest
// are left to the next.
func TestProposalCheckPollCap(t *testing.T) {
	e := newCheckerEnv(t)
	files := e.writeMany(proposalPollMax+6, time.Unix(1_000_000, 0))
	e.polls()
	if n := e.checked(files); n != proposalPollMax {
		t.Fatalf("%d checked in one poll, want %d", n, proposalPollMax)
	}
	for _, f := range files[proposalPollMax:] {
		if e.checkFile(f) != "" {
			t.Errorf("%s, among the newest, checked first", f)
		}
	}
	e.c.poll(context.Background())
	if n := e.checked(files); n != len(files) {
		t.Errorf("%d checked after the next poll, want %d", n, len(files))
	}
}

// Past proposalWriteBudget checks in an hour, the link checks none until
// the hour is over, and says so once.
func TestProposalCheckWriteBudget(t *testing.T) {
	e := newCheckerEnv(t)
	now := time.Unix(2_000_000, 0)
	e.c.now = func() time.Time { return now }
	var logged []string
	e.c.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	files := e.writeMany(proposalWriteBudget+5, time.Unix(1_000_000, 0))
	for range 10 {
		e.c.poll(context.Background())
	}
	if n := e.checked(files); n != proposalWriteBudget {
		t.Fatalf("%d checked, want %d", n, proposalWriteBudget)
	}
	paused := 0
	for _, l := range logged {
		if strings.Contains(l, "checking no more until the hour is over") {
			paused++
		}
	}
	if paused != 1 {
		t.Errorf("said it paused %d times:\n%s", paused, strings.Join(logged, "\n"))
	}
	now = now.Add(time.Hour)
	e.c.poll(context.Background())
	if n := e.checked(files); n != len(files) {
		t.Errorf("%d checked an hour later, want %d", n, len(files))
	}
}

// The checker logs at most checkLogMax lines a minute, then how many it
// did not.
func TestProposalCheckLogIsRateLimited(t *testing.T) {
	e := newCheckerEnv(t)
	now := time.Unix(2_000_000, 0)
	e.c.now = func() time.Time { return now }
	var logged []string
	e.c.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	e.writeMany(checkLogMax+10, time.Unix(1_000_000, 0))
	e.polls()
	if len(logged) != checkLogMax {
		t.Fatalf("logged %d lines in a minute, want %d", len(logged), checkLogMax)
	}
	now = now.Add(time.Minute)
	e.write("late.toml", "not toml", time.Unix(1_100_000, 0))
	e.polls()
	if len(logged) != checkLogMax+2 || logged[checkLogMax] != "... and 10 more lines about checking proposals not logged (at most 20 a minute)" {
		t.Errorf("after the minute:\n%s", strings.Join(logged[checkLogMax:], "\n"))
	}
}

// An error quoting a name the sandbox chose is logged escaped.
func TestProposalCheckLogsPrintably(t *testing.T) {
	e := newCheckerEnv(t)
	var logged []string
	e.c.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	e.write("x\x1b[2J.toml", "not toml", time.Unix(1_000_000, 0))
	if err := os.MkdirAll(filepath.Join(e.dd, proposal.Dir, "x\x1b[2J.check", "full"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.polls()
	all := strings.Join(logged, "\n")
	if !strings.Contains(all, "cannot write the check of") || strings.Contains(all, "\x1b") {
		t.Errorf("logged %q", all)
	}
}
