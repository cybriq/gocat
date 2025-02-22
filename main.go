// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Originally authored by Russ Cox (https://code.google.com/p/rsc/source/browse/cmd/bundle/main.go)
//
// Modified by Joseph Naegele, 2015

// Gocat combines multiple Go source files into a single source file,
// optionally adding a prefix to all top-level names.
//
// Usage:
//
//    gocat [-p pkgname] [-x prefix] file_0 [file_n...] >file.go
//
// Example
//
//	  gocat -p acl github.com/naegelejd/go-acl/acl* > acl.go
//
// Bugs
//
// Gocat has many limitations, most of them not fundamental.
//
// It does not work with cgo.
//
// It does not work with renamed imports.
//
// It does not correctly translate struct literals when prefixing is enabled
// and a field key in the literal key is the same as a top-level name.
//
// It does not work with embedded struct fields.
//
// Using grep and xargs to filter out tests can be done like so:
//
//     ls *.go|grep -v _test|xargs gocat>p2p/aio.go
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

var (
	pkgname = flag.String("p", "", "package name to use in output file")
	prefix  = flag.String("x", "", "prefix to add to all top-level names")
	notest  = flag.Bool("n", false, "ignore test files")
	kill    = flag.Bool("k", false, "delete concatenated files from disk")
)

func die(v ...interface{}) {
	fmt.Fprint(os.Stderr, v)
	os.Exit(1)
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)

	for _, name := range flag.Args() {
		if *notest && strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			die(err)
		}
		files[name] = f
		if *kill {
			if err := os.Remove(name); err != nil {
				die(err)
			}
		}
	}

	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	renamed := make(map[*ast.Ident]bool)
	rename := func(id *ast.Ident) {
		if !renamed[id] && id != nil && id.Name != "_" && id.Name != "" {
			id.Name = *prefix + id.Name
			renamed[id] = true
		}
	}

	isTop := make(map[interface{}]bool)
	isType := make(map[string]bool)
	isTopName := make(map[string]bool)
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						isTop[spec] = true
						for _, id := range spec.Names {
							isTopName[id.Name] = true
							rename(id)
						}
					case *ast.TypeSpec:
						isTop[spec] = true
						isType[spec.Name.Name] = true
						isTopName[spec.Name.Name] = true
						rename(spec.Name)
					}
				}

			case *ast.FuncDecl:
				if d.Recv != nil {
					break
				}
				isTop[d] = true
				isTopName[d.Name.Name] = true
				rename(d.Name)
			}
		}
	}

	for _, f := range files {
		walk(f,
			func(n interface{}) {
				kv, ok := n.(*ast.KeyValueExpr)
				if ok {
					if id, ok := kv.Key.(*ast.Ident); ok && renamed[id] && isType[id.Name[len(*prefix):]] {
						id.Name = id.Name[len(*prefix):]
					}
				}
				id, ok := n.(*ast.Ident)
				if ok && (id.Obj != nil && isTop[id.Obj.Decl] || id.Obj == nil && isTopName[id.Name]) {
					rename(id)
				}
			},
		)
	}

	var f0 *ast.File
	for i, name := range names {
		f := files[name]
		if i == 0 {
			f0 = f
			if *pkgname != "" {
				f.Name.Name = *pkgname
			}
		} else {
			f.Name = &ast.Ident{Name: "PACKAGE-DELETE-ME"}
			for _, spec := range f.Imports {
				if spec.Name != nil {
					fmt.Printf("%s: renamed import not supported",
						fset.Position(spec.Name.Pos()),
					)
					continue
				}
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					fmt.Printf("%s: invalid quoted string %s",
						fset.Position(spec.Name.Pos()), spec.Path.Value,
					)
					continue
				}
				if path == "C" {
					fmt.Printf("%s: import \"C\" not supported",
						fset.Position(spec.Name.Pos()),
					)
					continue
				}
				addImport(f0, path)
			}
			decls := f.Decls[:0]
			for _, d := range f.Decls {
				if d, ok := d.(*ast.GenDecl); ok && d.Tok == token.IMPORT {
					continue
				}
				decls = append(decls, d)
			}
			f.Decls = decls
		}
	}

	var buf bytes.Buffer

	for _, name := range names {
		f := files[name]
		printer.Fprint(&buf, fset, f)
	}

	data := bytes.Replace(
		buf.Bytes(), []byte("package PACKAGE-DELETE-ME\n"), nil, -1,
	)

	f, err := parser.ParseFile(fset, "rewritten", data, parser.ParseComments)
	if err != nil {
		die(err)
	}

	if err := format.Node(os.Stdout, fset, f); err != nil {
		die(err)
	}
}

