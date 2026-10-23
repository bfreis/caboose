// Package checkpoint keeps copies of a sandbox's writable paths on the
// host, in a store under the environment's directory that no sandbox
// mounts: the store's layout, its lock, listing, retention and pruning,
// and taking a checkpoint (Take), which copies a source the sandbox can
// write without ever following a symlink in it.
//
// The store holds one directory per configured path (its key), each with
// one directory per checkpoint (its stamp):
//
//	checkpoints/                      0700
//	  lock                            flock: every take, prune and restore
//	  checkpoints.log                 capped
//	  <key>/                          0700
//	    path.json                     {"version":1,"path":"..."}
//	    20261017T231005Z/             one checkpoint, complete
//	      meta.json
//	      tree
//	  .tmp-<key>-<stamp>-<rand>/      being taken, never listed
//	  .trash-<key>-<stamp>-<rand>/    being removed, never listed
//
// A checkpoint appears whole, by a rename of its .tmp- directory, and goes
// whole, by a rename to .trash- before its removal, so a listing never sees
// half of one.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// StampLayout is a checkpoint's stamp: the UTC second it was taken.
const StampLayout = "20060102T150405Z"

// Latest is the stamp an ID gives to name the newest checkpoint of a key.
const Latest = "latest"

// maxKeyBase is the most bytes of a key before its hash.
const maxKeyBase = 32

// Key is the store's directory name for the configured path p ("~/.claude",
// "/work/dev/x"): p's last element, lowercased, every byte outside
// [a-z0-9._-] replaced by '-', leading dots dropped, cut to 32 bytes ("path"
// when nothing is left), then '-' and the first 8 hex digits of p's sha256.
// The hash keeps two paths with the same last element apart; path.json in
// the key's directory says which path it is for.
func Key(p string) string {
	base := path.Base(strings.TrimRight(p, "/"))
	var b strings.Builder
	for i := 0; i < len(base); i++ {
		c := base[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if !keyByte(c) {
			c = '-'
		}
		b.WriteByte(c)
	}
	s := strings.TrimLeft(b.String(), ".")
	if len(s) > maxKeyBase {
		s = s[:maxKeyBase]
	}
	if s == "" {
		s = "path"
	}
	sum := sha256.Sum256([]byte(p))
	return s + "-" + hex.EncodeToString(sum[:4])
}

func keyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// ValidKey says whether k has a key's form: 1 to 32 bytes of [a-z0-9._-]
// not starting with a dot, '-', and 8 lowercase hex digits. A valid key is
// one plain path element, never "." or "..".
func ValidKey(k string) bool {
	n := len(k)
	if n < 10 || n > maxKeyBase+9 || k[n-9] != '-' {
		return false
	}
	for i := n - 8; i < n; i++ {
		if c := k[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	base := k[:n-9]
	if base[0] == '.' {
		return false
	}
	for i := 0; i < len(base); i++ {
		if !keyByte(base[i]) {
			return false
		}
	}
	return true
}

// FormatStamp is t's stamp: its UTC second.
func FormatStamp(t time.Time) string { return t.UTC().Format(StampLayout) }

// ParseStamp parses a stamp strictly: exactly the layout, a real UTC
// instant, and nothing that formats back to another string.
func ParseStamp(s string) (time.Time, error) {
	if len(s) != len(StampLayout) {
		return time.Time{}, fmt.Errorf("checkpoint: bad stamp %q", s)
	}
	for i := 0; i < len(s); i++ {
		c, l := s[i], StampLayout[i]
		if l == 'T' || l == 'Z' {
			if c != l {
				return time.Time{}, fmt.Errorf("checkpoint: bad stamp %q", s)
			}
		} else if c < '0' || c > '9' {
			return time.Time{}, fmt.Errorf("checkpoint: bad stamp %q", s)
		}
	}
	t, err := time.ParseInLocation(StampLayout, s, time.UTC)
	if err != nil || FormatStamp(t) != s {
		return time.Time{}, fmt.Errorf("checkpoint: bad stamp %q", s)
	}
	return t, nil
}

// ID names a checkpoint: KEY@STAMP, or KEY@latest for a key's newest.
type ID struct {
	Key    string
	Stamp  time.Time // zero when Latest
	Latest bool
}

// String is the ID as the user sees and gives it.
func (id ID) String() string {
	if id.Latest {
		return id.Key + "@" + Latest
	}
	return id.Key + "@" + FormatStamp(id.Stamp)
}

// ParseID parses an ID strictly, before any path is built from it: a valid
// key, '@', and a stamp that parses (or "latest"); anything else is refused.
func ParseID(s string) (ID, error) {
	k, st, ok := strings.Cut(s, "@")
	if !ok || !ValidKey(k) {
		return ID{}, fmt.Errorf("checkpoint: bad checkpoint id %q: want KEY@STAMP or KEY@latest", s)
	}
	if st == Latest {
		return ID{Key: k, Latest: true}, nil
	}
	t, err := ParseStamp(st)
	if err != nil {
		return ID{}, fmt.Errorf("checkpoint: bad checkpoint id %q: want KEY@STAMP or KEY@latest", s)
	}
	return ID{Key: k, Stamp: t}, nil
}
