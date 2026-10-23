package macui

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The darwin files type-checked here, on any platform, so that the Go
// type of every argument the glue sends a selector is held to the type
// encoding the self-check's table gives it (and the self-check holds the
// table to the running system): a float32 where a CGFloat is meant, an
// int where a BOOL is, or one argument too many fails here, not at run
// time on a Mac.

const darwinArch = "arm64"

// darwinPackage type-checks the package as it builds for darwin, with the
// export data `go list -export` writes for its imports.
func darwinPackage(t *testing.T) (*token.FileSet, []*ast.File, *types.Info) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go tool on PATH: %v", err)
	}
	cmd := exec.Command(goTool, "list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", ".")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+darwinArch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			exports[k] = v
		}
	}
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "darwin", darwinArch, false
	bp, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, f := range bp.GoFiles {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, af)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", darwinArch)}
	if _, err := conf.Check(bp.ImportPath, fset, files, info); err != nil {
		t.Fatalf("type-check for darwin: %v", err)
	}
	return fset, files, info
}

// encodingOf is the type encoding a Go value of type t is passed as, ""
// for a type that has none the glue may send.
func encodingOf(t types.Type) string {
	if named, ok := t.(*types.Named); ok {
		switch named.Obj().Name() {
		case "obj":
			return "@"
		case "selector":
			return ":"
		case "nsPoint":
			return "{CGPoint=dd}"
		case "nsSize":
			return "{CGSize=dd}"
		case "block":
			return "@?"
		}
		return ""
	}
	if p, ok := t.(*types.Pointer); ok {
		// An out-parameter, NSError**.
		if named, ok := p.Elem().(*types.Named); ok && named.Obj().Name() == "obj" {
			return "^@"
		}
		return ""
	}
	b, ok := types.Default(t).(*types.Basic)
	if !ok {
		return ""
	}
	switch b.Kind() {
	case types.UnsafePointer:
		return "^v" // a buffer's bytes
	case types.Bool:
		return "B"
	case types.Int, types.Int64:
		return "q"
	case types.Uint, types.Uint64:
		return "Q"
	case types.Int32:
		return "i"
	case types.Uint32:
		return "I"
	case types.Int16:
		return "s"
	case types.Float64:
		return "d"
	case types.Float32:
		return "f"
	case types.String:
		return "*"
	}
	return "" // uintptr and the rest: no telling what they stand for
}

// splitEncoding is a normalized encoding's types: return, self, _cmd,
// then each argument.
func splitEncoding(enc string) []string {
	var out []string
	for i := 0; i < len(enc); {
		j := skipType(enc, i)
		if j <= i {
			return nil
		}
		out = append(out, enc[i:j])
		i = j
	}
	return out
}

// returnsAs are the encodings each form of objc_msgSend may read a
// result of.
var returnsAs = map[string]map[string]bool{
	"send":       {"@": true, "v": true},
	"sendBool":   {"B": true},
	"sendInt":    {"q": true},
	"sendString": {"*": true},
	"sendEvent":  {"@": true},
	"sendSize":   {"v": true},
}

// typedForms are the forms whose parameters are typed, not args ...any.
var typedForms = map[string]bool{"sendEvent": true, "sendSize": true}

// tableEncodings is each selector's encoding; a selector the table lists
// on several classes has one encoding.
func tableEncodings(t *testing.T) map[string]string {
	encs := map[string]string{}
	for _, e := range selTable {
		if prev, ok := encs[e.Name]; ok && prev != e.Types {
			t.Errorf("%s has two encodings in the table: %q, %q", e.Name, prev, e.Types)
		}
		encs[e.Name] = e.Types
	}
	return encs
}

