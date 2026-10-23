package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// On darwin, init locks main to the main thread, which AppKit needs for
// caboose app's run loop: main_darwin.go's init calls runtime.LockOSThread,
// and that is all it does.
func TestMainDarwinLocksTheMainThread(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main_darwin.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "init" || fd.Recv != nil || fd.Body == nil || len(fd.Body.List) != 1 {
			continue
		}
		es, ok := fd.Body.List[0].(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "LockOSThread" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "runtime" {
				found = true
			}
		}
	}
	if !found {
		t.Error("main_darwin.go has no func init() { runtime.LockOSThread() }")
	}
}
