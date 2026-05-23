// docgen/config-ref emits docs/reference/config.md from two sources:
//
//   - TOML config: AST-parse internal/config/config.go for every
//     struct field that has a `toml:"..."` tag. Field name + tag +
//     Go doc comment + type become a row.
//   - Environment variables: scan all .go files under the repo root
//     for `// docgen:env NAME | description` comments. Name + the
//     description after `|` become a row.
//
// Invoked by `make docs-regen` (writes the file) and `make docs-check`
// (writes to a tempfile, diffs against the checked-in copy, fails CI
// if they drift). Convention is: add a field or env var, add the
// docgen comment in the same patch, regenerate, commit together.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	configPath    = "internal/config/config.go"
	defaultOutput = "docs/reference/config.md"
	envPrefix     = "// docgen:env "
)

// tomlField is one row in the TOML config table.
type tomlField struct {
	Section string // e.g. "spx", "gateway"
	Name    string // TOML field name, e.g. "members_auto_refresh"
	GoType  string // Go type as written, e.g. "*bool"
	Doc     string // first sentence of the Go doc comment
}

// envVar is one row in the environment variables table.
type envVar struct {
	Name string // e.g. "IBKR_SPX_MEMBERS_AUTO_REFRESH"
	Desc string // free-text description
	File string // source file the docgen comment lives in (for debugging)
}

func main() {
	output := flag.String("o", defaultOutput, "output path (- for stdout)")
	root := flag.String("root", ".", "repo root to scan for // docgen:env comments")
	flag.Parse()

	toml, err := parseTomlFields(configPath)
	if err != nil {
		fatal("parse config: %v", err)
	}
	envs, err := scanEnvVars(*root)
	if err != nil {
		fatal("scan env vars: %v", err)
	}

	body := render(toml, envs)

	if *output == "-" {
		os.Stdout.WriteString(body)
		return
	}
	if err := os.WriteFile(*output, []byte(body), 0o644); err != nil {
		fatal("write: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// parseTomlFields walks the config.go AST and extracts every field
// with a toml tag. Returns rows sorted by (section, name).
//
// Strategy: find the top-level Config struct, walk its fields to
// learn the section name → Go type mapping (e.g. SPX → "spx"). For
// each referenced type, walk its fields to collect the per-field
// rows.
func parseTomlFields(path string) ([]tomlField, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// First pass: build sectionByType, e.g. {"SPX": "spx", "Gateway": "gateway"}.
	// The top-level Config struct's fields tell us which TOML section
	// each referenced struct populates.
	sectionByType := map[string]string{}
	forEachStruct(file, func(name string, st *ast.StructType, _ *ast.CommentGroup) {
		if name != "Config" {
			return
		}
		for _, f := range st.Fields.List {
			if len(f.Names) == 0 || f.Tag == nil {
				continue
			}
			tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
			section := stripOmit(tag.Get("toml"))
			if section == "" {
				continue
			}
			typeName := goTypeName(f.Type)
			sectionByType[typeName] = section
		}
	})

	// Second pass: for every other struct that has a section mapping,
	// walk its fields. This produces one tomlField per row.
	var rows []tomlField
	forEachStruct(file, func(name string, st *ast.StructType, _ *ast.CommentGroup) {
		section, ok := sectionByType[name]
		if !ok {
			return
		}
		for _, f := range st.Fields.List {
			if len(f.Names) == 0 || f.Tag == nil {
				continue
			}
			tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
			fieldName := stripOmit(tag.Get("toml"))
			if fieldName == "" {
				continue
			}
			rows = append(rows, tomlField{
				Section: section,
				Name:    fieldName,
				GoType:  goTypeName(f.Type),
				Doc:     firstSentence(commentText(f.Doc)),
			})
		}
	})

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Section != rows[j].Section {
			return rows[i].Section < rows[j].Section
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

// forEachStruct calls fn for every named struct type in file.
func forEachStruct(file *ast.File, fn func(name string, st *ast.StructType, doc *ast.CommentGroup)) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			// Prefer the per-spec doc, fall back to the GenDecl doc
			// (Go puts the comment on GenDecl when there's exactly
			// one spec in a `type T struct {}` block).
			doc := ts.Doc
			if doc == nil {
				doc = gen.Doc
			}
			fn(ts.Name.Name, st, doc)
		}
	}
}

// stripOmit strips ",omitempty" and similar from a TOML tag.
func stripOmit(s string) string {
	if before, _, ok := strings.Cut(s, ","); ok {
		return before
	}
	return s
}

// goTypeName renders an ast.Expr type back to source-form text.
// Handles the shapes we see in config.go: identifiers, pointer,
// selector (e.g. time.Duration), map.
func goTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + goTypeName(t.X)
	case *ast.SelectorExpr:
		return goTypeName(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + goTypeName(t.Key) + "]" + goTypeName(t.Value)
	case *ast.ArrayType:
		return "[]" + goTypeName(t.Elt)
	default:
		return "?"
	}
}

// commentText flattens a CommentGroup to a single line of prose,
// stripping leading "//" markers.
func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	var lines []string
	for _, c := range cg.List {
		line := strings.TrimPrefix(c.Text, "//")
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

// firstSentence returns the first sentence-ish chunk of s. Heuristic:
// split on ". " and return the first segment. The full Go comment
// often has multiple paragraphs of context; the reference table only
// wants the headline.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// scanEnvVars walks root for *.go files and collects every
// `// docgen:env NAME | description` comment.
func scanEnvVars(root string) ([]envVar, error) {
	var out []envVar
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendor / hidden dirs / build artefacts.
			base := info.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" || strings.HasPrefix(base, ".") {
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		// Allow long doc comments without bufio.Scanner's 64KiB cap.
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, envPrefix) {
				continue
			}
			rest := strings.TrimPrefix(line, envPrefix)
			parts := strings.SplitN(rest, "|", 2)
			name := strings.TrimSpace(parts[0])
			desc := ""
			if len(parts) == 2 {
				desc = strings.TrimSpace(parts[1])
			}
			if name == "" {
				continue
			}
			out = append(out, envVar{Name: name, Desc: desc, File: path})
		}
		return sc.Err()
	})
	if err != nil {
		return nil, err
	}
	// Dedup by name (in case the same env var is referenced from
	// multiple files — take the first sighting).
	seen := map[string]bool{}
	uniq := make([]envVar, 0, len(out))
	for _, e := range out {
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Name < uniq[j].Name })
	return uniq, nil
}

