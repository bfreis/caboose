package proposal

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/nofollow"
)

// What checking a proposal found is written beside it, for the session
// that wrote it to read: NAME.check for NAME.toml, a short text file whose
// first line is its status (CheckOK, CheckError or CheckUnchecked) and
// whose every other line is one fact. The host writes it and never reads
// it back but for its time; the session can write one too, which says
// nothing to the host.

// CheckStatus is a check file's first line.
type CheckStatus string

const (
	// CheckOK: the proposal resolves; the facts say to what.
	CheckOK CheckStatus = "ok"
	// CheckError: the proposal is wrong as it stands, and the facts say
	// why; the session fixes it.
	CheckError CheckStatus = "error"
	// CheckUnchecked: the host could not check it now (no budget left,
	// the network, a timeout, its own configuration); nothing is known
	// to be wrong with it, and caboose apply checks it anyway.
	CheckUnchecked CheckStatus = "unchecked"
)

const (
	// CheckExt is the extension of what checking a proposal found.
	CheckExt = ".check"
	// checkLines is the most lines of facts a check file has, and
	// checkLineMax the most characters in one.
	checkLines   = 20
	checkLineMax = 400
)

// CheckFile is the name of the check file for proposal file.
func CheckFile(file string) string {
	return strings.TrimSuffix(file, Ext) + CheckExt
}

// FormatCheck is a check file's contents: status, then facts, a line
// each. Every line is made printable (Printable) and cut
// short, and there are a bounded number of them: a fact may quote what the
// sandbox wrote.
func FormatCheck(status CheckStatus, facts []string) []byte {
	var b strings.Builder
	b.WriteString(string(status))
	b.WriteByte('\n')
	n := 0
	for _, f := range facts {
		for _, l := range strings.Split(f, "\n") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			if n == checkLines {
				b.WriteString("...\n")
				return []byte(b.String())
			}
			n++
			b.WriteString(clip(Printable(l), checkLineMax))
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// clip is s cut to max runes, "..." marking the cut.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}

// WriteCheck writes what checking proposal file found (FormatCheck)
// beside it, through nofollow: whatever the sandbox put at that name, a
// symlink say, is replaced, never written through.
func WriteCheck(dataDir, file string, status CheckStatus, facts []string) error {
	return nofollow.Dir(dataDir).WriteFile(path.Join(Dir, CheckFile(file)), FormatCheck(status, facts), 0o644)
}

// RemoveCheck removes proposal file's check file, if there is one.
func RemoveCheck(dataDir, file string) error {
	err := nofollow.Dir(dataDir).Remove(path.Join(Dir, CheckFile(file)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// CheckModified is when proposal file's check file last changed, and
// whether there is one, a plain file.
func CheckModified(dataDir, file string) (time.Time, bool) {
	fi, err := nofollow.Dir(dataDir).Lstat(path.Join(Dir, CheckFile(file)))
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}
