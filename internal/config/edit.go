package config

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Edit is what caboose setup changes in config.toml: only the keys it
// asks about, so everything else in the file -- comments, other settings,
// their order -- stays as it was.
type Edit struct {
	// Set are keys by their dotted names ("isolation", "session.tmux",
	// "vm.default.cpus") and their new values: a string, a bool, an int, a
	// []string, or a []any of strings and whole numbers (forward_ports,
	// as PortsValue gives it).
	Set map[string]any
	// Unset are keys, by their dotted names, to comment out.
	Unset []string
	// Tables are tables to have, by their dotted names ("gvisor.default"):
	// one the file lacks is added, empty.
	Tables []string
	// Remove are tables to remove, by their dotted names
	// ("vm.default.volume.cache"): each one's header and settings are
	// commented out, down to the next active header, so that what it said
	// stays readable, and making it again uncomments them. A table given
	// by dotted keys of a table it is in (volume.cache.path = ... in
	// [vm.default]) has those keys commented out.
	Remove []string
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
// (the template's #[session]), else added after the tables of the table
// it is in when the file has that one ([vm.default.volume.cache] after
// [vm.default] and its other tables), else at the end. A top-level key has
// to stay above the first table, or TOML reads it as the table's, so that
// is where one is added, and a commented line below a table is never the
// one uncommented. The [roots] table is rewritten in place, keeping any
// comment lines in it, and any [roots.NAME] tables are commented out, the
// long form written inline; removed, its lines are commented out. A table
// in Remove is commented out the same way.
//
// A header names its table as TOML reads it ([vm."default"] is
// [vm.default]), and a line inside a multi-line string or array is never
// a header. Line-based editing still cannot follow every TOML construct
// (a table given as an inline table, say), so the result is to be checked
// with CheckEdit before it is written.
func EditFile(data []byte, e Edit) []byte {
	ed := &editor{fresh: map[string]bool{}, parsed: map[string]parsedKey{}}
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
				end := ed.valueEnd(i)
				for j := i; j <= end; j++ {
					ed.put(j, "#"+ed.lines[j])
				}
				i = end
			}
		}
	}
	for _, t := range e.Remove {
		if start, _, _ := ed.block(t); start >= 0 {
			ed.commentOut(start)
		}
		ed.removeDotted(t)
	}
	if e.SetRoots {
		ed.setRoots(e.Roots)
	}
	if len(ed.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(ed.lines, "\n") + "\n")
}

type editor struct {
	lines []string
	// fresh are the tables this edit added rather than uncommented: the
	// commented lines below one are another table's, never its own keys.
	fresh map[string]bool
	// parsed is what the TOML parser makes of a header's or a key's text
	// (parse), by that text.
	parsed map[string]parsedKey
	// cont is continued's, until lines change (put, setLines).
	cont []bool
}

// put makes line i s.
func (ed *editor) put(i int, s string) {
	ed.lines[i] = s
	ed.cont = nil
}

// setLines makes lines the file's.
func (ed *editor) setLines(lines []string) {
	ed.lines = lines
	ed.cont = nil
}

// parsedKey is a key or table name as TOML reads it, and what it is
// ("Hash" for a table): ok is false when the parser refuses the text.
type parsedKey struct {
	name, typ string
	ok        bool
}

// parse is what the TOML parser makes of src, one line of TOML holding a
// table's header or a key = value: the name it defines, in the dotted form
// a Key prints (quoted only where a part needs it: [vm."default"] is
// vm.default, [a."b.c"] stays a."b.c"), and its type.
func (ed *editor) parse(src string) parsedKey {
	if p, ok := ed.parsed[src]; ok {
		return p
	}
	var p parsedKey
	var v map[string]any
	if md, err := toml.Decode(src+"\n", &v); err == nil {
		if keys := md.Keys(); len(keys) > 0 {
			k := keys[len(keys)-1]
			p = parsedKey{name: k.String(), typ: md.Type(k...), ok: true}
		}
	}
	ed.parsed[src] = p
	return p
}