// render emits the final markdown body.
func render(toml []tomlField, envs []envVar) string {
	out := &strings.Builder{}
	out.WriteString("# Configuration reference\n\n")
	out.WriteString("*Generated by `scripts/docgen/config-ref`. Do not edit by hand — run `make docs-regen` after changing `internal/config/config.go` or adding/removing a `// docgen:env` comment.*\n\n")

	out.WriteString("## TOML config\n\n")
	out.WriteString("Config file is loaded from `$IBKR_CONFIG`, else `$XDG_CONFIG_HOME/ibkr/config.toml`, else `$HOME/.config/ibkr/config.toml`. Every field is optional; absent fields take their documented default.\n\n")

	if len(toml) == 0 {
		out.WriteString("*No documented TOML fields.*\n\n")
	} else {
		out.WriteString("| Section | Field | Type | Description |\n")
		out.WriteString("|---------|-------|------|-------------|\n")
		for _, f := range toml {
			fmt.Fprintf(out, "| `[%s]` | `%s` | `%s` | %s |\n",
				f.Section, f.Name, f.GoType, escapeTable(f.Doc))
		}
		out.WriteString("\n")
	}

	out.WriteString("## Environment variables\n\n")
	out.WriteString("Read at process startup. Override TOML config where applicable; see the per-var description for precedence rules.\n\n")

	if len(envs) == 0 {
		out.WriteString("*No documented environment variables. To document a variable, add a `// docgen:env NAME | description` comment next to its `os.Getenv` site.*\n\n")
	} else {
		out.WriteString("| Variable | Description |\n")
		out.WriteString("|----------|-------------|\n")
		for _, e := range envs {
			fmt.Fprintf(out, "| `%s` | %s |\n", e.Name, escapeTable(e.Desc))
		}
		out.WriteString("\n")
	}
	return out.String()
}

// escapeTable escapes characters that would break a markdown table row.
func escapeTable(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
