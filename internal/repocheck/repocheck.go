// Package repocheck holds checks over the repository's own source that run
// with the unit tests.
//
// No test file may name the link socket, which in a sandbox is a live
// connection to the host: tests always use a socket of their own. And the
// packages whose tests drive docker run them under internal/nodocker, so
// that a test that forgets to fake docker fails instead of reaching the
// machine's engine.
package repocheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one forbidden mention in a test file.
type Finding struct {
	File string // slash-separated, relative to the scanned root
	Line int
	What string
}

var (
	qualifiedRef = regexp.MustCompile(`\.SocketPath\b`)
	importsAgent = regexp.MustCompile(`"[^"]*/internal/agent"`)
	joinNoise    = strings.NewReplacer("\"", "", "`", "", "+", "", " ", "", "\t", "")
	bareRef      = regexp.MustCompile(`\bSocketPath\b`)
	pkgAgent     = regexp.MustCompile(`(?m)^package agent\s*(//.*)?$`)
)

// ScanTests walks root and reports every mention, in a _test.go file, of the
// link socket's path, of agent.SocketPath, or (in package agent) of a bare
// SocketPath. The path is also found when written as a concatenation or
// split raw strings on one line, and SocketPath under any import alias.
// Directories whose name starts with "." are skipped (version
// control, tool state, worktrees); testdata and vendor are not.
func ScanTests(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, scanFile(filepath.ToSlash(rel), b)...)
		return nil
	})
	return out, err
}

func scanFile(name string, src []byte) []Finding {
	inAgent := pkgAgent.Match(src) || importsAgent.Match(src)
	var out []Finding
	for i, line := range bytes.Split(src, []byte("\n")) {
		add := func(what string) { out = append(out, Finding{name, i + 1, what}) }
		switch {
		case strings.Contains(joinNoise.Replace(string(line)), "caboose-agent.sock"):
			add("names the link socket's path")
		case qualifiedRef.Match(line):
			add("names SocketPath of the agent package")
		case inAgent && bareRef.Match(line):
			add("names SocketPath")
		}
	}
	return out
}

// DockerPackages are the directories, relative to the repository's root,
// whose tests drive docker (by a stub, always) and so must run under
// nodocker.Run.
var DockerPackages = []string{".", "cmd/caboose", "internal/launcher", "internal/backend", "internal/docker"}

// CheckDockerGuard reports each of dirs (relative to root) that is missing
// or whose tests have no TestMain calling nodocker.Run.
func CheckDockerGuard(root string, dirs []string) ([]Finding, error) {
	var out []Finding
	for _, dir := range dirs {
		ok, err := guarded(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, Finding{File: filepath.ToSlash(dir), What: "has no TestMain that calls nodocker.Run"})
		}
	}
	return out, nil
}

// guarded reports whether a _test.go file directly in dir declares a
// TestMain whose body calls nodocker.Run. A missing dir is an error: a
// package that moved must take its guard along.
func guarded(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("repocheck: %w", err)
	}
	fset := token.NewFileSet()
	for _, e := range ents {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			if callsNodockerRun(f, fn.Body) {
				return true, nil
			}
		}
	}
	return false, nil
}

// callsNodockerRun reports whether body calls Run of the nodocker package,
// under whatever name f imports it.
func callsNodockerRun(f *ast.File, body *ast.BlockStmt) bool {
	names := map[string]bool{}
	for _, im := range f.Imports {
		if strings.Trim(im.Path.Value, `"`) != nodockerPath {
			continue
		}
		name := "nodocker"
		if im.Name != nil {
			name = im.Name.Name
		}
		names[name] = true
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
			if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

const nodockerPath = "github.com/bfreis/caboose/internal/nodocker"
