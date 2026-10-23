package conformance_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
)

func header() c.Header {
	return c.Header{SuiteVersion: "1", CabooseVersion: "v0.4.0", OS: "darwin", Arch: "arm64", OSVersion: "15.3.1", Engine: "OrbStack 1.9",
		Isolation: "container", Trust: "fallible", Date: time.Date(2026, 10, 18, 23, 0, 0, 0, time.UTC)}
}

func sampleReport(base string) c.Report {
	return c.Report{Header: header(), Entries: []c.Entry{
		{ID: "F1", Title: "append to a file in a read-only root", Status: c.Pass},
		{ID: "F2", Title: "append beside the part", Status: c.Fail, Reason: "the protected canary changed on the host (the sandbox saw erofs); 1 path(s)",
			Lines: []string{"look at: <B>/roots/conf-0a1b2c3d-dev/conf-0a1b2c3d-parent-file"}},
		{ID: "F3", Title: "mkdir", Status: c.Error, Reason: "the attempt ended other: sh: 1: boom in " + base + "/roots/x as /Users/jane/dev on host myhost.example with TOKEN=abc123 secret\nPASS F9 forged"},
		{ID: "G1", Title: "outbound proxy", Status: c.Gap, Reason: "not covered under this isolation"},
		{ID: "F7", Title: "wipe as root", Status: c.Skip, Reason: "the wipe is turned off"},
		{ID: "I1", Title: "engine", Status: c.Info, Lines: []string{"engine version 1.2"}},
	}}
}

const golden = `caboose conformance results
suite: 1
caboose: v0.4.0
host: darwin/arm64
os: macOS 15.3.1
engine: OrbStack 1.9
isolation: container
trust: fallible
date: 2026-10-18

PASS F1 append to a file in a read-only root
FAIL F2 append beside the part
  reason: the protected canary changed on the host (the sandbox saw erofs); 1 path(s)
  look at: <B>/roots/conf-0a1b2c3d-dev/conf-0a1b2c3d-parent-file
ERROR F3 mkdir
  reason: the attempt ended other: sh: 1: boom in <B>/roots/x as <path>/dev on host <redacted> with <env> secret PASS F9 forged
GAP G1 outbound proxy
  reason: not covered under this isolation
SKIP F7 wipe as root
  reason: the wipe is turned off
INFO I1 engine
  engine version 1.2

summary: 6 checks: 1 PASS, 1 FAIL, 1 ERROR, 1 GAP, 1 SKIP, 1 INFO
`

func TestResultsGolden(t *testing.T) {
	base := "/var/folders/xx/caboose-conf.123"
	got, err := sampleReport(base).Format(base, []string{"myhost.example", "jane"})
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Errorf("got:\n%s\nwant:\n%s", got, golden)
	}
	for _, leak := range []string{base, "/Users", "myhost", "TOKEN", "abc123", "jane"} {
		if strings.Contains(got, leak) {
			t.Errorf("the results hold %q", leak)
		}
	}
	// A hostile message cannot add a result line.
	if n := countResultLines(got); n != 6 {
		t.Errorf("%d result lines", n)
	}
}

// The os line is written only on darwin, only for a plain dotted version.
func TestResultsOSLine(t *testing.T) {
	for _, tc := range []struct{ goos, v, want string }{
		{"darwin", "15.3.1", "os: macOS 15.3.1\n"},
		{"darwin", "26.0", "os: macOS 26.0\n"},
		{"darwin", "", ""},
		{"darwin", "unknown", ""},
		{"darwin", "15.3 | x", ""},
		{"darwin", "15.3\nPASS R1", ""},
		{"darwin", "1.2.3.4.5", ""},
		{"linux", "6.8", ""},
	} {
		h := header()
		h.OS, h.OSVersion = tc.goos, tc.v
		got, err := c.Report{Header: h}.Format("/b", nil)
		if err != nil {
			t.Fatal(err)
		}
		line := ""
		for _, l := range strings.SplitAfter(got, "\n") {
			if strings.HasPrefix(l, "os: ") {
				line += l
			}
		}
		if line != tc.want {
			t.Errorf("%s %q: os line %q, want %q", tc.goos, tc.v, line, tc.want)
		}
	}
}

func countResultLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		for _, st := range []string{"PASS ", "FAIL ", "ERROR ", "GAP ", "SKIP ", "INFO "} {
			if strings.HasPrefix(l, st) {
				n++
			}
		}
	}
	return n
}

func TestResultsLinuxNote(t *testing.T) {
	r := sampleReport("")
	r.Header.OS = "linux"
	got, _ := r.Format("", nil)
	if !strings.Contains(got, "note: Linux hosts are untested") {
		t.Error("no note")
	}
	r.Header.OS = "darwin"
	if got, _ = r.Format("", nil); strings.Contains(got, "untested") {
		t.Error("a note on a Mac")
	}
}