// sendProblems checks every message the darwin files send.
func sendProblems(fset *token.FileSet, files []*ast.File, info *types.Info, encs map[string]string) (problems []string, sends int) {
	bad := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, args...))
	}
	for _, af := range files {
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || returnsAs[fun.Sel.Name] == nil {
				return true
			}
			form := fun.Sel.Name
			args := call.Args
			if x, ok := fun.X.(*ast.Ident); ok && x.Name == "rt" {
				if len(args) == 0 {
					return true // the method forms' own bodies pass args... through
				}
				args = args[1:] // the receiver
			}
			if call.Ellipsis.IsValid() {
				return true // obj.send's own body: args... passed on
			}
			sends++
			if len(args) == 0 {
				bad(call, "%s with no selector", form)
				return true
			}
			sc, ok := args[0].(*ast.CallExpr)
			id, _ := func() (*ast.Ident, bool) {
				if !ok {
					return nil, false
				}
				i, ok := sc.Fun.(*ast.Ident)
				return i, ok
			}()
			if id == nil || id.Name != "sel" || len(sc.Args) != 1 {
				bad(call, "%s's selector is not sel(\"...\")", form)
				return true
			}
			lit, ok := sc.Args[0].(*ast.BasicLit)
			if !ok {
				bad(call, "%s's selector is not a literal", form)
				return true
			}
			name, _ := strconv.Unquote(lit.Value)
			enc, ok := encs[name]
			if !ok || enc == "" {
				bad(call, "%s: no type encoding in the table", name)
				return true
			}
			parts := splitEncoding(enc)
			if len(parts) < 3 {
				bad(call, "%s: unreadable encoding %q", name, enc)
				return true
			}
			if !returnsAs[form][parts[0]] {
				bad(call, "%s returns %q, read through %s", name, parts[0], form)
			}
			want := parts[3:]
			var got []string
			if typedForms[form] {
				// A typed form: its parameters, after the receiver and the
				// selector, are what is passed.
				sig := info.Types[call.Fun].Type.(*types.Signature)
				for i := 2; i < sig.Params().Len(); i++ {
					got = append(got, encodingOf(sig.Params().At(i).Type()))
				}
			} else {
				for _, a := range args[1:] {
					got = append(got, encodingOf(info.Types[a].Type))
				}
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				bad(call, "%s takes %v, is sent %v", name, want, got)
			}
			return true
		})
	}
	return problems, sends
}

func TestSendArgumentTypesMatchEncodings(t *testing.T) {
	fset, files, info := darwinPackage(t)
	problems, sends := sendProblems(fset, files, info, tableEncodings(t))
	for _, p := range problems {
		t.Error(p)
	}
	if sends < 40 {
		t.Errorf("checked only %d sends", sends)
	}
}

// TestSendArgumentCheckCatches holds the check itself to a wrong table:
// with one encoding changed, the sends of that selector fail.
func TestSendArgumentCheckCatches(t *testing.T) {
	fset, files, info := darwinPackage(t)
	for name, enc := range map[string]string{
		"statusItemWithLength:":                  "@@:f",   // a float32 CGFloat
		"setEnabled:":                            "v@:q",   // an integer for a BOOL
		"setState:":                              "v@:q@",  // one argument more
		"runModal":                               "@@:",    // an object, read as an integer
		"performSelector:withObject:afterDelay:": "v@:@@d", // an object for a SEL
		"otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:": "@@:Q{CGPoint=dd}Qdq@iqq",
		"setSize:":              "v@:{CGPoint=dd}", // the wrong struct
		"dataWithBytes:length:": "@@:^vq",          // a signed length
	} {
		encs := tableEncodings(t)
		encs[name] = enc
		problems, _ := sendProblems(fset, files, info, encs)
		found := false
		for _, p := range problems {
			if strings.Contains(p, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("encoding %q for %s: no problem found", enc, name)
		}
	}
}

func TestEncodingOf(t *testing.T) {
	for typ, want := range map[types.Type]string{
		types.Typ[types.Bool]:                  "B",
		types.Typ[types.UntypedBool]:           "B",
		types.Typ[types.Int]:                   "q",
		types.Typ[types.UntypedInt]:            "q",
		types.Typ[types.Uint]:                  "Q",
		types.Typ[types.Float64]:               "d",
		types.Typ[types.UntypedFloat]:          "d",
		types.Typ[types.Float32]:               "f",
		types.Typ[types.Int16]:                 "s",
		types.Typ[types.String]:                "*",
		types.Typ[types.Uintptr]:               "",
		types.Typ[types.UnsafePointer]:         "^v",
		namedUintptr("obj"):                    "@",
		namedUintptr("block"):                  "@?",
		types.NewPointer(namedUintptr("obj")):  "^@",
		types.NewPointer(types.Typ[types.Int]): "",
	} {
		if got := encodingOf(typ); got != want {
			t.Errorf("encodingOf(%v) = %q, want %q", typ, got, want)
		}
	}
}

func namedUintptr(name string) types.Type {
	return types.NewNamed(types.NewTypeName(token.NoPos, nil, name, nil), types.Typ[types.Uintptr], nil)
}
