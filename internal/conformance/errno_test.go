package conformance_test

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/shelltest"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   int
		msg  string
		want c.ErrClass
	}{
		{"success is never a refusal", 0, "", c.ClassOK},
		{"success with a refusal-looking line", 0, "rm: cannot remove 'x': Read-only file system", c.ClassOK},
		{"erofs", 1, "rm: cannot remove '/work/a': Read-only file system", c.ClassErofs},
		{"erofs from a redirect", 2, "sh: 1: cannot create /work/a: Read-only file system", c.ClassErofs},
		{"eacces", 1, "touch: cannot touch '/work/a': Permission denied", c.ClassEacces},
		{"eperm", 32, "umount: /work/a: Operation not permitted", c.ClassEperm},
		{"enoent", 1, "cat: /work/a: No such file or directory", c.ClassEnoent},
		{"ebusy", 32, "umount: /work/a: Device or resource busy", c.ClassEbusy},
		{"enotempty", 1, "rm: cannot remove '/work/a': Directory not empty", c.ClassEnotempty},
		{"exdev", 1, "mv: cannot move 'a' to 'b': Invalid cross-device link", c.ClassExdev},
		{"enoent from BusyBox's redirect", 1, "/bin/sh: line 0: can't create /work/../canary/x: nonexistent directory", c.ClassEnoent},
		{"enoent from BusyBox's input redirect", 1, "/bin/sh: line 0: can't open /canary/x: no such file", c.ClassEnoent},
		{"enoent from dash's redirect", 2, "sh: 1: cannot create /canary/x: Directory nonexistent", c.ClassEnoent},
		{"enoent from dash's input redirect", 2, "sh: 1: cannot open /canary/x: No such file", c.ClassEnoent},
		{"eacces from BusyBox's redirect", 1, "/bin/sh: line 0: can't create /work/a: Permission denied", c.ClassEacces},
		{"two classes, one of them the shell's", 1, "nonexistent directory: Permission denied", c.ClassOther},
		{"no message", 1, "", c.ClassOther},
		{"unknown message", 1, "rm: cannot remove 'a': Is a directory", c.ClassOther},
		{"lower case is not strerror", 1, "rm: read-only file system", c.ClassOther},
		{"longer word", 1, "rm: Read-only file systems", c.ClassOther},
		{"text file busy is not busy", 1, "sh: Text file busy", c.ClassOther},
		{"two errors are none", 1, "Permission denied: Read-only file system", c.ClassOther},
		{"shell could not run it", 127, "sh: 1: rm: not found", c.ClassOther},
		{"shell could not run it with enoent", 127, "No such file or directory", c.ClassOther},
		{"not executable", 126, "Permission denied", c.ClassOther},
		{"not the wrapper", -1, "Read-only file system", c.ClassOther},
		{"operation not permitted is not permission denied", 1, "Operation not permitted", c.ClassEperm},
		{"permission denied is not operation not permitted", 1, "Permission denied", c.ClassEacces},
	} {
		if got := c.Classify(tc.rc, tc.msg); got != tc.want {
			t.Errorf("%s: Classify(%d, %q) = %s, want %s", tc.name, tc.rc, tc.msg, got, tc.want)
		}
	}
}

// A redirection into a directory that is not there is enoent whatever the
// shell: each POSIX shell this machine has says it its own way.
func TestClassifyShellRedirects(t *testing.T) {
	dir := t.TempDir()
	for _, sh := range shelltest.Shells(t) {
		for _, form := range []string{"echo x >> %s/gone/x", "echo x > %s/gone/x", "cat < %s/gone/x"} {
			script := fmt.Sprintf(form, dir)
			cmd := exec.Command(sh[0], append(sh[1:], "-c", "LC_ALL=C; export LC_ALL; "+script)...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("%v %q: %v", sh, script, err)
			}
			msg := strings.SplitN(strings.TrimSpace(stderr.String()), "\n", 2)[0]
			if got := c.Classify(ee.ExitCode(), msg); got != c.ClassEnoent {
				t.Errorf("%v %q: %q is %s, want enoent", sh, form, msg, got)
			}
		}
	}
}

func TestParseWrapped(t *testing.T) {
	for _, tc := range []struct {
		name, out, errOut string
		rc                int
		msg               string
		class             c.ErrClass
	}{
		{"refused", "\nrc=1\n", "rm: cannot remove 'a': Read-only file system\n", 1, "rm: cannot remove 'a': Read-only file system", c.ClassErofs},
		{"ok", "\nrc=0\n", "", 0, "", c.ClassOK},
		{"first line of stderr", "\nrc=1\n", "\nPermission denied\nRead-only file system\n", 1, "Permission denied", c.ClassEacces},
		{"stdout text is not the message", "\nrc=1\n", "", 1, "", c.ClassOther},
		{"no rc line", "Read-only file system\n", "", -1, "", c.ClassOther},
		{"empty", "", "Permission denied", -1, "", c.ClassOther},
		{"junk before rc", "rc=0\n\nrc=1\n", "", -1, "", c.ClassOther},
		{"junk after rc", "rc=0\nPermission denied\n", "", -1, "", c.ClassOther},
		{"rc too long", "\nrc=1000\n", "", -1, "", c.ClassOther},
		{"rc with a sign", "\nrc=-1\n", "", -1, "", c.ClassOther},
		{"crlf", "\r\nrc=1\r\n", "Permission denied\r\n", 1, "Permission denied", c.ClassEacces},
		{"rc inside a line", "x rc=0\n", "", -1, "", c.ClassOther},
	} {
		got := c.ParseWrapped(tc.out, tc.errOut)
		if got.RC != tc.rc || got.Msg != tc.msg || got.Class != tc.class {
			t.Errorf("%s: %+v, want rc=%d msg=%q class=%s", tc.name, got, tc.rc, tc.msg, tc.class)
		}
	}
}
