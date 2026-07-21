package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ruleMissingPackage    = "GODOC001"
	ruleDuplicatePackage  = "GODOC002"
	ruleMalformedPackage  = "GODOC003"
	ruleMissingExported   = "GODOC004"
	ruleMalformedExported = "GODOC005"
)

type finding struct {
	path    string
	line    int
	rule    string
	symbol  string
	message string
}

// String formats a finding as a deterministic path:line diagnostic.
func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s [%s] %s", f.path, f.line, f.rule, f.symbol, f.message)
}

type parsedFile struct {
	path string
	fset *token.FileSet
	file *ast.File
}

type packageKey struct {
	dir  string
	name string
}

func gitGoFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list Go files with git: %w", err)
	}
	lines := bytes.Split(output, []byte{0})
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		path := filepath.Clean(filepath.Join(root, string(line)))
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, statErr)
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func audit(paths []string) ([]finding, error) {
	files := make([]parsedFile, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory; pass Go files", path)
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if ast.IsGenerated(file) {
			continue
		}
		files = append(files, parsedFile{path: displayPath(path), fset: fset, file: file})
	}

	groups := make(map[packageKey][]parsedFile)
	for _, file := range files {
		key := packageKey{dir: filepath.Dir(file.path), name: file.file.Name.Name}
		groups[key] = append(groups[key], file)
	}

	keys := make([]packageKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dir != keys[j].dir {
			return keys[i].dir < keys[j].dir
		}
		return keys[i].name < keys[j].name
	})

	var findings []finding
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].path < group[j].path })
		findings = append(findings, auditPackage(key, group)...)
		for _, file := range group {
			findings = append(findings, auditExports(file)...)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.rule != b.rule {
			return a.rule < b.rule
		}
		if a.symbol != b.symbol {
			return a.symbol < b.symbol
		}
		return a.message < b.message
	})
	return findings, nil
}

func auditPackage(key packageKey, files []parsedFile) []finding {
	type packageDoc struct {
		file parsedFile
		doc  *ast.CommentGroup
	}
	var docs []packageDoc
	for _, file := range files {
		if file.file.Doc != nil {
			docs = append(docs, packageDoc{file: file, doc: file.file.Doc})
		}
	}
	symbol := "package " + key.name
	if len(docs) == 0 {
		file := files[0]
		return []finding{newFinding(file, file.file.Package, ruleMissingPackage, symbol, "package has no documentation comment")}
	}

	var findings []finding
	if len(docs) > 1 {
		for _, item := range docs {
			findings = append(findings, newFinding(item.file, item.doc.Pos(), ruleDuplicatePackage, symbol, "package has multiple documentation comments"))
		}
	}
	want := "Package " + key.name
	if key.name == "main" {
		want = "Command " + commandName(key.dir)
	}
	for _, item := range docs {
		if !startsWithIdentifier(item.doc.Text(), want) {
			findings = append(findings, newFinding(item.file, item.doc.Pos(), ruleMalformedPackage, symbol, fmt.Sprintf("documentation comment must start with %q", want)))
		}
	}
	return findings
}

func auditExports(file parsedFile) []finding {
	var findings []finding
	for _, decl := range file.file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if !decl.Name.IsExported() {
				continue
			}
			symbol := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				if !token.IsExported(receiverBaseName(decl.Recv.List[0].Type)) {
					continue
				}
				symbol = receiverName(decl.Recv.List[0].Type) + "." + symbol
			}
			findings = append(findings, auditComment(file, decl.Name.Pos(), decl.Doc, decl.Name.Name, symbol)...)
		case *ast.GenDecl:
			if decl.Tok == token.IMPORT {
				continue
			}
			if (decl.Tok == token.CONST || decl.Tok == token.VAR) && decl.Lparen.IsValid() {
				findings = append(findings, auditValueGroup(file, decl)...)
				continue
			}
			for index, spec := range decl.Specs {
				name, pos, doc := exportedSpec(spec)
				if name == "" {
					continue
				}
				if doc == nil && index == 0 {
					doc = decl.Doc
				}
				findings = append(findings, auditComment(file, pos, doc, name, name)...)
			}
		}
	}
	return findings
}

func auditValueGroup(file parsedFile, decl *ast.GenDecl) []finding {
	groupDocumented := decl.Doc != nil && strings.TrimSpace(decl.Doc.Text()) != ""
	allValuesDocumented := true
	hasExportedValue := false
	var findings []finding
	for _, spec := range decl.Specs {
		name, pos, doc := exportedSpec(spec)
		if name == "" {
			continue
		}
		hasExportedValue = true
		if doc == nil {
			allValuesDocumented = false
			continue
		}
		findings = append(findings, auditComment(file, pos, doc, name, name)...)
	}
	if !hasExportedValue || groupDocumented || allValuesDocumented {
		return findings
	}
	symbol := decl.Tok.String() + " group"
	message := "exported " + symbol + " has no group documentation comment"
	return append(findings, newFinding(file, decl.Pos(), ruleMissingExported, symbol, message))
}

func exportedSpec(spec ast.Spec) (string, token.Pos, *ast.CommentGroup) {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		if spec.Name.IsExported() {
			return spec.Name.Name, spec.Name.Pos(), spec.Doc
		}
	case *ast.ValueSpec:
		for _, name := range spec.Names {
			if name.IsExported() {
				return name.Name, name.Pos(), spec.Doc
			}
		}
	}
	return "", token.NoPos, nil
}

func auditComment(file parsedFile, pos token.Pos, doc *ast.CommentGroup, want, symbol string) []finding {
	if doc == nil {
		return []finding{newFinding(file, pos, ruleMissingExported, symbol, "exported declaration has no documentation comment")}
	}
	if !startsWithIdentifier(doc.Text(), want) {
		return []finding{newFinding(file, doc.Pos(), ruleMalformedExported, symbol, fmt.Sprintf("documentation comment must start with %q", want))}
	}
	return nil
}

func startsWithIdentifier(text, want string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, want) {
		return false
	}
	if len(text) == len(want) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[len(want):])
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

func receiverName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return "(*" + receiverName(expr.X) + ")"
	case *ast.IndexExpr:
		return receiverName(expr.X)
	case *ast.IndexListExpr:
		return receiverName(expr.X)
	case *ast.ParenExpr:
		return receiverName(expr.X)
	case *ast.SelectorExpr:
		return receiverName(expr.X) + "." + expr.Sel.Name
	default:
		return "receiver"
	}
}

func receiverBaseName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return receiverBaseName(expr.X)
	case *ast.IndexExpr:
		return receiverBaseName(expr.X)
	case *ast.IndexListExpr:
		return receiverBaseName(expr.X)
	case *ast.ParenExpr:
		return receiverBaseName(expr.X)
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

func newFinding(file parsedFile, pos token.Pos, rule, symbol, message string) finding {
	position := file.fset.Position(pos)
	return finding{path: filepath.ToSlash(file.path), line: position.Line, rule: rule, symbol: symbol, message: message}
}

func commandName(dir string) string {
	name := filepath.Base(dir)
	if name == "." {
		if cwd, err := os.Getwd(); err == nil {
			return filepath.Base(cwd)
		}
	}
	return name
}

func displayPath(path string) string {
	clean := filepath.Clean(path)
	if rel, err := filepath.Rel(".", clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return clean
}
