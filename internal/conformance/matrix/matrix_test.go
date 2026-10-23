package matrix_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/conformance/matrix"
)

func hdr(iso, trust string, day int) c.Header {
	return c.Header{SuiteVersion: "1", CabooseVersion: "v0.4.0", OS: "darwin", Arch: "arm64", OSVersion: "15.3.1", Engine: "OrbStack",
		Isolation: iso, Trust: trust, Date: time.Date(2026, 10, day, 23, 0, 0, 0, time.UTC)}
}

func format(t *testing.T, h c.Header, es ...c.Entry) string {
	t.Helper()
	s, err := c.Report{Header: h, Entries: es}.Format("/b", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func entry(id string, st c.Status) c.Entry { return c.Entry{ID: id, Title: "t " + id, Status: st} }

func TestParseRoundTrip(t *testing.T) {
	txt := format(t, hdr("vm", "hostile", 18), entry("R1", c.Pass), entry("R2", c.Fail), entry("E8", c.Gap), entry("D4", c.Skip), entry("D0", c.Info))
	r, err := matrix.ParseResults([]byte(txt))
	if err != nil {
		t.Fatal(err)
	}
	if r.Column() != "vm/hostile" || r.Date.Format("2006-01-02") != "2026-10-18" || r.Host != "darwin/arm64" || r.OS != "macOS 15.3.1" || r.Caboose != "v0.4.0" || len(r.Entries) != 5 {
		t.Fatalf("%+v", r)
	}
	if r.Entries[1].Status != c.Fail || r.Entries[1].ID != "R2" {
		t.Fatalf("%+v", r.Entries[1])
	}
	lin := format(t, c.Header{SuiteVersion: "1", CabooseVersion: "v", OS: "linux", Arch: "amd64", Engine: "e", Isolation: "gvisor", Trust: "hostile", Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}, entry("T1", c.Pass))
	if r, err = matrix.ParseResults([]byte(lin)); err != nil || r.Column() != "gvisor" || r.OS != "" {
		t.Fatalf("%v %+v", err, r)
	}
}

// A results file written before the os line existed still parses, and
// its cells name the host's OS/ARCH.
func TestParseOldFile(t *testing.T) {
	old := `caboose conformance results
suite: 1
caboose: v0.4.0
host: darwin/arm64
engine: OrbStack
isolation: container
trust: fallible
date: 2026-10-18

PASS R1 t R1
  look at: nothing

summary: 1 checks: 1 PASS
`
	r, err := matrix.ParseResults([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if r.OS != "" || r.Host != "darwin/arm64" || r.Engine != "OrbStack" || len(r.Entries) != 1 {
		t.Fatalf("%+v", r)
	}
	d, err := matrix.Merge(oneRow(matrix.Cell{Claim: matrix.Held}, "R1"), r, suiteOf("R1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cell(t, d, suiteOf("R1"), "container"); got != "held (tested 2026-10-18 on darwin/arm64 (OrbStack), caboose v0.4.0: `R1`)" {
		t.Error(got)
	}
	// And a data file whose stored results have no os loads.
	b, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"os"`) {
		t.Fatalf("an empty os is written: %s", b)
	}
	if _, err := matrix.ParseData(b); err != nil {
		t.Fatal(err)
	}
	// The same run with the os line names macOS.
	r.OS = "macOS 15.3.1"
	d, err = matrix.Merge(oneRow(matrix.Cell{Claim: matrix.Held}, "R1"), r, suiteOf("R1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cell(t, d, suiteOf("R1"), "container"); got != "held (tested 2026-10-18 on macOS 15.3.1 (OrbStack), caboose v0.4.0: `R1`)" {
		t.Error(got)
	}
	if b, _ := d.Marshal(); !strings.Contains(string(b), `"os": "macOS 15.3.1"`) {
		t.Fatalf("os not stored: %s", b)
	}
}

func TestParseRefusals(t *testing.T) {
	good := format(t, hdr("container", "fallible", 18), entry("R1", c.Pass), entry("R2", c.Fail))
	rep := func(old, nw string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("no %q", old)
		}
		return strings.Replace(good, old, nw, 1)
	}
	cases := map[string]string{
		"unknown status":     rep("PASS R1", "MAYBE R1"),
		"missing header":     rep("engine: OrbStack\n", ""),
		"bad isolation":      rep("isolation: container", "isolation: docker"),
		"bad trust":          rep("trust: fallible", "trust: lax"),
		"bad date":           rep("date: 2026-10-18", "date: 18/10/2026"),
		"date with time":     rep("date: 2026-10-18", "date: 2026-10-18T10:00:00Z"),
		"duplicate id":       rep("FAIL R2", "PASS R1"),
		"summary mismatch":   rep("1 PASS, 1 FAIL", "2 PASS"),
		"summary total":      rep("summary: 2 checks", "summary: 3 checks"),
		"no summary":         good[:strings.Index(good, "summary:")],
		"too big":            good + strings.Repeat(" ", matrix.MaxResultsSize),
		"not a results file": "hello\n",
		"host":               rep("host: darwin/arm64", "host: darwin"),
		"os not macOS":       rep("os: macOS 15.3.1", "os: Windows 11"),
		"os empty":           rep("os: macOS 15.3.1", "os: "),
		"os out of place":    rep("os: macOS 15.3.1\nengine: OrbStack", "engine: OrbStack\nos: macOS 15.3.1"),
		"junk after summary": good + "PASS R9 x\n",
	}
	for name, in := range cases {
		if _, err := matrix.ParseResults([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func suiteOf(ids ...string) []c.Check {
	var out []c.Check
	for _, id := range ids {
		out = append(out, c.Check{ID: id})
	}
	return out
}

func parse(t *testing.T, h c.Header, es ...c.Entry) matrix.Results {
	t.Helper()
	r, err := matrix.ParseResults([]byte(format(t, h, es...)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMerge(t *testing.T) {
	suite := suiteOf("R1", "R2")
	d := matrix.Data{Version: 1}
	d, err := matrix.Merge(d, parse(t, hdr("container", "fallible", 18), entry("R1", c.Pass), entry("R2", c.Pass)), suite)
	if err != nil {
		t.Fatal(err)
	}
	// Older does not replace.
	d, _ = matrix.Merge(d, parse(t, hdr("container", "hostile", 17), entry("R1", c.Fail)), suite)
	if d.Results["container"]["R1"].Status != c.Pass {
		t.Fatal("older replaced newer")
	}
	// Newer replaces.
	d, _ = matrix.Merge(d, parse(t, hdr("container", "fallible", 19), entry("R1", c.Fail)), suite)
	if r := d.Results["container"]["R1"]; r.Status != c.Fail || r.Date != "2026-10-19" {
		t.Fatalf("%+v", r)
	}
	// Equal date: the later file wins.
	d, _ = matrix.Merge(d, parse(t, hdr("container", "fallible", 19), entry("R1", c.Gap)), suite)
	if d.Results["container"]["R1"].Status != c.Gap {
		t.Fatal("equal date did not replace")
	}
	// Info is stored, as a check that ran; vm is per trust.
	d, _ = matrix.Merge(d, parse(t, hdr("vm", "fallible", 19), entry("R2", c.Info)), suite)
	if r := d.Results["vm/fallible"]["R2"]; len(d.Results["vm/fallible"]) != 1 || r.Status != c.Info {
		t.Fatalf("info not stored as run: %+v", d.Results["vm/fallible"])
	}
	// Unknown id refused, and the input is untouched.
	before := d
	if _, err := matrix.Merge(d, parse(t, hdr("container", "fallible", 19), entry("Z9", c.Pass)), suite); err == nil {
		t.Fatal("unknown id accepted")
	}
	if matrix.Diff(before, d) != nil && len(matrix.Diff(before, d)) != 0 {
		t.Fatal("changed")
	}
}

func chk(id string, iso, trust []string) c.Check {
	return c.Check{ID: id, Isolations: iso, Trusts: trust}
}

func oneRow(claim matrix.Cell, ids ...string) matrix.Data {
	cells := map[string]matrix.Cell{}
	for _, col := range matrix.Columns {
		cells[col] = claim
	}
	return matrix.Data{Version: 1, Controls: []matrix.Control{{Control: "x", Checks: ids, Cells: cells}}}
}

func res(st c.Status, day string) matrix.Result {
	return matrix.Result{Status: st, Date: day, Host: "darwin/arm64", Caboose: "v1", Suite: "1", Engine: "e"}
}

func cell(t *testing.T, d matrix.Data, suite []c.Check, col string) string {
	t.Helper()
	// A row cites only some of the suite: add a row for the rest, as a
	// real data file must.
	used := map[string]bool{}
	for _, id := range d.Controls[0].Checks {
		used[id] = true
	}
	rest := d.Controls[0]
	rest.Control, rest.Checks = "rest", nil
	for _, ck := range suite {
		if !used[ck.ID] {
			rest.Checks = append(rest.Checks, ck.ID)
		}
	}
	if len(rest.Checks) > 0 {
		d.Controls = append(append([]matrix.Control{}, d.Controls...), rest)
	}
	out, err := matrix.Render(d, suite)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(strings.TrimSpace(out), "\n")[2]
	parts := strings.Split(line, " | ")
	for i, cn := range matrix.Columns {
		if cn == col {
			return strings.TrimSuffix(strings.TrimPrefix(parts[i+1], ""), " |")
		}
	}
	t.Fatal("no column")
	return ""
}

func TestRenderCells(t *testing.T) {
	suite := []c.Check{chk("R1", nil, nil), chk("R2", nil, nil), chk("R3", nil, nil), chk("V1", []string{"vm"}, []string{"hostile"})}
	held := matrix.Cell{Claim: matrix.Held}
	withRes := func(d matrix.Data, col string, m map[string]matrix.Result) matrix.Data {
		d.Results = map[string]map[string]matrix.Result{col: m}
		return d
	}
	d := oneRow(held, "R1", "R2")
	if got := cell(t, d, suite, "container"); got != "held (not tested)" {
		t.Error(got)
	}
	// No applicable check.
	if got := cell(t, oneRow(held, "V1"), suite, "container"); got != "held (no check)" {
		t.Error(got)
	}
	// Applies by trust: V1 is vm hostile only.
	if got := cell(t, oneRow(held, "V1"), suite, "vm/hostile"); got != "held (not tested)" {
		t.Error(got)
	}
	if got := cell(t, oneRow(held, "V1"), suite, "vm/fallible"); got != "held (no check)" {
		t.Error(got)
	}
	// n/a.
	if got := cell(t, oneRow(matrix.Cell{Claim: matrix.NA}, "R1"), suite, "gvisor"); got != "n/a" {
		t.Error(got)
	}
	// All pass.
	all := withRes(d, "container", map[string]matrix.Result{"R1": res(c.Pass, "2026-10-18"), "R2": res(c.Pass, "2026-10-18")})
	if got := cell(t, all, suite, "container"); got != "held (tested 2026-10-18 on darwin/arm64 (e), caboose v1: `R1`, `R2`)" {
		t.Error(got)
	}
	// Gap and skip.
	gs := withRes(oneRow(held, "R1", "R2", "R3"), "container", map[string]matrix.Result{"R1": res(c.Pass, "2026-10-18"), "R2": res(c.Gap, "2026-10-18"), "R3": res(c.Skip, "2026-10-18")})
	if got := cell(t, gs, suite, "container"); got != "held (tested 2026-10-18 on darwin/arm64 (e), caboose v1: `R1`; gap: `R2`; skipped: `R3`)" {
		t.Error(got)
	}
	// Fail beats the claim.
	f := withRes(oneRow(matrix.Cell{Claim: matrix.NotCovered}, "R1", "R2"), "container", map[string]matrix.Result{"R1": res(c.Fail, "2026-10-18"), "R2": res(c.Pass, "2026-10-18")})
	if got := cell(t, f, suite, "container"); got != "FAILED: `R1` (2026-10-18, darwin/arm64 (e), caboose v1)" {
		t.Error(got)
	}
	// Error and missing.
	e := withRes(oneRow(held, "R1", "R2", "R3"), "container", map[string]matrix.Result{"R1": res(c.Error, "2026-10-18"), "R2": res(c.Pass, "2026-10-18")})
	if got := cell(t, e, suite, "container"); got != "held (not tested: `R1` error, `R3` not run)" {
		t.Error(got)
	}
	// Missing alone.
	m := withRes(d, "container", map[string]matrix.Result{"R1": res(c.Pass, "2026-10-18")})
	if got := cell(t, m, suite, "container"); got != "held (not tested: `R2` not run)" {
		t.Error(got)
	}
	// Two runs.
	two := withRes(d, "container", map[string]matrix.Result{"R1": res(c.Pass, "2026-10-18"), "R2": res(c.Pass, "2026-10-20")})
	if got := cell(t, two, suite, "container"); got != "held (tested 2026-10-18 on darwin/arm64 (e), caboose v1: `R1`) (tested 2026-10-20 on darwin/arm64 (e), caboose v1: `R2`)" {
		t.Error(got)
	}
	// The os line and the engine name the run: the same day on two
	// machines is two groups.
	mac := res(c.Pass, "2026-10-18")
	mac.OS = "macOS 15.3.1"
	other := res(c.Pass, "2026-10-18")
	other.OS, other.Engine = "macOS 26.0", "Docker Desktop 4.40"
	om := withRes(d, "container", map[string]matrix.Result{"R1": mac, "R2": other})
	if got := cell(t, om, suite, "container"); got != "held (tested 2026-10-18 on macOS 15.3.1 (e), caboose v1: `R1`) (tested 2026-10-18 on macOS 26.0 (Docker Desktop 4.40), caboose v1: `R2`)" {
		t.Error(got)
	}
	// Claim text.
	if got := cell(t, oneRow(matrix.Cell{Claim: matrix.Partial, Note: "only reads"}, "R1"), suite, "container"); got != "partial: only reads (not tested)" {
		t.Error(got)
	}
	if got := cell(t, oneRow(matrix.Cell{Claim: matrix.Held, Note: "same"}, "R1"), suite, "container"); got != "held, same (not tested)" {
		t.Error(got)
	}
}

// A results file whose check of a row recorded INFO (G7, a
// window) leaves the row tested, the INFO listed, never "not run".
func TestInfoCountsAsRun(t *testing.T) {
	suite := suiteOf("G1", "G7")
	d := oneRow(matrix.Cell{Claim: matrix.Held}, "G1", "G7")
	d, err := matrix.Merge(d, parse(t, hdr("container", "fallible", 18), entry("G1", c.Pass), entry("G7", c.Info)), suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate(d, suite); err != nil {
		t.Fatal(err)
	}
	b, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if d, err = matrix.ParseData(b); err != nil {
		t.Fatalf("the stored INFO does not read back: %v", err)
	}
	got := cell(t, d, suite, "container")
	if strings.Contains(got, "not run") || strings.Contains(got, "not tested") || !strings.HasPrefix(got, "held (tested 2026-10-18 ") || !strings.HasSuffix(got, ": `G1`; info: `G7`)") {
		t.Errorf("%q", got)
	}
	// An INFO alone proves nothing held: not tested, and why.
	d = oneRow(matrix.Cell{Claim: matrix.Held}, "G7")
	d, _ = matrix.Merge(d, parse(t, hdr("container", "fallible", 18), entry("G7", c.Info)), suiteOf("G7"))
	if got := cell(t, d, suiteOf("G7"), "container"); got != "held (not tested: `G7` info)" {
		t.Errorf("%q", got)
	}
}

func TestValidate(t *testing.T) {
	suite := suiteOf("R1", "R2")
	ok := oneRow(matrix.Cell{Claim: matrix.Held}, "R1", "R2")
	if err := matrix.Validate(ok, suite); err != nil {
		t.Fatal(err)
	}
	bad := map[string]matrix.Data{
		"unknown id":    oneRow(matrix.Cell{Claim: matrix.Held}, "R1", "R2", "Z1"),
		"uncovered":     oneRow(matrix.Cell{Claim: matrix.Held}, "R1"),
		"claim":         oneRow(matrix.Cell{Claim: "in effect"}, "R1", "R2"),
		"partial":       oneRow(matrix.Cell{Claim: matrix.Partial}, "R1", "R2"),
		"duplicate row": oneRow(matrix.Cell{Claim: matrix.Held}, "R1", "R2", "R1"),
		"bar":           oneRow(matrix.Cell{Claim: matrix.Held, Note: "a|b"}, "R1", "R2"),
	}
	missing := oneRow(matrix.Cell{Claim: matrix.Held}, "R1", "R2")
	delete(missing.Controls[0].Cells, "gvisor")
	bad["missing cell"] = missing
	for name, d := range bad {
		if matrix.Validate(d, suite) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReplaceTable(t *testing.T) {
	doc := "a\n" + matrix.Begin + "\nold\nold2\n" + matrix.End + "\nz\n"
	got, err := matrix.ReplaceTable(doc, "T\n")
	if err != nil || got != "a\n"+matrix.Begin+"\nT\n"+matrix.End+"\nz\n" {
		t.Fatalf("%q %v", got, err)
	}
	for name, d := range map[string]string{
		"none":  "a\n",
		"two":   doc + doc,
		"order": matrix.End + "\n" + matrix.Begin + "\n",
		"half":  matrix.Begin + "\n",
	} {
		if _, err := matrix.ReplaceTable(d, "T\n"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// repoFile reads a file of the checkout, two levels above this package's
// parent.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRepoMatrix(t *testing.T) {
	d, err := matrix.ParseData(repoFile(t, "docs/security-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	suite := checks.Every()
	if err := matrix.Validate(d, suite); err != nil {
		t.Fatal(err)
	}
	table, err := matrix.Render(d, suite)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(repoFile(t, "docs/security.md"))
	want, err := matrix.ReplaceTable(doc, table)
	if err != nil {
		t.Fatal(err)
	}
	if want != doc {
		t.Fatal("docs/security.md's matrix differs from Render of docs/security-matrix.json; run caboose conformance matrix")
	}
}

func TestHeaderTextIsConfined(t *testing.T) {
	good := format(t, hdr("container", "fallible", 18), entry("R1", c.Pass))
	for _, rep := range [][2]string{
		{"caboose: v0.4.0", "caboose: v1 | x | [y](javascript:z)"},
		{"host: darwin/arm64", "host: dar|win/arm64"},
		{"suite: 1", "suite: `1`"},
		{"engine: OrbStack", "engine: <b>Orb</b>"},
		{"os: macOS 15.3.1", "os: macOS 15.3 | x"},
		{"os: macOS 15.3.1", "os: macOS [15](x)"},
		{"caboose: v0.4.0", "caboose: " + strings.Repeat("v", 65)},
	} {
		if _, err := matrix.ParseResults([]byte(strings.Replace(good, rep[0], rep[1], 1))); err == nil {
			t.Errorf("accepted %q", rep[1])
		}
	}
}

func TestStoredResultsAreChecked(t *testing.T) {
	suite := suiteOf("R1")
	for name, mut := range map[string]func(*matrix.Result){
		"status":  func(r *matrix.Result) { r.Status = "BOGUS" },
		"host":    func(r *matrix.Result) { r.Host = "a|b/c" },
		"caboose": func(r *matrix.Result) { r.Caboose = "v1 | x" },
		"suite":   func(r *matrix.Result) { r.Suite = "[x](y)" },
		"engine":  func(r *matrix.Result) { r.Engine = "a|b" },
		"os":      func(r *matrix.Result) { r.OS = "macOS 15 | x" },
		"date":    func(r *matrix.Result) { r.Date = "2026-13-45" },
	} {
		d := oneRow(matrix.Cell{Claim: matrix.Held}, "R1")
		r := res(c.Pass, "2026-10-18")
		r.Engine = "e"
		mut(&r)
		d.Results = map[string]map[string]matrix.Result{"container": {"R1": r}}
		if matrix.Validate(d, suite) == nil {
			t.Errorf("%s: Validate accepted", name)
		}
		b, _ := d.Marshal()
		if _, err := matrix.ParseData(b); err == nil {
			t.Errorf("%s: ParseData accepted", name)
		}
	}
}

func TestRenderRefusesInvalidData(t *testing.T) {
	d := oneRow(matrix.Cell{Claim: "in effect"}, "R1")
	if _, err := matrix.Render(d, suiteOf("R1")); err == nil {
		t.Fatal("Render did not validate")
	}
}

func TestMergeKeepsOnlyTheRelease(t *testing.T) {
	for v, want := range map[string]string{
		"v0.3.2-12-g0123abc":      "v0.3.2+dev",
		"v0.3.2-1-gabcdef0-dirty": "v0.3.2+dev",
		"v0.4.0":                  "v0.4.0",
		"dev":                     "dev",
	} {
		h := hdr("vm", "hostile", 18)
		h.CabooseVersion = v
		d, err := matrix.Merge(oneRow(matrix.Cell{Claim: matrix.Held}, "R1"), parse(t, h, entry("R1", c.Pass)), suiteOf("R1"))
		if err != nil {
			t.Fatal(v, err)
		}
		if got := d.Results["vm/hostile"]["R1"].Caboose; got != want {
			t.Errorf("%s: stored %q, want %q", v, got, want)
		}
	}
}
