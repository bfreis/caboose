package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Header is what the results file says of the run.
type Header struct {
	SuiteVersion, CabooseVersion string
	OS, Arch                     string
	// OSVersion is the host's OS version (sw_vers -productVersion on a
	// Mac), written as the "os:" line only on darwin and only when it is
	// a plain dotted version; "" leaves the line out.
	OSVersion        string
	Engine           string
	Isolation, Trust string
	Date             time.Time
}

// Entry is one check's line.
type Entry struct {
	ID, Title string
	Status    Status
	Reason    string
	Lines     []string
}

// Report is everything the results file holds.
type Report struct {
	Header  Header
	Entries []Entry
}

// BuildReport pairs the runner's results with the checks' titles and the
// lines they returned. Every result becomes an entry, in order.
func BuildReport(h Header, checks []Check, results []Result, col *Collector) Report {
	titles := map[string]string{}
	for _, c := range checks {
		titles[c.ID] = c.Title
	}
	rep := Report{Header: h}
	for _, r := range results {
		e := Entry{ID: r.ID, Title: titles[r.ID], Status: r.Status, Reason: r.Reason}
		if col != nil {
			e.Lines = col.Lines(r.ID)
		}
		rep.Entries = append(rep.Entries, e)
	}
	return rep
}

// Counts are the number of entries per status.
func (r Report) Counts() map[Status]int {
	m := map[Status]int{}
	for _, e := range r.Entries {
		m[e.Status]++
	}
	return m
}

var statusOrder = []Status{Pass, Fail, Error, Gap, Skip, Info}

var (
	userPathRe = regexp.MustCompile(`/(?:Users|home)/[^/\s]+|/root\b`)
	envAssign  = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=\S*`)
	statusLine = regexp.MustCompile(`^(PASS|FAIL|ERROR|GAP|SKIP|INFO) `)
	osVersion  = regexp.MustCompile(`^[0-9]{1,4}(?:\.[0-9]{1,4}){0,3}$`)
)

// OSLine is the "os:" header's value, "macOS <version>", or "" when the
// header has no version fit to show: not darwin, none known, or anything
// but a plain dotted version.
func (h Header) OSLine() string {
	if h.OS != "darwin" || !osVersion.MatchString(h.OSVersion) {
		return ""
	}
	return "macOS " + h.OSVersion
}

const maxLine = 300

// Scrub makes a string safe for the results file: B shown as <B>, the
// caller's strings (home, host name, environment values) and any home path
// or NAME=value removed, control characters turned into spaces, and the
// length bounded. Nothing it returns can be more than one line.
func Scrub(s string, base string, redact []string) string {
	if base != "" {
		s = strings.ReplaceAll(s, base, "<B>")
	}
	for _, r := range redact {
		if len(r) >= 3 {
			s = strings.ReplaceAll(s, r, "<redacted>")
		}
	}
	s = userPathRe.ReplaceAllString(s, "<path>")
	s = envAssign.ReplaceAllString(s, "<env>")
	s = strings.Map(func(r rune) rune {
		if r == utf8Replacement || !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > maxLine {
		s = string(rs[:maxLine]) + "..."
	}
	return s
}

const utf8Replacement = '�'

// Format renders the report. It fails rather than drop an entry: the
// number of status lines it wrote must equal the number of entries.
func (r Report) Format(base string, redact []string) (string, error) {
	sc := func(s string) string { return Scrub(s, base, redact) }
	var b strings.Builder
	h := r.Header
	fmt.Fprintf(&b, "caboose conformance results\n")
	fmt.Fprintf(&b, "suite: %s\n", sc(h.SuiteVersion))
	fmt.Fprintf(&b, "caboose: %s\n", sc(h.CabooseVersion))
	fmt.Fprintf(&b, "host: %s/%s\n", sc(h.OS), sc(h.Arch))
	if l := h.OSLine(); l != "" {
		fmt.Fprintf(&b, "os: %s\n", l)
	}
	fmt.Fprintf(&b, "engine: %s\n", sc(h.Engine))
	fmt.Fprintf(&b, "isolation: %s\n", sc(h.Isolation))
	fmt.Fprintf(&b, "trust: %s\n", sc(h.Trust))
	fmt.Fprintf(&b, "date: %s\n", h.Date.UTC().Format("2006-01-02"))
	if h.OS == "linux" {
		b.WriteString("note: Linux hosts are untested\n")
	}
	b.WriteString("\n")
	for _, e := range r.Entries {
		if !validStatus(e.Status) {
			return "", fmt.Errorf("conformance: entry %s has status %q", e.ID, e.Status)
		}
		fmt.Fprintf(&b, "%s %s %s\n", e.Status, sc(e.ID), sc(e.Title))
		if e.Status != Pass && e.Reason != "" {
			fmt.Fprintf(&b, "  reason: %s\n", sc(e.Reason))
		}
		for _, l := range e.Lines {
			fmt.Fprintf(&b, "  %s\n", sc(l))
		}
	}
	counts := r.Counts()
	var parts []string
	for _, s := range statusOrder {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	fmt.Fprintf(&b, "\nsummary: %d checks", len(r.Entries))
	if len(parts) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(parts, ", "))
	}
	b.WriteString("\n")
	n := 0
	for _, l := range strings.Split(b.String(), "\n") {
		if statusLine.MatchString(l) {
			n++
		}
	}
	if n != len(r.Entries) {
		return "", fmt.Errorf("conformance: %d entries but %d result lines", len(r.Entries), n)
	}
	return b.String(), nil
}

// WriteResults writes text to path, which must be absolute and must not
// exist: it is created with O_EXCL (so a symbolic link there is refused
// too), mode 0600. It never replaces or removes anything.
func WriteResults(path, text string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("conformance: the results file %q must be a clean absolute path", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("conformance: not writing the results: %w", err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