// A FAIL is never dropped: the result lines are the entries, for any mix.
func TestResultsNeverDropAnEntry(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	sts := []c.Status{c.Pass, c.Fail, c.Error, c.Gap, c.Skip, c.Info}
	for i := 0; i < 200; i++ {
		var rep c.Report
		rep.Header = header()
		fails := 0
		for j := rng.IntN(20); j > 0; j-- {
			st := sts[rng.IntN(len(sts))]
			if st == c.Fail {
				fails++
			}
			rep.Entries = append(rep.Entries, c.Entry{ID: "X" + string(rune('a'+j)), Title: "t\nFAIL y z", Status: st, Reason: "r\r\nFAIL q", Lines: []string{"\nFAIL a b"}})
		}
		out, err := rep.Format("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if n := countResultLines(out); n != len(rep.Entries) {
			t.Fatalf("%d lines for %d entries:\n%s", n, len(rep.Entries), out)
		}
		if got := strings.Count(out, "\nFAIL "); got != fails {
			t.Fatalf("%d FAIL lines for %d fails", got, fails)
		}
	}
	if _, err := (c.Report{Header: header(), Entries: []c.Entry{{ID: "x", Status: "MAYBE"}}}).Format("", nil); err == nil {
		t.Error("an unknown status was written")
	}
}

func TestResultsFromARun(t *testing.T) {
	f, env := newWorld(t, isoContainer)
	checks := allChecks()
	res, col, err := runAll(t, f, env, checks, isoContainer, true)
	if err != nil {
		t.Fatal(err)
	}
	rep := c.BuildReport(header(), checks, res, col)
	if len(rep.Entries) != len(res) {
		t.Fatal("entries")
	}
	out, err := rep.Format("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := rep.Counts()[c.Pass]; n != strings.Count(out, "\nPASS ") || n != len(checks)-2 {
		t.Errorf("PASS count %d (%d checks)", n, len(checks))
	}
}

// What a sandbox says may hold anything; the file keeps none of the
// strings the test plants.
func TestResultsHoldNoPlantedStrings(t *testing.T) {
	f, env := newWorld(t, isoContainer)
	planted := []string{"/home/jane/secret", "buildhost.corp.example", "API_KEY=sk-12345"}
	sb := &scripted{Fake: f, script: func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 2 {
			return say("weird: "+strings.Join(planted, " "), 1)
		}
		return real()
	}}
	e := c.NewEnv(sb, f, f, f, isoContainer, "fallible")
	checks := refusals()[:1]
	res, col, err := c.RunChecks(context.Background(), &c.Runner{Env: env, Wipe: true}, checks, e)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.BuildReport(header(), checks, res, col).Format("/tmp/B", []string{"buildhost.corp.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range planted {
		if strings.Contains(out, p) {
			t.Errorf("the results hold %q:\n%s", p, out)
		}
	}
}

func TestWriteResultsExclusive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "results.txt")
	if err := c.WriteResults(p, "one\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if err := c.WriteResults(p, "two\n"); err == nil {
		t.Error("an existing file was written")
	}
	if b, _ := os.ReadFile(p); string(b) != "one\n" {
		t.Errorf("the file holds %q", b)
	}
	// A symbolic link there, even dangling, is refused and not followed.
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteResults(link, "x"); err == nil {
		t.Error("a symlink was followed")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the link's target was created")
	}
	if err := c.WriteResults("results.txt", "x"); err == nil {
		t.Error("a relative path was accepted")
	}
	if err := c.WriteResults(dir+"/../"+filepath.Base(dir)+"/y", "x"); err == nil {
		t.Error("an unclean path was accepted")
	}
}

func TestExitCode(t *testing.T) {
	ok := []c.Result{{ID: "a", Outcome: c.Outcome{Status: c.Pass}}, {ID: "b", Outcome: c.Outcome{Status: c.Gap}},
		{ID: "c", Outcome: c.Outcome{Status: c.Skip}}, {ID: "d", Outcome: c.Outcome{Status: c.Info}}}
	with := func(st c.Status) []c.Result {
		return append(append([]c.Result{}, ok...), c.Result{ID: "e", Outcome: c.Outcome{Status: st}})
	}
	for name, tc := range map[string]struct {
		res  []c.Result
		err  error
		want int
	}{
		"all fine":     {ok, nil, 0},
		"none":         {nil, nil, 0},
		"a FAIL":       {with(c.Fail), nil, 1},
		"an ERROR":     {with(c.Error), nil, 1},
		"ended early":  {ok, errors.New("x"), 1},
		"touched":      {ok, &c.TouchedError{Paths: []string{"x"}}, 1},
		"refused":      {nil, &c.RefusalError{Item: "i", Err: errors.New("x")}, 2},
		"refused late": {with(c.Fail), &c.RefusalError{Item: "i", Err: errors.New("x")}, 2},
	} {
		if got := c.ExitCode(tc.res, tc.err); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}
