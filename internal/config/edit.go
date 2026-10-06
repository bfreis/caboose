package config

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// Edit is what caboose setup changes in config.toml: only the keys it
// asks about, so everything else in the file -- comments, other settings,
// their order -- stays as it was.
type Edit struct {
	// Set are keys by their dotted names ("isolation", "session.tmux",
	// "vm.default.cpus") and their new values: a string, a bool or an int.
	Set map[string]any
	// Unset are keys, by their dotted names, to comment out.
	Unset []string
	// Tables are tables to have, by their dotted names ("gvisor.default"):
	// one the file lacks is added, empty.
	Tables []string
	// SetRoots replaces the [roots] table with Roots, or comments it out
	// when Roots is nil.
	SetRoots bool
	Roots    map[string]FileRoot
}

// EditFile applies e to a config.toml's contents, line by line, as a person
// would. A key's active line in its table is replaced; failing that, its
// commented-out line there (the template's #key = ...) is uncommented with
// the new value; failing that, the key is added at the end of its table. A
// table the file lacks is made by uncommenting its commented-out header
// (the template's #[session]), else added at the end. A top-level key has
// to stay above the first table, or TOML reads it as the table's, so that
// is where one is added, and a commented line below a table is never the
// one uncommented. The [roots] table is rewritten in place, keeping any
// comment lines in it, and any [roots.NAME] tables are commented out, the
// long form written inline; removed, its lines are commented out.
//
// Line-based editing cannot follow every TOML construct (a multi-line
// string, say), so the result is to be checked with CheckEdit before it is
// written.
func EditFile(data []byte, e Edit) []byte {
	ed := &editor{}
	if s := strings.TrimSuffix(string(data), "\n"); s != "" || len(data) > 0 {
		ed.lines = strings.Split(s, "\n")
	}
	for _, t := range e.Tables {
		ed.ensureTable(t)
	}
	for _, k := range slices.Sorted(maps.Keys(e.Set)) {
		ed.set(k, tomlValue(e.Set[k]))
	}
	for _, k := range e.Unset {
		table, key := splitKey(k)
		start, active, _ := ed.block(table)
		if table != "" && start < 0 {
			continue
		}
		for i := start + 1; i < active; i++ {
			if activeKey(key).MatchString(ed.lines[i]) {
				ed.lines[i] = "#" + ed.lines[i]
			}
		}
	}
	if e.SetRoots {
		ed.setRoots(e.Roots)
	}
	if len(ed.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(ed.lines, "\n") + "\n")
}

type editor struct{ lines []string }

// splitKey splits a dotted key into its table ("" at the top level) and
// its name.
func splitKey(k string) (table, key string) {
	i := strings.LastIndex(k, ".")
	if i < 0 {
		return "", k
	}
	return k[:i], k[i+1:]
}

var (
	activeHeader    = regexp.MustCompile(`^\s*\[\s*([^\[\]#]*?)\s*\]\s*(#.*)?$`)
	commentedHeader = regexp.MustCompile(`^\s*#\s*\[([A-Za-z0-9_.-]+)\]\s*$`)
)

// header is the table line i opens, and whether it is active rather than
// commented out; ok is false for any other line. A [[table]] is a header
// too, of no table EditFile edits.
func (ed *editor) header(i int) (name string, active, ok bool) {
	l := ed.lines[i]
	if m := activeHeader.FindStringSubmatch(l); m != nil {
		return strings.ReplaceAll(m[1], " ", ""), true, true
	}
	if strings.HasPrefix(strings.TrimSpace(l), "[") {
		return "", true, true
	}
	if m := commentedHeader.FindStringSubmatch(l); m != nil {
		return m[1], false, true
	}
	return "", false, false
}

