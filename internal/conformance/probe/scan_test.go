package probe

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The probe runs in a sandbox the suite attacks from: its own source may
// write, remove, execute, read the environment or name a socket by none
// of these routes. Scanned as Go tokens, so comments do not count and
// strings do.

// forbiddenIdents are calls and flags that write, remove, run or read
// what the probe has no business with, whatever they are called on.
var forbiddenIdents = map[string]bool{
	"Remove": true, "RemoveAll": true, "Rename": true, "Unlink": true, "Unlinkat": true, "Rmdir": true,
	"Create": true, "CreateTemp": true, "WriteFile": true, "Mkdir": true, "MkdirAll": true, "MkdirTemp": true,
	"Chmod": true, "Chown": true, "Lchown": true, "Chtimes": true, "Truncate": true, "Symlink": true, "Link": true,
	"O_WRONLY": true, "O_RDWR": true, "O_CREATE": true, "O_TRUNC": true, "O_APPEND": true,
	"Command": true, "CommandContext": true, "StartProcess": true, "ForkExec": true, "Exec": true, "Kill": true,
	"Getenv": true, "LookupEnv": true, "Environ": true, "UserHomeDir": true, "Executable": true,
	"SocketPath": true,
}

// forbiddenStrings may not appear in a string literal.
var forbiddenStrings = []string{
	"agent.sock", ".credentials", ".ssh", ".claude", "SSH_AUTH_SOCK", "$HOME", "/Users/", "/home/",
	"internal/agent\"", "os/exec", "internal/saferm",
}

func scanDir(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, scanFile(filepath.Base(f), src)...)
	}
	return out
}

// mountLinuxAllowed are what the one audited file (mount_linux.go) needs:
// the write-open flags of its witness write, to a validated path, and
// ForkExec, with which it starts the probe itself again in a new user
// namespace (TestMountLinuxExecsOnlyItself holds it to that). Every other
// probe file is held to writing and running nothing.
var mountLinuxAllowed = map[string]bool{"O_WRONLY": true, "O_APPEND": true, "ForkExec": true}

// scanFile scans one file, exempting mount_linux.go from the write-open
// flags alone (it is still held to every other forbidden ident and to
// every forbidden string).
func scanFile(name string, src []byte) []string {
	var out []string
	for _, f := range scan(name, src) {
		if name == "mount_linux.go" {
			if _, sp := cutIdent(f); mountLinuxAllowed[sp] {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// cutIdent splits a "pos: ident" finding into its position and its text.
func cutIdent(f string) (pos, ident string) {
	i := strings.LastIndex(f, ": ")
	if i < 0 {
		return f, ""
	}
	return f[:i], f[i+2:]
}

func scan(name string, src []byte) []string {
	fset := token.NewFileSet()
	file := fset.AddFile(name, -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var out []string
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		where := fset.Position(pos).String()
		switch tok {
		case token.IDENT:
			if forbiddenIdents[lit] {
				out = append(out, where+": "+lit)
			}
		case token.STRING:
			v, err := strconv.Unquote(lit)
			if err != nil {
				v = lit
			}
			for _, f := range forbiddenStrings {
				if strings.Contains(v+"\"", f) {
					out = append(out, where+": "+f)
				}
			}
		}
	}
	return out
}

func TestProbeSourceIsInert(t *testing.T) {
	for _, dir := range []string{".", "cmd/conformance-probe"} {
		for _, f := range scanDir(t, dir) {
			t.Errorf("%s: %s", dir, f)
		}
	}
}

// The scanner itself catches what it is for.
func TestProbeScannerCatches(t *testing.T) {
	for name, src := range map[string]string{
		"remove": `package p; func f() { os.Remove("x") }`,
		"write":  `package p; func f() { os.WriteFile("x", nil, 0) }`,
		"flag":   `package p; var x = os.O_RDWR`,
		"exec":   `package p; import "os/exec"`,
		"env":    `package p; func f() { os.Getenv("X") }`,
		"socket": `package p; const s = "/tmp/x-agent.sock"`,
		"agent":  "package p; import \"x/internal/" + "agent\"",
		"const":  `package p; var s = SocketPath`,
		"creds":  `package p; var s = "x/.credentials.json"`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := scan("f.go", []byte(src)); len(got) == 0 {
				t.Fatal("not caught")
			}
		})
	}
	if got := scan("f.go", []byte(`package p; // os.Remove in a comment
func f() { os.ReadDir("x") }`)); len(got) != 0 {
		t.Fatalf("caught %v", got)
	}
}

// The link socket's path is not in the probe's source in any spelling.
func TestProbeHasNoSocket(t *testing.T) {
	noise := strings.NewReplacer("\"", "", "`", "", "+", "", " ", "", "\t", "", "\n", "")
	for _, dir := range []string{".", "cmd/conformance-probe"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(noise.Replace(string(b)), "agent.sock") {
				t.Errorf("%s names the link socket", f)
			}
		}
	}
}