// continued is, for each line, whether it begins inside a value an
// earlier line opened: a multi-line string, or an array or inline table
// not closed yet. Such a line is never a header nor a key's.
func (ed *editor) continued() []bool {
	if ed.cont != nil {
		return ed.cont
	}
	cont := make([]bool, len(ed.lines))
	ml, depth := "", 0
	for i, l := range ed.lines {
		cont[i] = ml != "" || depth > 0
		if !cont[i] && strings.HasPrefix(strings.TrimSpace(l), "[") {
			// A header, which opens no value, even one TOML refuses.
			continue
		}
		for k := 0; k < len(l); {
			if ml != "" {
				end := closeMultiline(l[k:], ml)
				if end < 0 {
					break
				}
				ml, k = "", k+end
				continue
			}
			c := l[k]
			switch {
			case c == '#':
				k = len(l)
			case strings.HasPrefix(l[k:], `"""`) || strings.HasPrefix(l[k:], "'''"):
				ml, k = l[k:k+3], k+3
			case c == '"' || c == '\'':
				for k++; k < len(l) && l[k] != c; k++ {
					if c == '"' && l[k] == '\\' {
						k++
					}
				}
				k++
			case c == '[' || c == '{':
				depth, k = depth+1, k+1
			case c == ']' || c == '}':
				depth, k = max(0, depth-1), k+1
			default:
				k++
			}
		}
	}
	ed.cont = cont
	return cont
}

// closeMultiline is the index in l just past the delimiter q (three
// double quotes, or three single ones) that closes a multi-line string l
// is inside of, -1 when l does not close it.
func closeMultiline(l, q string) int {
	if q != `"""` {
		if i := strings.Index(l, q); i >= 0 {
			return i + 3
		}
		return -1
	}
	for k := 0; k < len(l); k++ {
		switch {
		case l[k] == '\\':
			k++
		case strings.HasPrefix(l[k:], q):
			return k + 3
		}
	}
	return -1
}

// lineKey is the key line i sets, as TOML reads it ("volume.cache.path"
// for volume."cache".path = ...), when it is a key's line.
func (ed *editor) lineKey(i int) (string, bool) {
	if ed.continued()[i] {
		return "", false
	}
	l := strings.TrimSpace(ed.lines[i])
	if l == "" || l[0] == '#' || l[0] == '[' {
		return "", false
	}
	// The key ends at the first = outside its quoted parts.
	for k := 0; k < len(l); k++ {
		switch c := l[k]; c {
		case '"', '\'':
			for k++; k < len(l) && l[k] != c; k++ {
				if c == '"' && l[k] == '\\' {
					k++
				}
			}
		case '=':
			if p := ed.parse(l[:k] + "= 0"); p.ok {
				return p.name, true
			}
			return "", false
		}
	}
	return "", false
}

// splitKey splits a dotted key into its table ("" at the top level) and
// its name.
func splitKey(k string) (table, key string) {
	i := strings.LastIndex(k, ".")
	if i < 0 {
		return "", k
	}
	return k[:i], k[i+1:]
}

// header is the table line i opens, as TOML reads its name, and whether
// it is active rather than commented out (#[name], with whatever spacing
// and trailing comment TOML would take on the line uncommented); ok is
// false for any other line, and for a line inside a multi-line value. A
// [[table]], or a line TOML would refuse that starts with [, is an active
// header too, of no table EditFile edits.
func (ed *editor) header(i int) (name string, active, ok bool) {
	if ed.continued()[i] {
		return "", false, false
	}
	l := strings.TrimSpace(ed.lines[i])
	if strings.HasPrefix(l, "[") {
		if p := ed.parse(l); p.ok && p.typ == "Hash" {
			return p.name, true, true
		}
		return "", true, true
	}
	if rest, ok := commentedHeader(l); ok {
		if p := ed.parse(rest); p.ok && p.typ == "Hash" {
			return p.name, false, true
		}
	}
	return "", false, false
}