// block is table's lines: its active header's index (-1 for the top
// level, or when it has none), the end of what TOML reads as its keys (the
// next active header), and the end of where its commented lines are looked
// for and new ones added (the next header of either kind).
func (ed *editor) block(table string) (start, activeEnd, anyEnd int) {
	start = -1
	if table != "" {
		for i := range ed.lines {
			if name, active, ok := ed.header(i); ok && active && name == table {
				start = i
				break
			}
		}
		if start < 0 {
			return -1, -1, -1
		}
	}
	activeEnd, anyEnd = len(ed.lines), len(ed.lines)
	for i := start + 1; i < len(ed.lines); i++ {
		_, active, ok := ed.header(i)
		if !ok {
			continue
		}
		if anyEnd == len(ed.lines) {
			anyEnd = i
		}
		if active {
			activeEnd = i
			break
		}
	}
	return start, activeEnd, anyEnd
}

// captures reports whether uncommenting the header at line i would make
// the active settings after it, which belong to the table above, its own.
func (ed *editor) captures(i int) bool {
	for j := i + 1; j < len(ed.lines); j++ {
		if _, active, ok := ed.header(j); ok && active {
			return false
		}
		if t := strings.TrimSpace(ed.lines[j]); t != "" && !strings.HasPrefix(t, "#") {
			return true
		}
	}
	return false
}

// ensureTable makes table exist: uncommenting its commented-out header
// when that moves no setting, else adding one at the end.
func (ed *editor) ensureTable(table string) {
	if start, _, _ := ed.block(table); start >= 0 {
		return
	}
	for i := range ed.lines {
		if name, active, ok := ed.header(i); ok && !active && name == table && !ed.captures(i) {
			ed.lines[i] = "[" + table + "]"
			return
		}
	}
	if n := len(ed.lines); n > 0 && strings.TrimSpace(ed.lines[n-1]) != "" {
		ed.lines = append(ed.lines, "")
	}
	ed.lines = append(ed.lines, "["+table+"]")
}

// set gives key k the value v, written as TOML.
func (ed *editor) set(k, v string) {
	table, key := splitKey(k)
	if table != "" {
		ed.ensureTable(table)
	}
	start, activeEnd, anyEnd := ed.block(table)
	line := key + " = " + v
	if i := findLine(ed.lines[start+1:activeEnd], activeKey(key)); i >= 0 {
		ed.lines[start+1+i] = line
		return
	}
	if i := findLine(ed.lines[start+1:anyEnd], commentedKey(key)); i >= 0 {
		ed.lines[start+1+i] = line
		return
	}
	if table == "" {
		// Above the first table, and above the comment lines that lead
		// into it.
		at := anyEnd
		for at > 0 && at < len(ed.lines) && strings.HasPrefix(strings.TrimSpace(ed.lines[at-1]), "#") {
			at--
		}
		if at < len(ed.lines) {
			ed.lines = slices.Insert(ed.lines, at, line, "")
		} else {
			ed.lines = append(ed.lines, line)
		}
		return
	}
	// After the table's last setting, or its header.
	at := start + 1
	for i := start + 1; i < anyEnd; i++ {
		if t := strings.TrimSpace(ed.lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			at = i + 1
		}
	}
	ed.lines = slices.Insert(ed.lines, at, line)
}

// setRoots rewrites [roots] as roots, or comments it out when roots is
// nil; either way any [roots.NAME] table is commented out, since the long
// form is written inline.
func (ed *editor) setRoots(roots map[string]FileRoot) {
	for i := 0; i < len(ed.lines); i++ {
		name, active, ok := ed.header(i)
		if !ok || !active || !strings.HasPrefix(name, rootsKey+".") {
			continue
		}
		ed.commentOut(i)
	}
	start, _, _ := ed.block(rootsKey)
	if roots == nil {
		if start >= 0 {
			ed.commentOut(start)
		}
		return
	}
	if start < 0 {
		// The template's commented-out [roots], else a new one at the end.
		for i := range ed.lines {
			if name, active, ok := ed.header(i); ok && !active && name == rootsKey {
				ed.lines[i] = "[" + rootsKey + "]"
				start = i
				break
			}
		}
	}
	if start < 0 {
		ed.ensureTable(rootsKey)
		start = len(ed.lines) - 1
	}
	_, _, end := ed.block(rootsKey)
	block := []string{"[" + rootsKey + "]"}
	for _, name := range slices.Sorted(maps.Keys(roots)) {
		r := roots[name]
		v := tomlValue(r.Host)
		if r.Path != "" {
			v = "{ host = " + tomlValue(r.Host) + ", path = " + tomlValue(r.Path) + " }"
		}
		block = append(block, name+" = "+v)
	}
	// Comment lines in the old table are kept, after the new entries.
	for _, l := range ed.lines[start+1 : end] {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			block = append(block, l)
		}
	}
	ed.lines = slices.Concat(ed.lines[:start], block, ed.lines[end:])
}

