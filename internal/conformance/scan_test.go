package conformance

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The package's own source may not remove, rename or exec rm by any route
// but internal/saferm, and may not name what it must never open, outside
// detect.go (the detectors and the refusal list). These tests read the
// non-test source as Go tokens, so comments are not matched and strings
// are.

// removalCalls are the methods and functions that remove or rename,
// whatever package or type they are called on; saferm's are the one way.
var removalCalls = map[string]bool{
	"Remove": true, "RemoveAll": true, "Rename": true, "Unlink": true,
	"Unlinkat": true, "Rmdir": true, "Renameat": true, "Renameat2": true,
	"RemoveTree": true, "RemoveRoot": true, "RemoveTreeAt": true,
}

// sensitive are strings no non-test file but detect.go may hold.
var sensitive = []string{".credentials", ".ssh", ".claude", "Keychain", "SSH_AUTH_SOCK", "/Users/", "$HOME"}

type finding struct {
	file string
	line int
	what string
}

// scanSource scans the non-test .go files of dir.
func scanSource(t *testing.T, dir string) []finding {
	t.Helper()
	return scanSourceAs(t, dir, "")
}

// scanSourceAs scans dir, naming its files prefix+name: the exemptions
// are for the package's own files, so a same-named file elsewhere has none.
func scanSourceAs(t *testing.T, dir, prefix string) []finding {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []finding
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, scanFile(prefix+filepath.Base(f), src)...)
	}
	return out
}

func scanFile(name string, src []byte) []finding {
	fset := token.NewFileSet()
	file := fset.AddFile(name, -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	type tok struct {
		t   token.Token
		lit string
		pos token.Pos
	}
	var toks []tok
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		toks = append(toks, tok{t, lit, pos})
	}
	var out []finding
	add := func(p token.Pos, what string) {
		out = append(out, finding{name, fset.Position(p).Line, what})
	}
	for i, k := range toks {
		switch {
		case k.t == token.IDENT && removalCalls[k.lit] && i > 1 && toks[i-1].t == token.PERIOD &&
			i+1 < len(toks) && toks[i+1].t == token.LPAREN:
			if q := toks[i-2]; !(q.t == token.IDENT && q.lit == "saferm") {
				add(k.pos, "calls "+k.lit+" not through saferm")
			}
		case k.t == token.IDENT && (k.lit == "Command" || k.lit == "CommandContext") && i+2 < len(toks) &&
			toks[i+1].t == token.LPAREN:
			// exec.Command("rm", ...), and "sh" outside the one file that
			// builds sandbox commands.
			for j := i + 2; j < len(toks) && j < i+5; j++ {
				if toks[j].t != token.STRING {
					continue
				}
				v, _ := strconv.Unquote(toks[j].lit)
				if v == "rm" || strings.HasSuffix(v, "/rm") || (v == "sh" || strings.HasSuffix(v, "/sh")) && name != "sandboxcmd.go" {
					add(k.pos, "execs "+v)
				}
				break
			}
		case k.t == token.STRING && name != "detect.go":
			v, err := strconv.Unquote(k.lit)
			if err != nil {
				v = k.lit
			}
			if strings.Contains(v, "/bin/sh") && name != "sandboxcmd.go" {
				add(k.pos, "names the shell")
			}
			// The wipe is built in the one file that builds commands, and
			// never as the removal of everything.
			if strings.Contains(v, "rm -rf") && name != "sandboxcmd.go" {
				add(k.pos, "builds a recursive removal")
			}
			if strings.Contains(v, "no-preserve-root") {
				add(k.pos, "names --no-preserve-root")
			}
			for _, w := range sensitive {
				if strings.Contains(v, w) {
					add(k.pos, "names "+w)
				}
			}
		case k.t == token.IDENT && name != "detect.go" && (k.lit == "SocketPath" || k.lit == "UserHomeDir"):
			add(k.pos, "names "+k.lit)
		}
	}
	return out
}

