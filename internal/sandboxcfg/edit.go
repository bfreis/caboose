package sandboxcfg

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Line-based edits of a sandbox config, as a person would make them: only
// the lines an edit is about change, comments and everything else stay.
// TOML read line by line can be fooled (a multi-line string, say), so every
// edit is parsed back and checked for doing what it said before it is
// returned.

// ContainerHome is the agent's home in the container, which a path may be
// given under instead of ~.
const ContainerHome = "/home/agent"

var (
	keepHeader = regexp.MustCompile(`^\s*\[\[\s*keep\s*\]\]\s*(#.*)?$`)
	syncHeader = regexp.MustCompile(`^\s*\[\[\s*keep\s*\.\s*sync\s*\]\]\s*(#.*)?$`)
	pathLine   = regexp.MustCompile(`^\s*path\s*=`)
	syncLine   = regexp.MustCompile(`^\s*sync\s*=`)
)

// HomeRel is p, given as ~/... or under ContainerHome, relative to the
// home, cleaned.
func HomeRel(p string) (string, error) {
	rel, ok := strings.CutPrefix(p, "~/")
	if !ok {
		rel, ok = strings.CutPrefix(p, ContainerHome+"/")
	}
	rel = strings.TrimSuffix(rel, "/")
	if !ok || !cleanRel(rel) {
		return "", fmt.Errorf("%q is not a path under the home: write it ~/...", p)
	}
	return rel, nil
}

// block is a [[keep]] entry's lines in a file.
type block struct {
	start, end int // lines[start:end], the header included
	rel        string
	path       int   // the line of its own path = ..., or -1
	sync       int   // the line of its own sync = ..., or -1
	subs       []sub // its [[keep.sync]] entries
}

// sub is a [[keep.sync]] entry's lines.
type sub struct {
	start, end int
	rel        string // its path, relative to the home; "" when unreadable
}

// blocks finds each [[keep]] entry in lines.
func blocks(lines []string) []block {
	var out []block
	var cur *block
	var cs *sub
	closeSub := func(at int) {
		if cs != nil {
			cs.end = at
			cur.subs = append(cur.subs, *cs)
			cs = nil
		}
	}
	closeBlock := func(at int) {
		if cur != nil {
			closeSub(at)
			cur.end = at
			out = append(out, *cur)
			cur = nil
		}
	}
	for i, l := range lines {
		switch {
		case keepHeader.MatchString(l):
			closeBlock(i)
			cur = &block{start: i, path: -1, sync: -1}
		case syncHeader.MatchString(l) && cur != nil:
			closeSub(i)
			cs = &sub{start: i}
		case headerRE.MatchString(l):
			closeBlock(i)
		case cur != nil && pathLine.MatchString(l):
			if rel, ok := linePath(l); ok {
				if cs != nil {
					cs.rel = rel
				} else {
					cur.rel, cur.path = rel, i
				}
			}
		case cur != nil && cs == nil && syncLine.MatchString(l):
			cur.sync = i
		}
	}
	closeBlock(len(lines))
	return out
}

// linePath reads a path = "..." line's value, relative to the home.
func linePath(l string) (string, bool) {
	var v struct {
		Path string `toml:"path"`
	}
	if _, err := toml.Decode(strings.TrimSpace(l), &v); err != nil {
		return "", false
	}
	rel, ok := strings.CutPrefix(v.Path, "~/")
	return strings.TrimSuffix(rel, "/"), ok
}

// endOf is where to insert at the end of b: after its last line that is
// not blank or a comment.
func endOf(lines []string, b block) int { return trimEnd(lines, b.start, b.end) }

// trimEnd is end moved back over the blank and comment lines that end
// lines[start:end] -- those belong to what follows -- but never onto start.
func trimEnd(lines []string, start, end int) int {
	for end > start+1 {
		t := strings.TrimSpace(lines[end-1])
		if t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		end--
	}
	return end
}

