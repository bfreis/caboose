package macui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The package's own source, every file whatever its build tag, read as
// text: the darwin files are checked here too, on any platform.

// rawRuntime are the runtime functions that take a class or selector name
// as a string; outside rawAllowed they are never used, so every name goes
// through sel or class, with a literal.
var (
	rawRuntime = map[string]bool{"getClass": true, "selRegisterName": true}
	// rawAllowed are the functions ("file:func", "file:Type.*" for every
	// method of a type) that may use them: the two that wrap them, the
	// load that binds them, the registration (whose name is checked at
	// its calls), and the self-check's lookups of the table's names.
	rawAllowed = map[string]bool{
		"objc_darwin.go:sel":                 true,
		"objc_darwin.go:class":               true,
		"objc_darwin.go:doLoad":              true,
		"objc_darwin.go:registerClass":       true,
		"selftest_darwin.go:runtimeLookup.*": true,
		"selftest_darwin.go:target":          true,
	}
	// nameFuncs take a selector's or a class's name.
	nameFuncs = map[string]bool{"sel": true, "class": true, "registerClass": true}
)

// recvName is a method receiver's type name.
func recvName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return "?"
}

type scanned struct {
	sels, classes map[string]bool
	problems      []string
}

func scanPackage(t *testing.T) scanned {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, af)
		names = append(names, f)
	}
	if len(parsed) < 5 {
		t.Fatalf("found only %d source files", len(parsed))
	}
	// The package's string constants, which count as literals.
	consts := map[string]string{}
	for _, af := range parsed {
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							v, _ := strconv.Unquote(bl.Value)
							consts[n.Name] = v
						}
					}
				}
			}
		}
	}
	s := scanned{sels: map[string]bool{}, classes: map[string]bool{}}
	bad := func(pos token.Pos, msg string) {
		s.problems = append(s.problems, fset.Position(pos).String()+": "+msg)
	}
	literal := func(e ast.Expr) (string, bool) {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				v, err := strconv.Unquote(e.Value)
				return v, err == nil
			}
		case *ast.Ident:
			v, ok := consts[e.Name]
			return v, ok
		}
		return "", false
	}
	for i, af := range parsed {
		name := names[i]
		for _, imp := range af.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/ebitengine/purego/objc" {
				bad(imp.Pos(), "imports purego's objc package, whose init loads the runtime")
			}
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "init" {
				bad(fd.Pos(), "func init: nothing may run when the package is linked")
			}
		}
		// Direct calls of sel, class and registerClass, with a literal.
		direct := map[*ast.Ident]bool{}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || !nameFuncs[id.Name] {
				return true
			}
			direct[id] = true
			if len(call.Args) == 0 {
				bad(call.Pos(), id.Name+" with no argument")
				return true
			}
			v, ok := literal(call.Args[0])
			if !ok {
				bad(call.Pos(), id.Name+" with an argument that is not a literal")
				return true
			}
			if id.Name == "sel" {
				s.sels[v] = true
			} else {
				s.classes[v] = true
			}
			return true
		})
		// Every other reference to them, or to the runtime's own lookups,
		// is a way around the literal: a parenthesized call, an alias, a
		// function value.
		for _, d := range af.Decls {
			fn := ""
			var defName *ast.Ident
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					fn = recvName(fd.Recv.List[0].Type) + "." + fn
				} else if name == "objc_darwin.go" && nameFuncs[fn] {
					defName = fd.Name
				}
			}
			var walk func(ast.Node) bool
			walk = func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if nameFuncs[n.Name] && !direct[n] && n != defName {
						bad(n.Pos(), "a reference to "+n.Name+" that is not a direct call with a literal")
					}
				case *ast.SelectorExpr:
					// Any receiver: a copy of rt, or a pointer to it, reaches the
					// same functions.
					if rawRuntime[n.Sel.Name] && !rawAllowed[name+":"+fn] && !rawAllowed[name+":"+strings.SplitN(fn, ".", 2)[0]+".*"] {
						bad(n.Pos(), "."+n.Sel.Name+" used in "+name+":"+fn)
					}
					ast.Inspect(n.X, walk)
					return false // its Sel is a field, not a reference to sel or class
				}
				return true
			}
			ast.Inspect(d, walk)
		}
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSourceMatchesSelfCheckTable fails on a selector or class the code
// names that the self-check's table lacks, on one named with anything but
// a literal, on a table entry the code does not use, and on a func init.
func TestSourceMatchesSelfCheckTable(t *testing.T) {
	s := scanPackage(t)
	for _, p := range s.problems {
		t.Error(p)
	}
	tableSels := map[string]bool{}
	tableClasses := map[string]bool{}
	entryClasses := map[string]bool{}
	for _, e := range selTable {
		tableSels[e.Name] = true
		entryClasses[e.Class] = true
	}
	for _, c := range classTable {
		tableClasses[c] = true
	}
	for _, v := range sortedKeys(s.sels) {
		if !tableSels[v] {
			t.Errorf("selector %q is not in the self-check's table", v)
		}
	}
	for _, v := range sortedKeys(s.classes) {
		if !tableClasses[v] {
			t.Errorf("class %q is not in the self-check's table", v)
		}
	}
	for _, v := range sortedKeys(tableSels) {
		if !s.sels[v] {
			t.Errorf("the table's selector %q is used nowhere", v)
		}
	}
	for _, v := range classTable {
		if !s.classes[v] && !entryClasses[v] {
			t.Errorf("the table's class %q is used nowhere", v)
		}
	}
	if len(s.sels) < 30 {
		t.Errorf("the scan found only %d selectors", len(s.sels))
	}
}