func TestSourceHasNoRemovalOrSecrets(t *testing.T) {
	for _, f := range scanSource(t, ".") {
		t.Errorf("%s:%d: %s", f.file, f.line, f.what)
	}
	// The model sandbox is held to the same rules.
	for _, d := range []string{"fake", "probe", "probe/cmd/conformance-probe"} {
		for _, f := range scanSourceAs(t, d, d+"/") {
			t.Errorf("%s:%d: %s", f.file, f.line, f.what)
		}
	}
}

// A same-named file in another directory has no exemption.
func TestScannerExemptionsAreForThePackage(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "sandboxcmd.go"), []byte(`package p; func f() { exec.Command("sh", "-c", c) }`), 0o644))
	if fs := scanSourceAs(t, dir, "fake/"); len(fs) == 0 {
		t.Error("a sandboxcmd.go in another directory was exempt")
	}
	if fs := scanSource(t, dir); len(fs) != 0 {
		t.Errorf("the package's own sandboxcmd.go flagged: %v", fs)
	}
}

// The scanner itself catches what it must, in files written to a temp dir.
func TestSourceScannerCatches(t *testing.T) {
	for name, src := range map[string]string{
		"osremove.go":   `package p; func f() { os.Remove("x") }`,
		"removeall.go":  `package p; func f() { os.RemoveAll(p) }`,
		"rename.go":     `package p; func f() { os.Rename(a, b) }`,
		"rootremove.go": `package p; func f(r *os.Root) { r.RemoveAll("x") }`,
		"nofollow.go":   `package p; func f() { nofollow.Dir(b).Remove("x") }`,
		"unlink.go":     `package p; func f() { unix.Unlinkat(fd, "x", 0) }`,
		"execrm.go":     `package p; func f() { exec.Command("rm", "-rf", p) }`,
		"execbinrm.go":  `package p; func f() { exec.CommandContext(ctx, "/bin/rm", p) }`,
		"execsh.go":     `package p; func f() { exec.Command("sh", "-c", c) }`,
		"creds.go":      `package p; var x = "data/home/.credentials.json"`,
		"ssh.go":        `package p; var x = "/.ssh/id"`,
		"claude.go":     `package p; var x = "~/.claude"`,
		"users.go":      `package p; var x = "/Users/me"`,
		"home.go":       `package p; var x = "$HOME/x"`,
		"sock.go":       `package p; var x = "SSH_AUTH_SOCK"`,
		"keychain.go":   "package p; var x = `Keychain`",
		"binsh.go":      `package p; var x = []string{"/bin/sh", "-c", c}`,
		"rmrf.go":       `package p; var x = "rm -rf " + p`,
		"preserve.go":   `package p; var x = "--no-" + "x" + "--no-preserve-root"`,
		"sandboxcmd.go": `package p; var x = "rm -rf --no-preserve-root /"`,
	} {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
		if fs := scanSource(t, dir); len(fs) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	// The link socket's constant, spelled in parts so this file does not
	// name it.
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "socket.go"), []byte("package p; var x = agent."+"Socket"+"Path"), 0o644))
	if fs := scanSource(t, dir); len(fs) == 0 {
		t.Error("socket.go: not caught")
	}
	for name, src := range map[string]string{
		"saferm.go":      `package p; func f() { saferm.RemoveAll(b, p); saferm.RemoveTree(b, p); saferm.RemoveRoot(b) }`,
		"comment.go":     "package p\n// os.RemoveAll is never called here, nor ~/.claude read.\nvar x = 1",
		"sandboxcmd.go":  `package p; func f() { exec.Command("sh", "-c", c) }`,
		"detect.go":      `package p; var x = "/etc/claude-code" + "SSH_AUTH_SOCK"`,
		"x_test.go":      `package p; func f() { os.RemoveAll(p) }`,
		"otherremove.go": `package p; func f() { removeLater(x) }`,
	} {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
		if fs := scanSource(t, dir); len(fs) != 0 {
			t.Errorf("%s: flagged %v", name, fs)
		}
	}
}