// commentedHeader is l, a trimmed line, uncommented, when it is a comment
// whose text starts as a table's header does.
func commentedHeader(l string) (string, bool) {
	rest, ok := strings.CutPrefix(l, "#")
	rest = strings.TrimSpace(rest)
	return rest, ok && strings.HasPrefix(rest, "[") && !strings.HasPrefix(rest, "[[")
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
			// As it was before it was commented out, its trailing comment
			// and all.
			rest, _ := commentedHeader(strings.TrimSpace(ed.lines[i]))
			ed.put(i, rest)
			return
		}
	}
	ed.fresh[table] = true
	if at := ed.familyEnd(table); at >= 0 {
		add := []string{"[" + table + "]"}
		if at > 0 && strings.TrimSpace(ed.lines[at-1]) != "" {
			add = append([]string{""}, add...)
		}
		if at < len(ed.lines) {
			add = append(add, "")
		}
		ed.setLines(slices.Insert(ed.lines, at, add...))
		return
	}
	lines := ed.lines
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) != "" {
		lines = append(lines, "")
	}
	ed.setLines(append(lines, "["+table+"]"))
}

// familyEnd is where a new table goes among the tables of the nearest
// table the file has that table is in (vm.default for
// vm.default.volume.cache): after the last of them, ahead of the comment
// lines that lead into the header after it, so that they stay with that
// header. Those are the comment lines after the last blank line before
// it; comment lines with no blank line between them and the last table's
// settings are that table's (a commented-out key of it), and stay with
// it. -1 when the file has no such table.
func (ed *editor) familyEnd(table string) int {
	parent := table
	for {
		i := strings.LastIndex(parent, ".")
		if i < 0 {
			return -1
		}
		parent = parent[:i]
		if start, _, _ := ed.block(parent); start >= 0 {
			break
		}
	}
	last := -1
	for i := range ed.lines {
		if name, active, ok := ed.header(i); ok && active && (name == parent || strings.HasPrefix(name, parent+".")) {
			last = i
		}
	}
	at := len(ed.lines)
	for i := last + 1; i < len(ed.lines); i++ {
		if _, _, ok := ed.header(i); ok {
			at = i
			break
		}
	}
	if at == len(ed.lines) {
		return at
	}
	// The last line of the table's own: its last setting, or its header.
	own := last
	for i := last + 1; i < at; i++ {
		if t := strings.TrimSpace(ed.lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			own = i
		}
	}
	lead := at
	for lead-1 > own && strings.HasPrefix(strings.TrimSpace(ed.lines[lead-1]), "#") {
		lead--
	}
	if lead-1 > own && strings.TrimSpace(ed.lines[lead-1]) == "" {
		return lead
	}
	return at
}

// set gives key k the value v, written as TOML.
func (ed *editor) set(k, v string) {
	table, key := splitKey(k)
	if table != "" {
		ed.ensureTable(table)
	}
	start, activeEnd, anyEnd := ed.block(table)
	if table != "" && start < 0 {
		return // a table it could not make; CheckEdit says so
	}
	line := key + " = " + v
	if i := findLine(ed.lines[start+1:activeEnd], activeKey(key)); i >= 0 {
		at := start + 1 + i
		ed.setLines(slices.Concat(ed.lines[:at], []string{line}, ed.lines[ed.valueEnd(at)+1:]))
		return
	}
	if i := findLine(ed.lines[start+1:anyEnd], commentedKey(key)); i >= 0 && !ed.fresh[table] {
		ed.put(start+1+i, line)
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
			ed.setLines(slices.Insert(ed.lines, at, line, ""))
		} else {
			ed.setLines(append(ed.lines, line))
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
	ed.setLines(slices.Insert(ed.lines, at, line))
}

// valueEnd is the last line of the value set on line i, a key's active
// line: i itself, unless the value is an array or a multi-line string that
// does not close on it, when it is the line that closes it (continued).
func (ed *editor) valueEnd(i int) int {
	cont := ed.continued()
	for i+1 < len(cont) && cont[i+1] {
		i++
	}
	return i
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
				ed.put(i, "["+rootsKey+"]")
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
		v := inlineRoot(r)
		block = append(block, name+" = "+v)
	}
	// Comment lines in the old table are kept, after the new entries.
	for _, l := range ed.lines[start+1 : end] {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			block = append(block, l)
		}
	}
	ed.setLines(slices.Concat(ed.lines[:start], block, ed.lines[end:]))
}