// NOTE: Below here stolen from gofix, should probably be in a library eventually.

// addImport adds the import path to the file f, if absent.
// Copied from cmd/fix.
func addImport(f *ast.File, ipath string) (added bool) {
	if imports(f, ipath) {
		return false
	}

	// Determine name of import.
	// Assume added imports follow convention of using last element.
	_, name := path.Split(ipath)

	// Rename any conflicting top-level references from name to name_.
	renameTop(f, name, name+"_")

	newImport := &ast.ImportSpec{
		Path: &ast.BasicLit{
			Kind:  token.STRING,
			Value: strconv.Quote(ipath),
		},
	}

	// Find an import decl to add to.
	var (
		bestMatch  = -1
		lastImport = -1
		impDecl    *ast.GenDecl
		impIndex   = -1
	)
	for i, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.IMPORT {
			lastImport = i

			// Compute longest shared prefix with imports in this block.
			for j, spec := range gen.Specs {
				impspec := spec.(*ast.ImportSpec)
				n := matchLen(importPath(impspec), ipath)
				if n > bestMatch {
					bestMatch = n
					impDecl = gen
					impIndex = j
				}
			}
		}
	}

	// If no import decl found, add one after the last import.
	if impDecl == nil {
		impDecl = &ast.GenDecl{
			Tok: token.IMPORT,
		}
		f.Decls = append(f.Decls, nil)
		copy(f.Decls[lastImport+2:], f.Decls[lastImport+1:])
		f.Decls[lastImport+1] = impDecl
	}

	// Ensure the import decl has parentheses, if needed.
	if len(impDecl.Specs) > 0 && !impDecl.Lparen.IsValid() {
		impDecl.Lparen = impDecl.Pos()
	}

	insertAt := impIndex + 1
	if insertAt == 0 {
		insertAt = len(impDecl.Specs)
	}
	impDecl.Specs = append(impDecl.Specs, nil)
	copy(impDecl.Specs[insertAt+1:], impDecl.Specs[insertAt:])
	impDecl.Specs[insertAt] = newImport
	if insertAt > 0 {
		// Assign same position as the previous import,
		// so that the sorter sees it as being in the same block.
		prev := impDecl.Specs[insertAt-1]
		newImport.Path.ValuePos = prev.Pos()
		newImport.EndPos = prev.Pos()
	}

	f.Imports = append(f.Imports, newImport)
	return true
}

// renameTop renames all references to the top-level name old.
// It returns true if it makes any changes.
func renameTop(f *ast.File, old, new string) bool {
	var fixed bool

	// Rename any conflicting imports
	// (assuming package name is last element of path).
	for _, s := range f.Imports {
		if s.Name != nil {
			if s.Name.Name == old {
				s.Name.Name = new
				fixed = true
			}
		} else {
			_, thisName := path.Split(importPath(s))
			if thisName == old {
				s.Name = ast.NewIdent(new)
				fixed = true
			}
		}
	}

	// Rename any top-level declarations.
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == old {
				d.Name.Name = new
				d.Name.Obj.Name = new
				fixed = true
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == old {
						s.Name.Name = new
						s.Name.Obj.Name = new
						fixed = true
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name == old {
							n.Name = new
							n.Obj.Name = new
							fixed = true
						}
					}
				}
			}
		}
	}

	// Rename top-level old to new, both unresolved names
	// (probably defined in another file) and names that resolve
	// to a declaration we renamed.
	walk(f,
		func(n interface{}) {
			id, ok := n.(*ast.Ident)
			if ok && isTopName(id, old) {
				id.Name = new
				fixed = true
			}
			if ok && id.Obj != nil && id.Name == old && id.Obj.Name == new {
				id.Name = id.Obj.Name
				fixed = true
			}
		},
	)

	return fixed
}

// matchLen returns the length of the longest prefix shared by x and y.
func matchLen(x, y string) int {
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	return i
}

// imports returns true if f imports path.
func imports(f *ast.File, path string) bool {
	return importSpec(f, path) != nil
}

// isTopName returns true if n is a top-level unresolved identifier with the given name.
func isTopName(n ast.Expr, name string) bool {
	id, ok := n.(*ast.Ident)
	return ok && id.Name == name && id.Obj == nil
}

