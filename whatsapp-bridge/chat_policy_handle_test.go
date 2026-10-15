package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"strings"
	"testing"
)

// Type-check the actual policy declarations, rather than a parallel test type.
func TestChatPolicyHandleCannotExposeUnresolvedListFields(t *testing.T) {
	for _, field := range []string{"restricted", "exact", "servers", "invalidPositions", "source"} {
		t.Run(field, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "chat_policy.go", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var declarations []ast.Decl
			for _, decl := range file.Decls {
				group, ok := decl.(*ast.GenDecl)
				if !ok || group.Tok != token.TYPE {
					continue
				}
				for _, spec := range group.Specs {
					name := spec.(*ast.TypeSpec).Name.Name
					if name == "chatPolicy" || name == "chatPolicyHandle" {
						declarations = append(declarations, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{spec}})
					}
				}
			}
			if len(declarations) != 2 {
				t.Fatal("value and handle declarations must be separate")
			}
			probe, err := parser.ParseFile(fset, "consumer.go", "package main; func consumer(h chatPolicyHandle) { _ = h."+field+" }", 0)
			if err != nil {
				t.Fatal(err)
			}
			probe.Decls = append(declarations, probe.Decls...)
			config := gotypes.Config{}
			_, err = config.Check("main", fset, []*ast.File{probe}, nil)
			if err == nil || !strings.Contains(err.Error(), "has no field or method "+field) {
				t.Fatalf("unresolved handle field unexpectedly compiles: %v", err)
			}
		})
	}
}
