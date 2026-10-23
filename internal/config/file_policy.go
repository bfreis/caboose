package config

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on what a root's writable list may hold.
const (
	MaxWritable    = 64
	maxWritableLen = 1024
)

// printableText reports whether s is text a person can read where it is
// shown: valid UTF-8 with no control, format (bidi) or line-separator
// characters.
func printableText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// CheckWritable checks a root's writable list: at most MaxWritable
// entries, each a path relative to the root, clean, with no .. component,
// no leading /, no control character and no : or , (docker's -v syntax),
// none twice and none inside another; "." (the whole root) is the only
// entry when it is there. Whether each exists, and is no symlink, is
// checked when the root is resolved (ResolveRoots).
func CheckWritable(list []string) error {
	if len(list) > MaxWritable {
		return fmt.Errorf("writable lists %d paths, at most %d", len(list), MaxWritable)
	}
	for _, e := range list {
		switch {
		case e == "":
			return errors.New(`writable has an empty path (use "." for the whole root)`)
		case len(e) > maxWritableLen:
			return fmt.Errorf("writable path of %d bytes is too long", len(e))
		case strings.HasPrefix(e, "/"):
			return fmt.Errorf("writable %q must be relative to the root, not absolute", e)
		case !printableText(e):
			return fmt.Errorf("writable %q has a control or invisible character", e)
		case strings.ContainsAny(e, ":,\\"):
			return fmt.Errorf(`writable %q has a ":", "," or "\\"`, e)
		case slices.Contains(strings.Split(e, "/"), ".."):
			return fmt.Errorf(`writable %q has a ".." component`, e)
		case path.Clean(e) != e:
			return fmt.Errorf("writable %q is not a clean path (%q is)", e, path.Clean(e))
		}
	}
	if len(list) > 1 && slices.Contains(list, ".") {
		return errors.New(`writable "." is the whole root and must be the only entry`)
	}
	for i, a := range list {
		for j, b := range list {
			switch {
			case i < j && a == b:
				return fmt.Errorf("writable lists %q twice", a)
			case i != j && a != b && strings.HasPrefix(b, a+"/"):
				return fmt.Errorf("writable %q is inside writable %q, which already covers it", b, a)
			}
		}
	}
	return nil
}
