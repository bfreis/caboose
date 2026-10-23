package conformance

import (
	"regexp"
	"strconv"
	"strings"
)

// ErrClass is what a sandbox-side command's failure was, read from the
// message the shell printed.
type ErrClass string

const (
	ClassOK        ErrClass = "ok" // the command succeeded: never a refusal
	ClassErofs     ErrClass = "erofs"
	ClassEacces    ErrClass = "eacces"
	ClassEperm     ErrClass = "eperm"
	ClassEnoent    ErrClass = "enoent"
	ClassEbusy     ErrClass = "ebusy"
	ClassEnotempty ErrClass = "enotempty"
	ClassExdev     ErrClass = "exdev"
	ClassOther     ErrClass = "other" // anything else, including no message
)

// refusalClass reports whether c is a class that can stand for a refusal:
// ok, enoent and other never do.
func refusalClass(c ErrClass) bool {
	switch c {
	case ClassErofs, ClassEacces, ClassEperm, ClassEbusy, ClassEnotempty, ClassExdev:
		return true
	}
	return false
}

// phrases are strerror's messages, matched exactly (the shell runs with
// LC_ALL=C), as whole words of the line.
var phrases = []struct {
	class ErrClass
	re    *regexp.Regexp
}{
	{ClassErofs, phrase("Read-only file system")},
	{ClassEacces, phrase("Permission denied")},
	{ClassEperm, phrase("Operation not permitted")},
	{ClassEnoent, phrase("No such file or directory")},
	// The ash family of shells says these for a redirection in place of
	// strerror's ENOENT, and of ENOTDIR, which a path that leads nowhere
	// is as much. BusyBox's ash (many a base's /bin/sh): "can't create P:
	// nonexistent directory" for > and >>, "can't open P: no such file"
	// for <; dash (Debian's): "cannot create P: Directory nonexistent",
	// "cannot open P: No such file".
	{ClassEnoent, phrase("nonexistent directory")},
	{ClassEnoent, phrase("no such file")},
	{ClassEnoent, phrase("Directory nonexistent")},
	{ClassEnoent, phrase("No such file")},
	{ClassEbusy, phrase("Device or resource busy")},
	{ClassEnotempty, phrase("Directory not empty")},
	{ClassExdev, phrase("Invalid cross-device link")},
}

func phrase(s string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z])` + regexp.QuoteMeta(s) + `($|[^A-Za-z])`)
}

// Classify maps a command's exit status and the first line of its error
// message to a class. A status of 0 is ok whatever was printed; a negative
// status stands for output that was not the wrapper's. Statuses 126 and
// 127 (the shell could not run the command) are other, as is a message
// that names no error, or errors of more than one class.
func Classify(rc int, msg string) ErrClass {
	switch {
	case rc == 0:
		return ClassOK
	case rc < 0, rc == 126, rc == 127:
		return ClassOther
	}
	found := ClassOther
	for _, p := range phrases {
		if !p.re.MatchString(msg) {
			continue
		}
		if found != ClassOther && found != p.class {
			return ClassOther
		}
		found = p.class
	}
	return found
}

// ExecResult is what a wrapped command came to.
type ExecResult struct {
	RC    int // -1 when the output was not the wrapper's
	Msg   string
	Out   string
	Class ErrClass
}

var rcLine = regexp.MustCompile(`^rc=([0-9]{1,3})$`)

// ParseWrapped reads a wrapped command's result: stdout must hold only the
// wrapper's rc=<n> line (the wrapper discards the command's own stdout),
// and the message is the first non-empty line of stderr. Anything else is
// RC -1, class other: the shell did not run the wrapper.
func ParseWrapped(stdout, stderr string) ExecResult {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r", ""), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	res := ExecResult{RC: -1, Class: ClassOther}
	if len(lines) == 0 {
		return res
	}
	m := rcLine.FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		return res
	}
	rc, err := strconv.Atoi(m[1])
	if err != nil {
		return res
	}
	if strings.TrimSpace(strings.Join(lines[:len(lines)-1], "")) != "" {
		return res
	}
	res.RC = rc
	errLines := strings.Split(strings.ReplaceAll(stderr, "\r", ""), "\n")
	for _, l := range errLines {
		if strings.TrimSpace(l) != "" {
			res.Msg = l
			break
		}
	}
	res.Out = strings.TrimRight(strings.Join(errLines, "\n"), "\n")
	res.Class = Classify(rc, res.Msg)
	return res
}
