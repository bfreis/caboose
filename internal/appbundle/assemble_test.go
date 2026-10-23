package appbundle

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/saferm"
)

// workDir is the test's directory, checked to be under the system's temp
// directory before anything in it is replaced or removed.
func workDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(tmp, d); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatalf("%s is not under %s", d, tmp)
	}
	return d
}

func fakeExe(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "built-caboose")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// recordRemoves records every removal Assemble makes, through saferm.
func recordRemoves(t *testing.T) *[][2]string {
	t.Helper()
	var calls [][2]string
	old := remove
	remove = func(root, p string) error {
		calls = append(calls, [2]string{root, p})
		return saferm.RemoveAll(root, p)
	}
	t.Cleanup(func() { remove = old })
	return &calls
}

// entries are the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func checkBundle(t *testing.T, app, content, id string) {
	t.Helper()
	fi, err := os.Stat(app)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("bundle dir: %v, %v", fi, err)
	}
	exe := filepath.Join(app, "Contents", "MacOS", "caboose")
	fi, err = os.Stat(exe)
	if err != nil || fi.Mode().Perm() != 0o755 || !fi.Mode().IsRegular() {
		t.Fatalf("executable: %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != content {
		t.Errorf("executable holds %q", b)
	}
	b, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseIdentifier(b); err != nil || got != id {
		t.Errorf("identifier %q, %v", got, err)
	}
	if !bytes.Equal(b, Plist(Info{ID: id, Version: "v1.2.3"})) {
		t.Errorf("Info.plist:\n%s", b)
	}
	if !InCheckoutBundle(exe, ReadPlist) || !IsAppLaunch(exe, nil, ReadPlist) {
		t.Error("the assembled bundle is not recognized")
	}
}

