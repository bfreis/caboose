package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/conformance"
)

// Version is the data file's format version.
const Version = 1

// Columns are the matrix's columns, in order.
var Columns = []string{"container", "gvisor", "vm/fallible", "vm/hostile"}

var columnHeader = map[string]string{
	"container": "`container`", "gvisor": "`gvisor`",
	"vm/fallible": "`vm`, `fallible`", "vm/hostile": "`vm`, `hostile`",
}

// columnRuns are the isolation and trusts a column covers.
func columnRuns(col string) (string, []string) {
	switch col {
	case "container", "gvisor":
		return col, []string{"fallible", "hostile"}
	case "vm/fallible":
		return "vm", []string{"fallible"}
	default:
		return "vm", []string{"hostile"}
	}
}

// Claims.
const (
	Held       = "held"
	Partial    = "partial"
	NotCovered = "not covered"
	NA         = "n/a"
)

// Cell is what a control claims in a column. It is written by hand.
type Cell struct {
	Claim string `json:"claim"`
	Note  string `json:"note,omitempty"`
}

// Control is one row.
type Control struct {
	Control string          `json:"control"`
	Checks  []string        `json:"checks"`
	Cells   map[string]Cell `json:"cells"`
}

// Result is the latest verdict of one check in one column.
type Result struct {
	Status  conformance.Status `json:"status"`
	Date    string             `json:"date"`
	Host    string             `json:"host"`
	Caboose string             `json:"caboose"`
	Suite   string             `json:"suite"`
	Engine  string             `json:"engine"`
	// OS is the results file's os line ("macOS 15.3.1"), absent from
	// results stored before it existed.
	OS string `json:"os,omitempty"`
}

// Data is docs/security-matrix.json.
type Data struct {
	Version  int                          `json:"version"`
	Controls []Control                    `json:"controls"`
	Results  map[string]map[string]Result `json:"results"`
}

// ParseData reads the data file, refusing unknown fields and versions.
func ParseData(b []byte) (Data, error) {
	var d Data
	if len(b) > MaxResultsSize*4 {
		return d, fmt.Errorf("matrix: the data file is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("matrix: %w", err)
	}
	if dec.More() {
		return d, fmt.Errorf("matrix: text after the data")
	}
	if d.Version != Version {
		return d, fmt.Errorf("matrix: data file version %d, want %d", d.Version, Version)
	}
	for col, m := range d.Results {
		if !slices.Contains(Columns, col) {
			return d, fmt.Errorf("matrix: results for unknown column %q", col)
		}
		for id, r := range m {
			if err := checkResult(r); err != nil {
				return d, fmt.Errorf("matrix: stored result %s in %s: %w", id, col, err)
			}
		}
	}
	return d, nil
}

// Marshal writes the data file, with map keys in order.
func (d Data) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (d Data) clone() Data {
	out := Data{Version: d.Version, Controls: d.Controls, Results: map[string]map[string]Result{}}
	for col, m := range d.Results {
		out.Results[col] = map[string]Result{}
		for id, r := range m {
			out.Results[col][id] = r
		}
	}
	return out
}

// Merge stores the results file's verdicts in a copy of data. A stored
// result is replaced unless it has a later date; an equal date is
// replaced, so the later file on a command line wins. An INFO line is a
// check that ran and recorded a fact: stored as run, and listed as such.
// A build past a release is stored as that release plus "+dev"
// (releaseOf), since its commit names nothing a reader can look up.
func Merge(data Data, r Results, suite []conformance.Check) (Data, error) {
	have := map[string]bool{}
	for _, c := range suite {
		have[c.ID] = true
	}
	for _, e := range r.Entries {
		if !have[e.ID] {
			return data, fmt.Errorf("matrix: the results name a check the suite lacks: %s", e.ID)
		}
	}
	out := data.clone()
	col := r.Column()
	date := r.Date.UTC().Format("2006-01-02")
	for _, e := range r.Entries {
		if out.Results[col] == nil {
			out.Results[col] = map[string]Result{}
		}
		if old, ok := out.Results[col][e.ID]; ok && old.Date > date {
			continue
		}
		out.Results[col][e.ID] = Result{Status: e.Status, Date: date, Host: r.Host, Caboose: releaseOf(r.Caboose), Suite: r.Suite, Engine: r.Engine, OS: r.OS}
	}
	return out, nil
}

// describeRe is git describe's form for a commit past a tag.
var describeRe = regexp.MustCompile(`^(v\d+\.\d+\.\d+)-\d+-g[0-9a-f]+(-dirty)?$`)

// releaseOf is v, or for git describe's "v0.3.2-12-g0123abc" the
// release it builds on, "v0.3.2+dev".
func releaseOf(v string) string {
	if m := describeRe.FindStringSubmatch(v); m != nil {
		return m[1] + "+dev"
	}
	return v
}

var (
	dateRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}/[A-Za-z0-9._+-]{1,32}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	engineRe  = regexp.MustCompile(`^[A-Za-z0-9._+:,() -]{1,64}$`)
	osRe      = regexp.MustCompile(`^macOS [0-9]{1,4}(?:\.[0-9]{1,4}){0,3}$`)
)

