// Package matrix reads the conformance suite's results files, keeps the
// latest verdict per check and isolation in docs/security-matrix.json, and
// renders the table that docs/security.md shows. It is pure: it opens no
// file, runs no process and reads no clock.
package matrix

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/conformance"
)

// MaxResultsSize is the largest results file ParseResults reads.
const MaxResultsSize = 1 << 20

// Results is a parsed results file.
type Results struct {
	Suite, Caboose, Host, Engine string
	// OS is the optional "os:" line ("macOS 15.3.1"); "" when the file
	// has none, as files written before it existed do not.
	OS               string
	Isolation, Trust string
	Date             time.Time
	Entries          []Entry
}

// Entry is one status line.
type Entry struct {
	Status conformance.Status
	ID     string
	Title  string
}

var (
	entryRe   = regexp.MustCompile(`^([A-Z]+) (\S+)(?: (.*))?$`)
	summaryRe = regexp.MustCompile(`^summary: (\d+) checks?(?:: (.+))?$`)
	countRe   = regexp.MustCompile(`^(\d+) ([A-Z]+)$`)
)

var knownStatus = map[conformance.Status]bool{
	conformance.Pass: true, conformance.Fail: true, conformance.Error: true,
	conformance.Gap: true, conformance.Skip: true, conformance.Info: true,
}

var headerKeys = []string{"suite", "caboose", "host", "engine", "isolation", "trust", "date"}

// ParseResults parses what conformance.Report.Format writes, strictly.
// The os line after host is optional: files written before it existed
// have none.
func ParseResults(b []byte) (Results, error) {
	var r Results
	if len(b) > MaxResultsSize {
		return r, fmt.Errorf("results: the file is over %d bytes", MaxResultsSize)
	}
	lines := strings.Split(string(b), "\n")
	i := 0
	if lines[i] != "caboose conformance results" {
		return r, fmt.Errorf("results: not a conformance results file")
	}
	i++
	vals := map[string]string{}
	for _, k := range headerKeys {
		if i >= len(lines) || !strings.HasPrefix(lines[i], k+": ") {
			return r, fmt.Errorf("results: missing header line %q", k+":")
		}
		v := strings.TrimSpace(strings.TrimPrefix(lines[i], k+": "))
		if v == "" {
			return r, fmt.Errorf("results: header %q is empty", k)
		}
		vals[k] = v
		i++
		// The optional os line follows host.
		if k == "host" && i < len(lines) && strings.HasPrefix(lines[i], "os: ") {
			v := strings.TrimSpace(strings.TrimPrefix(lines[i], "os: "))
			if !osRe.MatchString(v) {
				return r, fmt.Errorf("results: the os header has characters or a length the matrix does not take")
			}
			vals["os"] = v
			i++
		}
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "note: ") {
		i++
	}
	r.Suite, r.Caboose, r.Host, r.Engine = vals["suite"], vals["caboose"], vals["host"], vals["engine"]
	r.Isolation, r.Trust, r.OS = vals["isolation"], vals["trust"], vals["os"]
	if os, arch, ok := strings.Cut(r.Host, "/"); !ok || os == "" || arch == "" || strings.Contains(arch, "/") {
		return r, fmt.Errorf("results: host %q is not OS/ARCH", r.Host)
	}
	switch r.Isolation {
	case "container", "gvisor", "vm":
	default:
		return r, fmt.Errorf("results: unknown isolation %q", r.Isolation)
	}
	switch r.Trust {
	case "fallible", "hostile":
	default:
		return r, fmt.Errorf("results: unknown trust %q", r.Trust)
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"host", hostRe.MatchString(r.Host)}, {"caboose", versionRe.MatchString(r.Caboose)},
		{"suite", versionRe.MatchString(r.Suite)}, {"engine", engineRe.MatchString(r.Engine)},
	} {
		if !c.ok {
			return r, fmt.Errorf("results: the %s header has characters or a length the matrix does not take", c.name)
		}
	}
	d, err := time.ParseInLocation("2006-01-02", vals["date"], time.UTC)
	if err != nil {
		return r, fmt.Errorf("results: date %q is not YYYY-MM-DD", vals["date"])
	}
	r.Date = d
	seen := map[string]bool{}
	summarized := false
	for ; i < len(lines); i++ {
		l := lines[i]
		switch {
		case l == "" || strings.HasPrefix(l, " "):
			continue
		case summarized:
			return r, fmt.Errorf("results: text after the summary: %q", l)
		case strings.HasPrefix(l, "summary:"):
			if err := checkSummary(l, r.Entries); err != nil {
				return r, err
			}
			summarized = true
			continue
		}
		m := entryRe.FindStringSubmatch(l)
		if m == nil {
			return r, fmt.Errorf("results: unreadable line %q", l)
		}
		st := conformance.Status(m[1])
		if !knownStatus[st] {
			return r, fmt.Errorf("results: unknown status %q", m[1])
		}
		if seen[m[2]] {
			return r, fmt.Errorf("results: check %s appears twice", m[2])
		}
		seen[m[2]] = true
		r.Entries = append(r.Entries, Entry{Status: st, ID: m[2], Title: m[3]})
	}
	if !summarized {
		return r, fmt.Errorf("results: no summary line")
	}
	return r, nil
}

func checkSummary(l string, entries []Entry) error {
	m := summaryRe.FindStringSubmatch(l)
	if m == nil {
		return fmt.Errorf("results: unreadable summary %q", l)
	}
	total, _ := strconv.Atoi(m[1])
	if total != len(entries) {
		return fmt.Errorf("results: the summary says %d checks, the file has %d", total, len(entries))
	}
	want := map[conformance.Status]int{}
	for _, e := range entries {
		want[e.Status]++
	}
	got := map[conformance.Status]int{}
	if m[2] != "" {
		for _, p := range strings.Split(m[2], ", ") {
			c := countRe.FindStringSubmatch(p)
			if c == nil {
				return fmt.Errorf("results: unreadable summary count %q", p)
			}
			n, _ := strconv.Atoi(c[1])
			got[conformance.Status(c[2])] += n
		}
	}
	for st := range knownStatus {
		if got[st] != want[st] {
			return fmt.Errorf("results: the summary counts %d %s, the file has %d", got[st], st, want[st])
		}
	}
	for st := range got {
		if !knownStatus[st] {
			return fmt.Errorf("results: unknown status %q in the summary", st)
		}
	}
	return nil
}

// Column is the matrix column a results file feeds.
func (r Results) Column() string {
	if r.Isolation == "vm" {
		return "vm/" + r.Trust
	}
	return r.Isolation
}