func TestAssembleNew(t *testing.T) {
	d := workDir(t)
	calls := recordRemoves(t)
	app := filepath.Join(d, Name)
	if err := Assemble(app, fakeExe(t, d, "one"), Info{ID: CheckoutID, Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	checkBundle(t, app, "one", CheckoutID)
	if got := entries(t, d); strings.Join(got, ",") != "Caboose.app,built-caboose" {
		t.Errorf("left %v", got)
	}
	if len(*calls) != 0 {
		t.Errorf("removed %v", *calls)
	}
}

func TestAssembleReplaces(t *testing.T) {
	d := workDir(t)
	app := filepath.Join(d, Name)
	if err := Assemble(app, fakeExe(t, d, "one"), Info{ID: CheckoutID, Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	// Something of the old bundle's own, gone with it.
	if err := os.WriteFile(filepath.Join(app, "Contents", "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := recordRemoves(t)
	if err := Assemble(app, fakeExe(t, d, "two"), Info{ID: CheckoutID, Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	checkBundle(t, app, "two", CheckoutID)
	if _, err := os.Lstat(filepath.Join(app, "Contents", "stale")); err == nil {
		t.Error("the old bundle's file is still there")
	}
	if got := entries(t, d); strings.Join(got, ",") != "Caboose.app,built-caboose" {
		t.Errorf("left %v", got)
	}
	// One removal, of the old bundle moved aside, confined to the parent.
	if len(*calls) != 1 || (*calls)[0][0] != d || filepath.Dir((*calls)[0][1]) != d ||
		!strings.HasPrefix(filepath.Base((*calls)[0][1]), ".Caboose.app.tmp-") {
		t.Errorf("removals %v", *calls)
	}
}

// Anything at dir that is not caboose's bundle is refused, and nothing is
// removed: not it, nor anything in it.
func TestAssembleRefuses(t *testing.T) {
	for name, set := range map[string]func(t *testing.T, d string) string{
		"another name": func(t *testing.T, d string) string {
			app := filepath.Join(d, "Other.app")
			mustBundle(t, app, CheckoutID)
			return app
		},
		"another identifier": func(t *testing.T, d string) string {
			app := filepath.Join(d, Name)
			mustBundle(t, app, "com.example.other")
			return app
		},
		"no plist": func(t *testing.T, d string) string {
			app := filepath.Join(d, Name)
			mustWrite(t, filepath.Join(app, "Contents", "MacOS", "caboose"), "x")
			return app
		},
		"a malformed plist": func(t *testing.T, d string) string {
			app := filepath.Join(d, Name)
			mustWrite(t, filepath.Join(app, "Contents", "Info.plist"), "<plist><dict>")
			return app
		},
		"a plain directory": func(t *testing.T, d string) string {
			app := filepath.Join(d, Name)
			mustWrite(t, filepath.Join(app, "precious"), "x")
			return app
		},
		"a file": func(t *testing.T, d string) string {
			app := filepath.Join(d, Name)
			mustWrite(t, app, "x")
			return app
		},
		"a symlink to a bundle": func(t *testing.T, d string) string {
			real := filepath.Join(d, "real", Name)
			mustBundle(t, real, CheckoutID)
			app := filepath.Join(d, Name)
			if err := os.Symlink(real, app); err != nil {
				t.Fatal(err)
			}
			return app
		},
		"a plist that is a symlink": func(t *testing.T, d string) string {
			other := filepath.Join(d, "o", "Info.plist")
			mustWrite(t, other, idPlist(CheckoutID))
			app := filepath.Join(d, Name)
			mustWrite(t, filepath.Join(app, "Contents", "precious"), "x")
			if err := os.Symlink(other, filepath.Join(app, "Contents", "Info.plist")); err != nil {
				t.Fatal(err)
			}
			return app
		},
		"no .app": func(t *testing.T, d string) string {
			app := filepath.Join(d, "Caboose")
			mustWrite(t, filepath.Join(app, "precious"), "x")
			return app
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := workDir(t)
			app := set(t, d)
			before := snapshot(t, d)
			calls := recordRemoves(t)
			if err := Assemble(app, fakeExe(t, d, "new"), Info{ID: CheckoutID, Version: "v1.2.3"}); err == nil {
				t.Fatal("accepted")
			}
			if len(*calls) != 0 {
				t.Errorf("removed %v", *calls)
			}
			after := snapshot(t, d)
			delete(after, "built-caboose")
			if !equal(before, after) {
				t.Errorf("changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestAssembleNeedsAnIdentifier(t *testing.T) {
	d := workDir(t)
	if err := Assemble(filepath.Join(d, Name), fakeExe(t, d, "x"), Info{Version: "v1.2.3"}); err == nil {
		t.Error("accepted")
	}
	if got := entries(t, d); len(got) != 1 {
		t.Errorf("left %v", got)
	}
}

// A failure while building removes the temporary directory, and nothing
// else.
func TestAssembleCleansUp(t *testing.T) {
	d := workDir(t)
	calls := recordRemoves(t)
	if err := Assemble(filepath.Join(d, Name), filepath.Join(d, "missing"), Info{ID: CheckoutID}); err == nil {
		t.Fatal("accepted a missing binary")
	}
	if got := entries(t, d); len(got) != 0 {
		t.Errorf("left %v", got)
	}
	if len(*calls) != 1 || (*calls)[0][0] != d {
		t.Errorf("removals %v", *calls)
	}
}

// The removal Assemble uses refuses anything outside the bundle's parent.
func TestRemoveRefusesOutsideTheParent(t *testing.T) {
	d := workDir(t)
	parent := filepath.Join(d, "parent")
	outside := filepath.Join(d, "outside")
	mustWrite(t, filepath.Join(outside, "keep"), "x")
	mustWrite(t, filepath.Join(parent, "x"), "x")
	for _, p := range []string{outside, filepath.Join(parent, "..", "outside"), parent, d} {
		if err := remove(parent, p); !errors.Is(err, saferm.ErrOutside) {
			t.Errorf("remove(%s): %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("removed outside the parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "x")); err != nil {
		t.Errorf("removed the parent: %v", err)
	}
}

// The package removes nothing but through remove (saferm).
func TestRemovesOnlyThroughSaferm(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := s.X.(*ast.Ident); ok && id.Name == "os" && strings.HasPrefix(s.Sel.Name, "Remove") {
					t.Errorf("%s: os.%s", fset.Position(s.Pos()), s.Sel.Name)
				}
			}
			return true
		})
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustBundle(t *testing.T, app, id string) {
	t.Helper()
	mustWrite(t, filepath.Join(app, "Contents", "Info.plist"), idPlist(id))
	mustWrite(t, filepath.Join(app, "Contents", "MacOS", "caboose"), "old")
}

// snapshot is every path under dir with its contents (a link's target).
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case e.Type()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			m[rel] = "-> " + l
		case e.IsDir():
			m[rel] = "dir"
		default:
			b, _ := os.ReadFile(p)
			m[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
