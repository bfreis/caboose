package assets

import (
	"fmt"
	"regexp"
	"strings"
)

// SectionName is what a section's name may be: the NAME in its marker.
var SectionName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// instructionRE finds a Dockerfile instruction a proposed section may not
// hold, at the start of any line -- also one continued from the line
// before, where it would be an argument, since telling the two apart takes
// a whole parser. FROM would start a new stage, putting the base in the
// section's hands, and ONBUILD would run in the layer built on it.
var instructionRE = regexp.MustCompile(`(?im)^\s*(FROM|ONBUILD)\b`)

// runFlagRE finds the RUN flags that reach past the build's own sandbox:
// the host's network, or an insecure build.
var runFlagRE = regexp.MustCompile(`--(network|security)\b`)

// CheckSectionBody says why body cannot be a section's body, or nil: it
// must not mark sections itself, nor start a stage, nor take a RUN out of
// the build's sandbox.
func CheckSectionBody(body string) error {
	for i, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "# caboose:") {
			return fmt.Errorf("line %d: a section's body cannot hold a caboose marker", i+1)
		}
	}
	if m := instructionRE.FindStringSubmatch(body); m != nil {
		return fmt.Errorf("a section cannot hold %s: the base is not a section's to change", strings.ToUpper(m[1]))
	}
	if m := runFlagRE.FindStringSubmatch(body); m != nil {
		return fmt.Errorf("a section cannot use --%s: it would reach past the build's sandbox", m[1])
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("the section's body is empty")
	}
	return nil
}

// SetSection puts section name, titled title, with body, into dockerfile:
// in place of a section of that name, where it was, or else at the end.
// Everything else stays as it is.
func SetSection(dockerfile []byte, name, title, body string) ([]byte, error) {
	switch {
	case !SectionName.MatchString(name):
		return nil, fmt.Errorf("%q is not a section name: lowercase letters, digits and -, at most 32", name)
	case strings.TrimSpace(title) == "" || strings.Contains(title, "\n"):
		return nil, fmt.Errorf("a section's title is one line")
	case title == "off" || strings.HasPrefix(title, "off "):
		// Read back, the marker would say the section is off.
		return nil, fmt.Errorf("a section's title cannot start with \"off\"")
	}
	if err := CheckSectionBody(body); err != nil {
		return nil, err
	}
	blocks, err := parseSections(dockerfile)
	if err != nil {
		return nil, err
	}
	sec := block{
		sec:   &Section{Name: name, Title: title, Default: true},
		lines: append(append([]string{"# caboose:section " + name + " " + title}, strings.Split(strings.TrimSuffix(body, "\n"), "\n")...), "# caboose:end"),
	}
	var lines []string
	placed := false
	for _, b := range blocks {
		if b.sec != nil && b.sec.Name == name {
			b, placed = sec, true
		}
		lines = append(lines, b.lines...)
	}
	if !placed {
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(append(lines, ""), sec.lines...)
	}
	out := strings.Join(lines, "\n") + "\n"
	// What was written must read back as it was meant.
	if _, err := parseSections([]byte(out)); err != nil {
		return nil, fmt.Errorf("the Dockerfile would not read back: %v", err)
	}
	return []byte(out), nil
}
