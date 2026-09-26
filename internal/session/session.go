// Package session names tmux sessions and picks the one a terminal attaches
// to.
//
// A second terminal in the same project gets its own session, not a second
// view of the first one. Plain `caboose` takes the first name in the series
// -- <project>, <project>-2, <project>-3 -- whose session is free, meaning
// absent or orphaned (its terminal was closed). So the first terminal
// creates <project>, a second one opened alongside it creates <project>-2,
// and either, reopened after being closed, reattaches to the session it left
// behind.
//
// --session NAME (or CABOOSE_SESSION=NAME) names a session outright and
// skips that search: it attaches if the name exists and creates it
// otherwise, which is also the way to mirror one session onto two terminals
// on purpose (point both at the same name).
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MaxSeries bounds the search for a free session name.
const MaxSeries = 64

// ErrTooMany is returned when every name in the series is taken.
var ErrTooMany = errors.New("64 live sessions on this project already; name one with --session")

// sanitize maps every character outside [A-Za-z0-9_-] to '-', as
// `tr -c 'A-Za-z0-9_-' '-'` does. tmux treats '.' and ':' as structure, so
// they can't appear in a name.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// NameFor is the tmux session name for a project: a sanitized basename plus a
// short digest of the full path, so two repos with the same basename don't
// collide: /x/caboose is "caboose-<digest>".
//
// The optional suffix is the whole mechanism behind --session: same
// project, a different name, therefore a genuinely separate session instead
// of a second client mirroring the first.
func NameFor(path, suffix string) string {
	sum := sha256.Sum256([]byte(path))
	name := sanitize(filepath.Base(path)) + "-" + hex.EncodeToString(sum[:])[:6]
	if suffix != "" {
		name += "-" + sanitize(suffix)
	}
	return name
}

// ClientCounter reports how many tmux clients are attached to the session
// with exactly that name (0 for an absent session).
type ClientCounter func(name string) int

// CountLines counts the clients in `tmux list-clients` output: one per line.
func CountLines(out string) int {
	return strings.Count(out, "\n")
}

// FirstFree is the first name in the series -- base, base-2, base-3 -- with
// no client on it. A session with a client is one someone is looking at
// right now, and taking it over is the mirror: two terminals on one pane,
// both shrunk to the smaller one's size. A session with none is either
// absent or orphaned by a closed terminal, and both are ours to take, which
// is what keeps detach/reattach working -- a reopened terminal lands back on
// the session it left.
//
// clients must query with an exact '=name' target: tmux matches a -t target
// exactly, then by prefix, then by fnmatch, so a bare '<project>' would
// match '<project>-2' once the exact session is gone, and this search would
// report a free session as busy and vice versa.
func FirstFree(base string, clients ClientCounter) (string, error) {
	candidate := base
	for n := 1; clients(candidate) != 0; {
		n++
		if n > MaxSeries {
			return "", ErrTooMany
		}
		candidate = base + "-" + strconv.Itoa(n)
	}
	return candidate, nil
}

// ProjectSessions filters `tmux list-sessions -F '#{session_name}'` output
// down to every session this project owns: the base name and its numbered
// siblings.
func ProjectSessions(list, base string) []string {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + "(-[0-9]+)?$")
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(list, "\r", ""), "\n") {
		if re.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}
