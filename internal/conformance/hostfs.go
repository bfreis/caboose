package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
)

// The real Host's file work under B: the stubs, a canary's contents, a
// directory canary filled, and a checkpoint's copy read. All of it from
// the host side, through nofollow, bounded.

// maxCanaryRead bounds what ReadCanary reads of one file.
const maxCanaryRead = 1 << 20

// maxStubLog bounds what StubCalls reads of the stubs' log.
const maxStubLog = 1 << 20

// maxPopulate bounds how many files Populate writes.
const maxPopulate = 10000

// StubNames are the programs the suite stubs: what the host's link and
// daemon run to open a URL, show a notification or ask with a dialog, on
// a Mac (open, osascript) and on Linux (xdg-open, notify-send, zenity,
// kdialog).
var StubNames = []string{"open", "osascript", "xdg-open", "notify-send", "zenity", "kdialog"}

// stubScript is the stub called name: it appends a line to the log and
// shows nothing. The openers write their first argument after their name
// on that line (the URL the host asked them to open: StubOpened). The
// dialogs exit 1, which their callers take as declined, so that no stub
// ever says yes to anything.
func (g *Guard) stubScript(name string) (string, error) {
	logPath := path.Join(g.base, StubLog)
	if !hostPathOK(logPath) {
		return "", fmt.Errorf("conformance: %q is not a path a stub may write", logPath)
	}
	exit := "0"
	line := "printf '" + name + "\\n'"
	switch name {
	case "zenity", "kdialog":
		exit = "1"
	case "open", "xdg-open":
		line = "printf '" + name + " %s\\n' \"$1\""
	case "osascript", "notify-send":
	default:
		return "", fmt.Errorf("conformance: no stub %q", name)
	}
	return scriptHeader + line + " >> '" + logPath + "'\nexit " + exit + "\n", nil
}

// StubOpened are the URLs the stub openers (open, xdg-open) recorded,
// oldest first.
func (g *Guard) StubOpened() ([]string, error) {
	if g == nil || !g.made {
		return nil, refuse("guard", "not an environment this suite created")
	}
	b, _, err := nofollow.Dir(g.base).ReadFileMax(StubLog, maxStubLog)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		for _, opener := range []string{"open ", "xdg-open "} {
			if u, ok := strings.CutPrefix(l, opener); ok {
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// StubPath is the host directory of the stubs (B/canary/bin), the first
// entry of the daemon's PATH.
func (g *Guard) StubPath() string { return path.Join(g.base, StubDir) }

// WriteStubs writes the stubs into B/canary/bin and an empty log, from the
// host and never through a symlink. Once per run: neither may exist yet.
func (g *Guard) WriteStubs() error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	d := nofollow.Dir(g.base)
	for _, rel := range []string{StubDir, StubLog} {
		if _, err := d.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("conformance: %s exists already", rel)
		}
	}
	if err := mkdirNoFollow(g.base, StubDir); err != nil {
		return err
	}
	for _, n := range StubNames {
		s, err := g.stubScript(n)
		if err != nil {
			return err
		}
		if err := d.WriteFile(StubDir+"/"+n, []byte(s), 0o755); err != nil {
			return err
		}
	}
	return d.WriteFile(StubLog, nil, 0o600)
}

// CheckStubs is nil when every stub is in B/canary/bin as WriteStubs wrote
// it: a plain executable file with the stub's text; else why not.
func (g *Guard) CheckStubs() error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	d := nofollow.Dir(g.base)
	es, err := d.ReadDir(StubDir)
	if err != nil {
		return fmt.Errorf("the stubs' directory: %v", err)
	}
	// Beside the stubs it holds the suite's alias scripts; nothing in it
	// is a link or a directory.
	for _, e := range es {
		if !e.Type().IsRegular() {
			return fmt.Errorf("the stubs' directory holds %s, which is not a plain file", e.Name())
		}
	}
	for _, n := range StubNames {
		want, err := g.stubScript(n)
		if err != nil {
			return err
		}
		b, mode, err := d.ReadFileMax(StubDir+"/"+n, 4096)
		if err != nil {
			return fmt.Errorf("the stub %s: %v", n, err)
		}
		if string(b) != want || mode.Perm()&0o111 == 0 || mode.Perm()&0o022 != 0 {
			return fmt.Errorf("the stub %s is not the suite's", n)
		}
	}
	if fi, err := d.Lstat(StubLog); err != nil || !fi.Mode().IsRegular() {
		return errors.New("the stubs' log is not a plain file")
	}
	return nil
}