// importPath returns the unquoted import path of s,
// or "" if the path is not properly quoted.
func importPath(s *ast.ImportSpec) string {
	t, err := strconv.Unquote(s.Path.Value)
	if err == nil {
		return t
	}
	return ""
}

// importSpec returns the import spec if f imports path,
// or nil otherwise.
func importSpec(f *ast.File, path string) *ast.ImportSpec {
	for _, s := range f.Imports {
		if importPath(s) == path {
			return s
		}
	}
	return nil
}

// walk traverses the AST x, calling visit(y) for each node y in the tree but
// also with a pointer to each ast.Expr, ast.Stmt, and *ast.BlockStmt,
// in a bottom-up traversal.
func walk(x interface{}, visit func(interface{})) {
	walkBeforeAfter(x, nop, visit)
}

func nop(interface{}) {}

// walkBeforeAfter is like walk but calls before(x) before traversing
// x's children and after(x) afterward.
func walkBeforeAfter(x interface{}, before, after func(interface{})) {
	before(x)

	switch n := x.(type) {
	default:
		panic(fmt.Errorf("unexpected type %T in walkBeforeAfter", x))

	case nil:

	// pointers to interfaces
	case *ast.Decl:
		walkBeforeAfter(*n, before, after)
	case *ast.Expr:
		walkBeforeAfter(*n, before, after)
	case *ast.Spec:
		walkBeforeAfter(*n, before, after)
	case *ast.Stmt:
		walkBeforeAfter(*n, before, after)

	// pointers to struct pointers
	case **ast.BlockStmt:
		walkBeforeAfter(*n, before, after)
	case **ast.CallExpr:
		walkBeforeAfter(*n, before, after)
	case **ast.FieldList:
		walkBeforeAfter(*n, before, after)
	case **ast.FuncType:
		walkBeforeAfter(*n, before, after)
	case **ast.Ident:
		walkBeforeAfter(*n, before, after)
	case **ast.BasicLit:
		walkBeforeAfter(*n, before, after)

	// pointers to slices
	case *[]ast.Decl:
		walkBeforeAfter(*n, before, after)
	case *[]ast.Expr:
		walkBeforeAfter(*n, before, after)
	case *[]*ast.File:
		walkBeforeAfter(*n, before, after)
	case *[]*ast.Ident:
		walkBeforeAfter(*n, before, after)
	case *[]ast.Spec:
		walkBeforeAfter(*n, before, after)
	case *[]ast.Stmt:
		walkBeforeAfter(*n, before, after)

	// These are ordered and grouped to match ../../pkg/go/ast/ast.go
	case *ast.Field:
		walkBeforeAfter(&n.Names, before, after)
		walkBeforeAfter(&n.Type, before, after)
		walkBeforeAfter(&n.Tag, before, after)
	case *ast.FieldList:
		for _, field := range n.List {
			walkBeforeAfter(field, before, after)
		}
	case *ast.BadExpr:
	case *ast.Ident:
	case *ast.Ellipsis:
		walkBeforeAfter(&n.Elt, before, after)
	case *ast.BasicLit:
	case *ast.FuncLit:
		walkBeforeAfter(&n.Type, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.CompositeLit:
		walkBeforeAfter(&n.Type, before, after)
		walkBeforeAfter(&n.Elts, before, after)
	case *ast.ParenExpr:
		walkBeforeAfter(&n.X, before, after)
	case *ast.SelectorExpr:
		walkBeforeAfter(&n.X, before, after)
	case *ast.IndexExpr:
		walkBeforeAfter(&n.X, before, after)
		walkBeforeAfter(&n.Index, before, after)
	case *ast.SliceExpr:
		walkBeforeAfter(&n.X, before, after)
		if n.Low != nil {
			walkBeforeAfter(&n.Low, before, after)
		}
		if n.High != nil {
			walkBeforeAfter(&n.High, before, after)
		}
	case *ast.TypeAssertExpr:
		walkBeforeAfter(&n.X, before, after)
		walkBeforeAfter(&n.Type, before, after)
	case *ast.CallExpr:
		walkBeforeAfter(&n.Fun, before, after)
		walkBeforeAfter(&n.Args, before, after)
	case *ast.StarExpr:
		walkBeforeAfter(&n.X, before, after)
	case *ast.UnaryExpr:
		walkBeforeAfter(&n.X, before, after)
	case *ast.BinaryExpr:
		walkBeforeAfter(&n.X, before, after)
		walkBeforeAfter(&n.Y, before, after)
	case *ast.KeyValueExpr:
		walkBeforeAfter(&n.Key, before, after)
		walkBeforeAfter(&n.Value, before, after)

	case *ast.ArrayType:
		walkBeforeAfter(&n.Len, before, after)
		walkBeforeAfter(&n.Elt, before, after)
	case *ast.StructType:
		walkBeforeAfter(&n.Fields, before, after)
	case *ast.FuncType:
		walkBeforeAfter(&n.Params, before, after)
		if n.Results != nil {
			walkBeforeAfter(&n.Results, before, after)
		}
	case *ast.InterfaceType:
		walkBeforeAfter(&n.Methods, before, after)
	case *ast.MapType:
		walkBeforeAfter(&n.Key, before, after)
		walkBeforeAfter(&n.Value, before, after)
	case *ast.ChanType:
		walkBeforeAfter(&n.Value, before, after)

	case *ast.BadStmt:
	case *ast.DeclStmt:
		walkBeforeAfter(&n.Decl, before, after)
	case *ast.EmptyStmt:
	case *ast.LabeledStmt:
		walkBeforeAfter(&n.Stmt, before, after)
	case *ast.ExprStmt:
		walkBeforeAfter(&n.X, before, after)
	case *ast.SendStmt:
		walkBeforeAfter(&n.Chan, before, after)
		walkBeforeAfter(&n.Value, before, after)
	case *ast.IncDecStmt:
		walkBeforeAfter(&n.X, before, after)
	case *ast.AssignStmt:
		walkBeforeAfter(&n.Lhs, before, after)
		walkBeforeAfter(&n.Rhs, before, after)
	case *ast.GoStmt:
		walkBeforeAfter(&n.Call, before, after)
	case *ast.DeferStmt:
		walkBeforeAfter(&n.Call, before, after)
	case *ast.ReturnStmt:
		walkBeforeAfter(&n.Results, before, after)
	case *ast.BranchStmt:
	case *ast.BlockStmt:
		walkBeforeAfter(&n.List, before, after)
	case *ast.IfStmt:
		walkBeforeAfter(&n.Init, before, after)
		walkBeforeAfter(&n.Cond, before, after)
		walkBeforeAfter(&n.Body, before, after)
		walkBeforeAfter(&n.Else, before, after)
	case *ast.CaseClause:
		walkBeforeAfter(&n.List, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.SwitchStmt:
		walkBeforeAfter(&n.Init, before, after)
		walkBeforeAfter(&n.Tag, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.TypeSwitchStmt:
		walkBeforeAfter(&n.Init, before, after)
		walkBeforeAfter(&n.Assign, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.CommClause:
		walkBeforeAfter(&n.Comm, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.SelectStmt:
		walkBeforeAfter(&n.Body, before, after)
	case *ast.ForStmt:
		walkBeforeAfter(&n.Init, before, after)
		walkBeforeAfter(&n.Cond, before, after)
		walkBeforeAfter(&n.Post, before, after)
		walkBeforeAfter(&n.Body, before, after)
	case *ast.RangeStmt:
		walkBeforeAfter(&n.Key, before, after)
		walkBeforeAfter(&n.Value, before, after)
		walkBeforeAfter(&n.X, before, after)
		walkBeforeAfter(&n.Body, before, after)

	case *ast.ImportSpec:
	case *ast.ValueSpec:
		walkBeforeAfter(&n.Type, before, after)
		walkBeforeAfter(&n.Values, before, after)
		walkBeforeAfter(&n.Names, before, after)
	case *ast.TypeSpec:
		walkBeforeAfter(&n.Type, before, after)

	case *ast.BadDecl:
	case *ast.GenDecl:
		walkBeforeAfter(&n.Specs, before, after)
	case *ast.FuncDecl:
		if n.Recv != nil {
			walkBeforeAfter(&n.Recv, before, after)
		}
		walkBeforeAfter(&n.Type, before, after)
		if n.Body != nil {
			walkBeforeAfter(&n.Body, before, after)
		}

	case *ast.File:
		walkBeforeAfter(&n.Decls, before, after)

	case *ast.Package:
		walkBeforeAfter(&n.Files, before, after)

	case []*ast.File:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	case []ast.Decl:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	case []ast.Expr:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	case []*ast.Ident:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	case []ast.Stmt:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	case []ast.Spec:
		for i := range n {
			walkBeforeAfter(&n[i], before, after)
		}
	}
	after(x)
}