// AddSync makes home path p sync: a [[keep.sync]] entry in the keep entry
// that holds it, sync = true when p is that entry itself, or a new entry,
// synced, when none holds it. msg says what it did; data comes back as it
// was when p already syncs.
func AddSync(data []byte, p string) (out []byte, msg string, err error) {
	rel, err := HomeRel(p)
	if err != nil {
		return nil, "", err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, "", err
	}
	if slices.Contains(Denied, rel) {
		return nil, "", fmt.Errorf("~/%s never syncs", rel)
	}
	if r, ok := c.Match(rel); ok {
		return data, fmt.Sprintf("~/%s already syncs (%s)", rel, r.Path), nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	k, held := c.Keeps(rel)
	switch {
	case !held:
		if _, err := keepRel("~/" + rel); err != nil {
			return nil, "", fmt.Errorf("no keep entry holds ~/%s, and it cannot be one: %v", rel, err)
		}
		lines = append(lines, "", "[[keep]]", fmt.Sprintf("path = %q", "~/"+rel), "sync = true")
		msg = fmt.Sprintf("added ~/%s, kept and synced; the keep takes effect at the next 'caboose restart'", rel)
	case !k.Listed:
		lines = append(lines, "", "[[keep]]", fmt.Sprintf("path = %q", k.Path))
		if k.File {
			lines = append(lines, "file = true")
		}
		if rel == k.Rel {
			lines = append(lines, "sync = true")
		} else {
			lines = append(lines, "  [[keep.sync]]", fmt.Sprintf("  path = %q", "~/"+rel))
		}
		msg = fmt.Sprintf("listed %s, syncing ~/%s", k.Path, rel)
	default:
		for _, r := range k.Rules {
			if r.Matches(rel) {
				return nil, "", fmt.Errorf("%s covers ~/%s but leaves it out; edit %s by hand", r.Path, rel, HomePath)
			}
		}
		i := slices.IndexFunc(blocks(lines), func(b block) bool { return b.rel == k.Rel })
		if i < 0 {
			return nil, "", fmt.Errorf("cannot find the entry for %s in the file; edit %s by hand", k.Path, HomePath)
		}
		b := blocks(lines)[i]
		switch {
		case rel == k.Rel && len(b.subs) > 0:
			lines = slices.Insert(lines, endOf(lines, b), "  [[keep.sync]]", fmt.Sprintf("  path = %q", k.Path))
		case rel == k.Rel && b.sync >= 0:
			lines[b.sync] = "sync = true"
		case rel == k.Rel:
			lines = slices.Insert(lines, max(b.path, b.start)+1, "sync = true")
		case b.sync >= 0:
			// sync = false: rules instead.
			lines = slices.Delete(lines, b.sync, b.sync+1)
			b = blocks(lines)[i]
			lines = slices.Insert(lines, endOf(lines, b), "  [[keep.sync]]", fmt.Sprintf("  path = %q", "~/"+rel))
		default:
			lines = slices.Insert(lines, endOf(lines, b), "  [[keep.sync]]", fmt.Sprintf("  path = %q", "~/"+rel))
		}
		msg = fmt.Sprintf("~/%s syncs, in %s", rel, k.Path)
	}
	out = []byte(strings.Join(lines, "\n") + "\n")
	if nc, err := Parse(out); err != nil {
		return nil, "", fmt.Errorf("the edit would not parse (%v); edit %s by hand", err, HomePath)
	} else if _, ok := nc.Match(rel); !ok {
		return nil, "", fmt.Errorf("the edit would not make ~/%s sync; edit %s by hand", rel, HomePath)
	}
	return out, msg, nil
}

// ErrNotSynced is RemoveSync's error for a path nothing syncs.
var ErrNotSynced = errors.New("not synced")

// RemoveSync stops home path p syncing: its [[keep.sync]] entry removed,
// or its entry's sync = line. The keep entry stays, and so do the files.
// A path synced only as part of a broader rule is refused, naming it.
func RemoveSync(data []byte, p string) (out []byte, msg string, err error) {
	rel, err := HomeRel(p)
	if err != nil {
		return nil, "", err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	done := false
	for _, b := range blocks(lines) {
		for j := len(b.subs) - 1; j >= 0; j-- {
			if s := b.subs[j]; s.rel == rel {
				lines = slices.Delete(lines, s.start, trimEnd(lines, s.start, s.end))
				done = true
				break
			}
		}
		if !done && b.rel == rel && b.sync >= 0 {
			lines = slices.Delete(lines, b.sync, b.sync+1)
			done = true
		}
		if done {
			break
		}
	}
	if !done {
		if r, ok := c.Match(rel); ok {
			return nil, "", fmt.Errorf("~/%s syncs as part of %s; remove that, or add an exclude to it in %s", rel, r.Path, HomePath)
		}
		return nil, "", fmt.Errorf("~/%s: %w", rel, ErrNotSynced)
	}
	out = []byte(strings.Join(lines, "\n") + "\n")
	nc, err := Parse(out)
	if err != nil {
		return nil, "", fmt.Errorf("the edit would not parse (%v); edit %s by hand", err, HomePath)
	}
	msg = fmt.Sprintf("~/%s no longer syncs; its files stay, here and on every machine", rel)
	if r, ok := nc.Match(rel); ok {
		msg = fmt.Sprintf("removed the rule for ~/%s, but it still syncs as part of %s", rel, r.Path)
	}
	return out, msg, nil
}
