package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Edit is what caboose setup changes in config.toml: only the keys it
// asks about, so everything else in the file -- comments, other settings,
// their order -- stays as it was.
type Edit struct {
	// Set are top-level keys, by their config.toml names, and their new
	// values: a string or a bool.
	Set map[string]any
	// Unset are top-level keys to comment out.
	Unset []string
	// SetRoots replaces the [roots] table with Roots (name -> path), or
	// comments it out when Roots is nil.
	SetRoots bool
	Roots    map[string]string
}

// EditFile applies e to a config.toml's contents, line by line, as a person
// would: a key's active line is replaced; failing that, its commented-out
// line (the template's #key = ...) is uncommented with the new value;
// failing that, the key is added. A top-level key has to stay above the
// first table, or TOML reads it as the table's, so that is where one is
// added, and a commented line below a table is never the one uncommented.
// The [roots] table is rewritten in place, keeping any comment lines in
// it, or added at the end; removed, its lines are commented out.
//
// Line-based editing cannot follow every TOML construct (a multi-line
// string, say), so the result is to be checked with CheckEdit before it is
// written.
func EditFile(data []byte, e Edit) []byte {
	var lines []string
	if s := strings.TrimSuffix(string(data), "\n"); s != "" || len(data) > 0 {
		lines = strings.Split(s, "\n")
	}
	top := func() int { // the end of the top-level part: the first table
		for i, l := range lines {
			if isHeader(l) {
				return i
			}
		}
		return len(lines)
	}
	for _, k := range slices.Sorted(maps.Keys(e.Set)) {
		line := k + " = " + tomlValue(e.Set[k])
		end := top()
		if i := findLine(lines[:end], activeKey(k)); i >= 0 {
			lines[i] = line
		} else if i := findLine(lines[:end], commentedKey(k)); i >= 0 {
			lines[i] = line
		} else if end < len(lines) {
			lines = slices.Insert(lines, end, line, "")
		} else {
			lines = append(lines, line)
		}
	}
	for _, k := range e.Unset {
		end := top()
		for i, l := range lines[:end] {
			if activeKey(k).MatchString(l) {
				lines[i] = "#" + l
			}
		}
	}
	if e.SetRoots {
		lines = editRoots(lines, e.Roots)
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// editRoots rewrites the [roots] table in lines.
func editRoots(lines []string, roots map[string]string) []string {
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "["+rootsKey+"]" {
			start = i
			break
		}
	}
	end := len(lines)
	if start >= 0 {
		for i := start + 1; i < len(lines); i++ {
			if isHeader(lines[i]) {
				end = i
				break
			}
		}
	}
	if roots == nil {
		if start >= 0 {
			for i := start; i < end; i++ {
				if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
					lines[i] = "#" + lines[i]
				}
			}
		}
		return lines
	}
	block := []string{"[" + rootsKey + "]"}
	for _, name := range slices.Sorted(maps.Keys(roots)) {
		block = append(block, name+" = "+tomlValue(roots[name]))
	}
	if start < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		return append(lines, block...)
	}
	// Comment lines in the old table are kept, after the new entries.
	for _, l := range lines[start+1 : end] {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			block = append(block, l)
		}
	}
	return slices.Concat(lines[:start], block, lines[end:])
}

// CheckEdit reports whether edited, config.toml as EditFile made it, reads
// back as e meant: it parses, every key set has its value, every key unset
// is gone, and the roots are e's.
func CheckEdit(path string, edited []byte, e Edit) error {
	f, err := ParseFile(path, edited)
	if err != nil {
		return err
	}
	for k, v := range e.Set {
		want := fmt.Sprint(v)
		switch v {
		case true:
			want = "1"
		case false:
			want = ""
		}
		if got := f.Vals[fileKeys[k]]; got != want {
			return fmt.Errorf("%s reads back as %q, not %q", k, got, want)
		}
	}
	for _, k := range e.Unset {
		if got := f.Vals[fileKeys[k]]; got != "" {
			return fmt.Errorf("%s is still set, to %q", k, got)
		}
	}
	if e.SetRoots && !maps.Equal(f.Roots, e.Roots) {
		return fmt.Errorf("[roots] reads back as %v, not %v", f.Roots, e.Roots)
	}
	return nil
}

// Snippet is e as lines to add to config.toml by hand, for when an edit
// cannot be made safely.
func (e Edit) Snippet() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(e.Set)) {
		fmt.Fprintf(&b, "%s = %s\n", k, tomlValue(e.Set[k]))
	}
	for _, k := range e.Unset {
		fmt.Fprintf(&b, "# (remove %s)\n", k)
	}
	if e.SetRoots {
		if e.Roots == nil {
			b.WriteString("# (remove the [roots] table)\n")
		} else {
			b.WriteString("[" + rootsKey + "]\n")
			for _, name := range slices.Sorted(maps.Keys(e.Roots)) {
				fmt.Fprintf(&b, "%s = %s\n", name, tomlValue(e.Roots[name]))
			}
		}
	}
	return b.String()
}

var headerRE = regexp.MustCompile(`^\s*\[`)

func isHeader(line string) bool { return headerRE.MatchString(line) }

func activeKey(k string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*` + regexp.QuoteMeta(k) + `\s*=`)
}

func commentedKey(k string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*#\s*` + regexp.QuoteMeta(k) + `\s*=`)
}

func findLine(lines []string, re *regexp.Regexp) int {
	for i, l := range lines {
		if re.MatchString(l) {
			return i
		}
	}
	return -1
}

// tomlValue writes v, a string or a bool, as TOML: a string as a basic
// string, with \, " and control characters escaped.
func tomlValue(v any) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