// StubCalls is how many times the stubs have been called: the lines of
// their log.
func (g *Guard) StubCalls() (int, error) {
	if g == nil || !g.made {
		return 0, refuse("guard", "not an environment this suite created")
	}
	b, _, err := nofollow.Dir(g.base).ReadFileMax(StubLog, maxStubLog)
	if err != nil {
		return 0, err
	}
	return bytes.Count(b, []byte("\n")), nil
}

// ReadCanary reads the regular file rel (relative to B), a canary path or
// below one, without following a symlink in any component, at most 1 MiB.
// A file that is not there is an error wrapping fs.ErrNotExist.
func (h *GuardHost) ReadCanary(rel string) ([]byte, error) {
	if h.G == nil || !h.G.made {
		return nil, refuse("guard", "not an environment this suite created")
	}
	if !cleanRel(rel) || !h.inCanary(rel) {
		return nil, fmt.Errorf("conformance: %s is not a canary of this run", rel)
	}
	b, _, err := nofollow.Dir(h.G.base).ReadFileMax(rel, maxCanaryRead)
	return b, err
}

// Populate fills the directory canary c with n files of its content,
// named after it, from the host and never through a symlink.
func (h *GuardHost) Populate(c Canary, n int) error {
	if h.G == nil || !h.G.made {
		return refuse("guard", "not an environment this suite created")
	}
	if n < 1 || n > maxPopulate {
		return fmt.Errorf("conformance: %d files is not a number Populate writes", n)
	}
	h.G.mu.Lock()
	rec, ok := h.G.canaries[c.Rel]
	h.G.mu.Unlock()
	if !ok || rec.canary.Rel != c.Rel || rec.canary.Kind != Dir {
		return fmt.Errorf("conformance: %s is not a directory canary of this run", c.Rel)
	}
	d := nofollow.Dir(h.G.base)
	if fi, err := d.Lstat(c.Rel); err != nil || !fi.IsDir() {
		return fmt.Errorf("conformance: %s is not a directory", c.Rel)
	}
	content := h.G.CanaryContent(rec.canary.Role)
	base := path.Base(c.Rel)
	for i := 1; i <= n; i++ {
		f := c.Rel + "/" + base + "-p" + strconv.Itoa(i)
		if _, err := d.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("conformance: %s exists already", f)
		}
		if err := d.WriteFile(f, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// StoreTree is the identity of every path below top (relative to B, a
// checkpoint's copy inside the environment's checkpoint store), by its
// path relative to top, slash-separated, top left out; read without
// following a symlink.
func (g *Guard) StoreTree(top string) (map[string]Identity, error) {
	if g == nil || !g.made {
		return nil, refuse("guard", "not an environment this suite created")
	}
	store := g.StoreRel()
	if !cleanRel(top) || !strings.HasPrefix(top, store+"/") {
		return nil, fmt.Errorf("conformance: %s is not in the environment's checkpoint store", top)
	}
	d := nofollow.Dir(g.base)
	if fi, err := d.Lstat(top); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("conformance: %s is not a directory", top)
	}
	out := map[string]Identity{}
	err := d.Walk(top, func(rel string, _ fs.DirEntry) error {
		if rel == top {
			return nil
		}
		if len(out) >= maxWalkEntries {
			return fmt.Errorf("conformance: %s holds too many entries", top)
		}
		if strings.Count(rel, "/") >= maxWalkDepth {
			return fmt.Errorf("conformance: %s is too deep", top)
		}
		id, err := identityOf(d, rel)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(rel, top+"/")] = id
		return nil
	})
	return out, err
}

// cleanRel reports whether rel is a clean relative slash path inside B.
func cleanRel(rel string) bool {
	return rel != "" && path.Clean(rel) == rel && !path.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, "../")
}