// commentOut comments out the table whose active header is line start,
// down to the next active header.
func (ed *editor) commentOut(start int) {
	for i := start; i < len(ed.lines); i++ {
		if _, active, ok := ed.header(i); i > start && ok && active {
			return
		}
		if t := strings.TrimSpace(ed.lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			ed.lines[i] = "#" + ed.lines[i]
		}
	}
}

// CheckEdit reports whether edited, config.toml as EditFile made it from
// orig, reads back as e meant: it parses, every key set has its value, every
// key unset is gone, every table asked for is there, the roots are e's, and
// nothing else changed: each setting orig had, outside [roots], still has
// its value in its table (uncommenting a header can move the settings below
// it into another table).
func CheckEdit(path string, orig, edited []byte, e Edit) error {
	f, err := ParseFile(path, edited)
	if err != nil {
		return err
	}
	if o, err := ParseFile(path, orig); err == nil {
		for k, v := range o.Vals {
			if _, set := e.Set[k]; set || slices.Contains(e.Unset, k) {
				continue
			}
			if got, ok := f.Vals[k]; !ok || !reflect.DeepEqual(got, v) {
				return fmt.Errorf("%s would change, from %v to %v", displayKey(k), v, got)
			}
		}
		for _, p := range o.Profiles {
			if !slices.Contains(f.Profiles, p) {
				return fmt.Errorf("[%s] would be lost", p)
			}
		}
		if !e.SetRoots && !maps.Equal(o.Roots, f.Roots) {
			return fmt.Errorf("[roots] would change")
		}
	}
	for k, v := range e.Set {
		if n, ok := v.(int); ok {
			v = int64(n)
		}
		if got, ok := f.Vals[k]; !ok || !reflect.DeepEqual(got, v) {
			return fmt.Errorf("%s reads back as %v, not %v", displayKey(k), got, v)
		}
	}
	for _, k := range e.Unset {
		if got, ok := f.Vals[k]; ok {
			return fmt.Errorf("%s is still set, to %v", displayKey(k), got)
		}
	}
	for _, t := range e.Tables {
		kind, _, _ := strings.Cut(t, ".")
		if profileKeys[kind] != nil && !slices.Contains(f.Profiles, t) {
			return fmt.Errorf("[%s] is not there", t)
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
	byTable := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(e.Set)) {
		table, key := splitKey(k)
		byTable[table] = append(byTable[table], key+" = "+tomlValue(e.Set[k]))
	}
	for _, t := range e.Tables {
		if byTable[t] == nil {
			byTable[t] = []string{}
		}
	}
	for _, t := range slices.Sorted(maps.Keys(byTable)) {
		if t != "" {
			b.WriteString("[" + t + "]\n")
		}
		for _, l := range byTable[t] {
			b.WriteString(l + "\n")
		}
	}
	for _, k := range e.Unset {
		fmt.Fprintf(&b, "# (remove %s)\n", displayKey(k))
	}
	switch {
	case !e.SetRoots:
	case e.Roots == nil:
		b.WriteString("# (remove the [" + rootsKey + "] table)\n")
	default:
		b.WriteString("[" + rootsKey + "]\n")
		for _, name := range slices.Sorted(maps.Keys(e.Roots)) {
			r := e.Roots[name]
			if r.Path != "" {
				fmt.Fprintf(&b, "%s = { host = %s, path = %s }\n", name, tomlValue(r.Host), tomlValue(r.Path))
			} else {
				fmt.Fprintf(&b, "%s = %s\n", name, tomlValue(r.Host))
			}
		}
	}
	return b.String()
}

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

// tomlValue writes v, a string, a bool or a whole number, as TOML: a
// string as a basic string, with \, " and control characters escaped.
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
