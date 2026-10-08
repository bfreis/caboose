package assets

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	caboose "github.com/bfreis/caboose"
)

// The seed is the embedded Dockerfile cut down to the sections chosen:
// what caboose setup writes into a dockerfile image profile's dir. The
// Dockerfile marks its optional parts itself, between a
//
//	# caboose:section NAME [off] TITLE
//
// line and a "# caboose:end" one. Everything outside a section -- the
// base, the OS packages the image check requires -- is always kept. An
// "off" section is commented out line by line in the Dockerfile, so that
// the Dockerfile builds without it; chosen, it is uncommented.

// Section is one optional part of the image.
type Section struct {
	Name, Title string
	// Default is whether the default seed has it: every section but an off
	// one.
	Default bool
}

var (
	sectionRE = regexp.MustCompile(`^# caboose:section ([a-z0-9-]+)( off)? (.+)$`)
	endRE     = regexp.MustCompile(`^# caboose:end$`)
)

// block is a stretch of the Dockerfile: outside every section (sec nil),
// or one section with its marker lines.
type block struct {
	sec   *Section
	lines []string
}

// parseSections splits a Dockerfile into blocks, checking the markers are
// well formed: no section inside another, every one ended, names unique,
// an off section's body all comment lines.
func parseSections(data []byte) ([]block, error) {
	var blocks []block
	cur := &block{}
	seen := map[string]bool{}
	for i, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch m := sectionRE.FindStringSubmatch(l); {
		case m != nil:
			if cur.sec != nil {
				return nil, fmt.Errorf("line %d: section %s inside section %s", i+1, m[1], cur.sec.Name)
			}
			if seen[m[1]] {
				return nil, fmt.Errorf("line %d: section %s again", i+1, m[1])
			}
			seen[m[1]] = true
			blocks = append(blocks, *cur)
			cur = &block{sec: &Section{Name: m[1], Title: m[3], Default: m[2] == ""}, lines: []string{l}}
			continue
		case endRE.MatchString(l):
			if cur.sec == nil {
				return nil, fmt.Errorf("line %d: caboose:end outside a section", i+1)
			}
			cur.lines = append(cur.lines, l)
			blocks = append(blocks, *cur)
			cur = &block{}
			continue
		case strings.HasPrefix(l, "# caboose:"):
			return nil, fmt.Errorf("line %d: unknown marker %q", i+1, l)
		}
		if cur.sec != nil && !cur.sec.Default && l != "#" && !strings.HasPrefix(l, "# ") {
			return nil, fmt.Errorf("line %d: section %s is off, so every line in it must be commented out", i+1, cur.sec.Name)
		}
		cur.lines = append(cur.lines, l)
	}
	if cur.sec != nil {
		return nil, fmt.Errorf("section %s is never ended", cur.sec.Name)
	}
	return append(blocks, *cur), nil
}

func embeddedDockerfile() []byte {
	data, err := fs.ReadFile(caboose.Files, "Dockerfile")
	if err != nil {
		panic(err) // a broken build of the launcher; TestSections guards it
	}
	return data
}

// Sections are the embedded Dockerfile's optional parts, in its order.
func Sections() []Section {
	blocks, err := parseSections(embeddedDockerfile())
	if err != nil {
		panic(err)
	}
	var out []Section
	for _, b := range blocks {
		if b.sec != nil {
			out = append(out, *b.sec)
		}
	}
	return out
}

// DefaultSections are the names of the sections the default seed has.
func DefaultSections() []string {
	var out []string
	for _, s := range Sections() {
		if s.Default {
			out = append(out, s.Name)
		}
	}
	return out
}

// seedHeader is what a written seed starts with: plain comments, which
// nothing reads back.
const seedHeader = `# Written by caboose from the Dockerfile it embeds. It is yours now: a
# newer caboose never changes it. This directory is the build context, so
# files next to it can be COPYed. After an edit, 'caboose build' rebuilds
# the image, and 'caboose restart' moves the container onto it.
#
`

// Seed is the embedded Dockerfile with only the named sections, an off
// one uncommented, under a few lines saying whose file it is now: what
// caboose writes into a dockerfile image profile's dir that has no
// Dockerfile yet. With DefaultSections, the body is the embedded
// Dockerfile without its off sections.
func Seed(sections []string) ([]byte, error) {
	blocks, err := parseSections(embeddedDockerfile())
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, b := range blocks {
		if b.sec != nil {
			known[b.sec.Name] = true
		}
	}
	for _, n := range sections {
		if !known[n] {
			return nil, fmt.Errorf("no section %q in the embedded Dockerfile", n)
		}
	}
	var body []string
	for _, b := range blocks {
		switch {
		case b.sec == nil:
			body = append(body, b.lines...)
		case !slices.Contains(sections, b.sec.Name):
		case b.sec.Default:
			body = append(body, b.lines...)
		default:
			body = append(body, "# caboose:section "+b.sec.Name+" "+b.sec.Title)
			for _, l := range b.lines[1 : len(b.lines)-1] {
				body = append(body, strings.TrimPrefix(strings.TrimPrefix(l, "#"), " "))
			}
			body = append(body, b.lines[len(b.lines)-1])
		}
	}
	return []byte(seedHeader + strings.Join(body, "\n") + "\n"), nil
}