// commentOut comments out the table whose active header is line start,
// down to the next active header (never a line inside a multi-line value,
// which is the table's).
func (ed *editor) commentOut(start int) {
	end := len(ed.lines)
	for i := start + 1; i < len(ed.lines); i++ {
		if _, active, ok := ed.header(i); ok && active {
			end = i
			break
		}
	}
	ed.commentLines(start, end)
}

// commentLines comments out lines from up to end, but blank ones and
// comments.
func (ed *editor) commentLines(from, end int) {
	for i := from; i < end; i++ {
		if t := strings.TrimSpace(ed.lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			ed.put(i, "#"+ed.lines[i])
		}
	}
}

// removeDotted comments out the keys that give table by dotted keys of a
// table it is in, or of the top level: volume.cache.path = ... in
// [vm.default], or volume.cache = { ... }, for vm.default.volume.cache.
func (ed *editor) removeDotted(table string) {
	type span struct{ from, end int }
	var spans []span
	for parent := table; parent != ""; {
		i := strings.LastIndex(parent, ".")
		if i < 0 {
			parent = ""
		} else {
			parent = parent[:i]
		}
		rest := strings.TrimPrefix(table[len(parent):], ".")
		start, activeEnd, _ := ed.block(parent)
		if parent != "" && start < 0 {
			continue
		}
		for j := start + 1; j < activeEnd; j++ {
			if k, ok := ed.lineKey(j); ok && (k == rest || strings.HasPrefix(k, rest+".")) {
				end := ed.valueEnd(j)
				spans = append(spans, span{j, end + 1})
				j = end
			}
		}
	}
	for _, s := range spans {
		ed.commentLines(s.from, s.end)
	}
}