// checkResult refuses a stored result whose text could break the table
// or whose status is not a verdict.
func checkResult(r Result) error {
	switch r.Status {
	case conformance.Pass, conformance.Fail, conformance.Error, conformance.Gap, conformance.Skip, conformance.Info:
	default:
		return fmt.Errorf("status %q is not a result", r.Status)
	}
	if !dateRe.MatchString(r.Date) {
		return fmt.Errorf("date %q", r.Date)
	}
	if _, err := time.Parse("2006-01-02", r.Date); err != nil {
		return fmt.Errorf("date %q", r.Date)
	}
	if !hostRe.MatchString(r.Host) {
		return fmt.Errorf("host %q", r.Host)
	}
	if !versionRe.MatchString(r.Caboose) {
		return fmt.Errorf("caboose version %q", r.Caboose)
	}
	if !versionRe.MatchString(r.Suite) {
		return fmt.Errorf("suite %q", r.Suite)
	}
	if !engineRe.MatchString(r.Engine) {
		return fmt.Errorf("engine %q", r.Engine)
	}
	if r.OS != "" && !osRe.MatchString(r.OS) {
		return fmt.Errorf("os %q", r.OS)
	}
	return nil
}

// Diff counts, per column, the stored results that differ between two
// data sets.
func Diff(before, after Data) map[string]int {
	n := map[string]int{}
	for col, m := range after.Results {
		for id, r := range m {
			if old, ok := before.Results[col][id]; !ok || old != r {
				n[col]++
			}
		}
	}
	return n
}

// Validate checks the data against the suite.
func Validate(d Data, suite []conformance.Check) error {
	have := map[string]bool{}
	for _, c := range suite {
		have[c.ID] = true
	}
	used := map[string]bool{}
	for i, row := range d.Controls {
		if row.Control == "" || strings.ContainsAny(row.Control, "|\n") {
			return fmt.Errorf("matrix: row %d has no usable control text", i+1)
		}
		seen := map[string]bool{}
		for _, id := range row.Checks {
			if !have[id] {
				return fmt.Errorf("matrix: row %q names %s, which is not a check", row.Control, id)
			}
			if seen[id] {
				return fmt.Errorf("matrix: row %q names %s twice", row.Control, id)
			}
			seen[id] = true
			used[id] = true
		}
		for col := range row.Cells {
			if !slices.Contains(Columns, col) {
				return fmt.Errorf("matrix: row %q has a cell for unknown column %q", row.Control, col)
			}
		}
		for _, col := range Columns {
			c, ok := row.Cells[col]
			if !ok {
				return fmt.Errorf("matrix: row %q has no cell for %s", row.Control, col)
			}
			switch c.Claim {
			case Held, NotCovered, NA:
			case Partial:
				if c.Note == "" {
					return fmt.Errorf("matrix: row %q is partial in %s with no note", row.Control, col)
				}
			default:
				return fmt.Errorf("matrix: row %q has the claim %q in %s", row.Control, c.Claim, col)
			}
			if strings.ContainsAny(c.Note, "|\n") {
				return fmt.Errorf("matrix: row %q has a note with a bar or a line break", row.Control)
			}
		}
	}
	for _, c := range suite {
		if !used[c.ID] {
			return fmt.Errorf("matrix: check %s is in no row", c.ID)
		}
	}
	for col, m := range d.Results {
		if !slices.Contains(Columns, col) {
			return fmt.Errorf("matrix: results for unknown column %q", col)
		}
		for id, r := range m {
			if !have[id] {
				return fmt.Errorf("matrix: a stored result in %s names %s, which is not a check", col, id)
			}
			if err := checkResult(r); err != nil {
				return fmt.Errorf("matrix: stored result %s in %s: %w", id, col, err)
			}
		}
	}
	return nil
}

func claimText(c Cell) string {
	switch c.Claim {
	case Partial:
		return "partial: " + c.Note
	case NA:
		return NA
	}
	if c.Note != "" {
		return c.Claim + ", " + c.Note
	}
	return c.Claim
}