// CheckEdit reports whether edited, config.toml as EditFile made it from
// orig, reads back as e meant: it parses, every key set has its value, every
// key unset is gone, every table asked for is there, every table removed
// is gone, the roots are e's, and nothing else changed: each setting orig
// had, outside [roots] and the tables removed, still has its value in its
// table (uncommenting a header can move the settings below it into another
// table).
func CheckEdit(path string, orig, edited []byte, e Edit) error {
	f, err := ParseFile(path, edited)
	if err != nil {
		return err
	}
	if o, err := ParseFile(path, orig); err == nil {
		for k, v := range o.Vals {
			if _, set := e.Set[k]; set || slices.Contains(e.Unset, k) || e.removes(k) {
				continue
			}
			if got, ok := f.Vals[k]; !ok || !reflect.DeepEqual(got, v) {
				return fmt.Errorf("%s would change, from %v to %v", displayKey(k), v, got)
			}
		}
		for _, p := range slices.Concat(o.Profiles, o.ImageProfiles) {
			if !slices.Contains(f.Profiles, p) && !slices.Contains(f.ImageProfiles, p) {
				return fmt.Errorf("[%s] would be lost", p)
			}
		}
		if !e.SetRoots && !sameFileRoots(o.Roots, f.Roots) {
			return fmt.Errorf("[roots] would change")
		}
	}
	for k, v := range e.Set {
		switch t := v.(type) {
		case int:
			v = int64(t)
		case []string:
			list := make([]any, len(t))
			for i, e := range t {
				list[i] = e
			}
			v = list
		case []any:
			list := make([]any, len(t))
			for i, e := range t {
				if n, ok := e.(int); ok {
					e = int64(n)
				}
				list[i] = e
			}
			v = list
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
		if _, ok := volumeTable(t); ok {
			profile, name, _ := strings.Cut(t, "."+volumeKey+".")
			if !slices.ContainsFunc(f.Volumes[profile], func(v FileVolume) bool { return v.Name == name }) {
				return fmt.Errorf("[%s] is not there", t)
			}
			continue
		}
		if kindKeys(kind) != nil && !slices.Contains(f.Profiles, t) && !slices.Contains(f.ImageProfiles, t) {
			return fmt.Errorf("[%s] is not there", t)
		}
	}
	for _, t := range e.Remove {
		gone := !slices.Contains(f.Profiles, t) && !slices.Contains(f.ImageProfiles, t)
		// A volume's table has its path at least, so it is gone with its
		// keys.
		for k := range f.Vals {
			gone = gone && !strings.HasPrefix(k, t+".")
		}
		if !gone {
			return fmt.Errorf("[%s] is still there", t)
		}
	}
	if e.SetRoots && !sameFileRoots(f.Roots, e.Roots) {
		return fmt.Errorf("[roots] reads back as %v, not %v", f.Roots, e.Roots)
	}
	return nil
}

// CheckTOML is the TOML parser's error for data, nil when data is TOML,
// whatever its keys: an edit that gives an error here broke the file,
// where one that ParseFile alone refuses asked for what config.toml does
// not take.
func CheckTOML(data []byte) error {
	var v map[string]any
	_, err := toml.Decode(string(data), &v)
	return err
}

// removes reports whether key k, by its dotted name, is in a table e
// removes.
func (e Edit) removes(k string) bool {
	return slices.ContainsFunc(e.Remove, func(t string) bool { return strings.HasPrefix(k, t+".") })
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
	for _, t := range e.Remove {
		fmt.Fprintf(&b, "# (remove the [%s] table)\n", t)
	}
	switch {
	case !e.SetRoots:
	case e.Roots == nil:
		b.WriteString("# (remove the [" + rootsKey + "] table)\n")
	default:
		b.WriteString("[" + rootsKey + "]\n")
		for _, name := range slices.Sorted(maps.Keys(e.Roots)) {
			r := e.Roots[name]
			fmt.Fprintf(&b, "%s = %s\n", name, inlineRoot(r))
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

// tomlValue writes v, a string, a bool, a whole number, a []string or a
// []any of those, as TOML: a string as a basic string, with \, " and
// control characters escaped, and a list as an inline array of them.
func tomlValue(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, e := range list {
			parts[i] = tomlValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if list, ok := v.([]string); ok {
		parts := make([]string, len(list))
		for i, e := range list {
			parts[i] = tomlValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
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

// inlineRoot writes r as a [roots] value: the short form when it has
// nothing but a host, else an inline table of host and whichever of path
// and writable it has.
func inlineRoot(r FileRoot) string {
	if r.Path == "" && len(r.Writable) == 0 {
		return tomlValue(r.Host)
	}
	parts := []string{"host = " + tomlValue(r.Host)}
	if r.Path != "" {
		parts = append(parts, "path = "+tomlValue(r.Path))
	}
	if len(r.Writable) > 0 {
		parts = append(parts, "writable = "+tomlValue(r.Writable))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// sameFileRoots reports whether a and b are the same [roots], writable
// lists in order and all.
func sameFileRoots(a, b map[string]FileRoot) bool {
	return maps.EqualFunc(a, b, func(x, y FileRoot) bool {
		return x.Host == y.Host && x.Path == y.Path && slices.Equal(x.Writable, y.Writable)
	})
}