func renderCell(row Control, col string, d Data, suite []conformance.Check) string {
	cell := row.Cells[col]
	if cell.Claim == NA {
		return NA
	}
	iso, trusts := columnRuns(col)
	byID := map[string]conformance.Check{}
	for _, c := range suite {
		byID[c.ID] = c
	}
	var ids []string
	for _, id := range row.Checks {
		ck, ok := byID[id]
		if !ok {
			continue
		}
		for _, t := range trusts {
			if ck.Applies(iso, t) {
				ids = append(ids, id)
				break
			}
		}
	}
	// Suite order within a row.
	slices.SortStableFunc(ids, func(a, b string) int {
		return slices.IndexFunc(suite, func(c conformance.Check) bool { return c.ID == a }) -
			slices.IndexFunc(suite, func(c conformance.Check) bool { return c.ID == b })
	})
	text := claimText(cell)
	if len(ids) == 0 {
		return text + " (no check)"
	}
	tick := func(id string) string { return "`" + id + "`" }
	var fails, errs, missing, skips []string
	// where is the machine a run was on: the os line, else OS/ARCH, and
	// the engine.
	type runKey struct{ date, where, caboose string }
	passed := map[runKey][]string{}
	gaps := map[runKey][]string{}
	skipped := map[runKey][]string{}
	infos := map[runKey][]string{}
	var keys []runKey
	var failDetail []string
	anyRes := false
	for _, id := range ids {
		r, ok := d.Results[col][id]
		if !ok {
			missing = append(missing, tick(id)+" not run")
			continue
		}
		anyRes = true
		k := runKey{r.Date, where(r), r.Caboose}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
		switch r.Status {
		case conformance.Fail:
			fails = append(fails, tick(id))
			failDetail = append(failDetail, fmt.Sprintf("%s (%s, %s, caboose %s)", tick(id), r.Date, where(r), r.Caboose))
		case conformance.Error:
			errs = append(errs, tick(id)+" error")
		case conformance.Gap:
			gaps[k] = append(gaps[k], tick(id))
		case conformance.Skip:
			skipped[k] = append(skipped[k], tick(id))
			skips = append(skips, tick(id)+" skipped")
		case conformance.Info:
			// Ran, and recorded: no verdict, but not "not run".
			infos[k] = append(infos[k], tick(id))
			skips = append(skips, tick(id)+" info")
		case conformance.Pass:
			passed[k] = append(passed[k], tick(id))
		}
	}
	if !anyRes {
		return text + " (not tested)"
	}
	if len(fails) > 0 {
		return "FAILED: " + strings.Join(failDetail, "; ")
	}
	if len(errs) > 0 || len(missing) > 0 || len(passed) == 0 && len(gaps) == 0 {
		detail := append(append(append([]string{}, errs...), missing...), skips...)
		if len(errs) > 0 || len(missing) > 0 {
			detail = append(append([]string{}, errs...), missing...)
		}
		return text + " (not tested: " + strings.Join(detail, ", ") + ")"
	}
	slices.SortFunc(keys, func(a, b runKey) int {
		return strings.Compare(a.date+"\x00"+a.where+"\x00"+a.caboose, b.date+"\x00"+b.where+"\x00"+b.caboose)
	})
	var groups []string
	for _, k := range keys {
		if len(passed[k]) == 0 && len(gaps[k]) == 0 {
			continue
		}
		g := fmt.Sprintf("tested %s on %s, caboose %s: %s", k.date, k.where, k.caboose, strings.Join(passed[k], ", "))
		if len(passed[k]) == 0 {
			g = fmt.Sprintf("tested %s on %s, caboose %s:", k.date, k.where, k.caboose)
		}
		if len(gaps[k]) > 0 {
			g += "; gap: " + strings.Join(gaps[k], ", ")
			g = strings.Replace(g, ":; gap", ": gap", 1)
		}
		if len(skipped[k]) > 0 {
			g += "; skipped: " + strings.Join(skipped[k], ", ")
		}
		if len(infos[k]) > 0 {
			g += "; info: " + strings.Join(infos[k], ", ")
		}
		groups = append(groups, "("+g+")")
	}
	return text + " " + strings.Join(groups, " ")
}

// where is the machine a stored result was run on, as a cell names it:
// "macOS 15.3.1 (engine)", or the host's OS/ARCH for a result stored
// without an os line.
func where(r Result) string {
	m := r.OS
	if m == "" {
		m = r.Host
	}
	return m + " (" + r.Engine + ")"
}

// Render is the markdown table, ending in a newline.
func Render(d Data, suite []conformance.Check) (string, error) {
	if err := Validate(d, suite); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| control |")
	for _, c := range Columns {
		b.WriteString(" " + columnHeader[c] + " |")
	}
	b.WriteString("\n|---|---|---|---|---|\n")
	for _, row := range d.Controls {
		b.WriteString("| " + row.Control + " |")
		for _, c := range Columns {
			b.WriteString(" " + renderCell(row, c, d, suite) + " |")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// Markers delimit the table in the document.
const (
	Begin = "<!-- matrix:begin -->"
	End   = "<!-- matrix:end -->"
)

// ReplaceTable replaces what is between the markers' lines.
func ReplaceTable(doc, table string) (string, error) {
	lines := strings.SplitAfter(doc, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case Begin:
			if begin >= 0 {
				return "", fmt.Errorf("matrix: the document has several %s", Begin)
			}
			begin = i
		case End:
			if end >= 0 {
				return "", fmt.Errorf("matrix: the document has several %s", End)
			}
			end = i
		}
	}
	if begin < 0 || end < 0 {
		return "", fmt.Errorf("matrix: the document needs one %s and one %s", Begin, End)
	}
	if end < begin {
		return "", fmt.Errorf("matrix: %s comes before %s", End, Begin)
	}
	if table != "" && !strings.HasSuffix(table, "\n") {
		table += "\n"
	}
	return strings.Join(lines[:begin+1], "") + table + strings.Join(lines[end:], ""), nil
}
