package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPackageComments(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		rules   []string
		symbols []string
	}{
		{
			name:  "missing",
			files: map[string]string{"sample/a.go": "package sample\n"},
			rules: []string{ruleMissingPackage}, symbols: []string{"package sample"},
		},
		{
			name: "duplicate",
			files: map[string]string{
				"sample/a.go": "// Package sample provides a sample.\npackage sample\n",
				"sample/b.go": "// Package sample contains another package comment.\npackage sample\n",
			},
			rules:   []string{ruleDuplicatePackage, ruleDuplicatePackage},
			symbols: []string{"package sample", "package sample"},
		},
		{
			name:  "malformed",
			files: map[string]string{"sample/a.go": "// Utilities for samples.\npackage sample\n"},
			rules: []string{ruleMalformedPackage}, symbols: []string{"package sample"},
		},
		{
			name:  "command valid",
			files: map[string]string{"widget/main.go": "// Command widget runs widgets.\npackage main\n"},
		},
		{
			name:  "command malformed",
			files: map[string]string{"widget/main.go": "// Package main runs widgets.\npackage main\n"},
			rules: []string{ruleMalformedPackage}, symbols: []string{"package main"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := writeFiles(t, test.files)
			findings, err := audit(paths)
			if err != nil {
				t.Fatal(err)
			}
			assertFindingFields(t, findings, test.rules, test.symbols)
		})
	}
}

func TestExportedDeclarations(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

// First is the first value.
const (
	First = 1
	Second = 2
	// Third is the third value.
	Third = 3
)

// Pair contains two related values.
var Pair, Partner int

// Thing is an example.
type Thing struct{}

// WrongName describes Good.
func (Thing) Good() {}

func (*Thing) Missing() {}

// Build builds a Thing.
func Build() Thing { return Thing{} }
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	assertFindingFields(t, findings,
		[]string{ruleMalformedExported, ruleMissingExported},
		[]string{"Thing.Good", "(*Thing).Missing"},
	)
}

func TestGroupedValuesMayUseOneGroupComment(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

// Status values returned by operations.
const (
	Ready = iota
	Waiting
)

// Shared process state.
var (
	Started bool
	Stopped bool
)
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("unexpected findings:\n%s", formatFindings(findings))
	}
}

func TestGroupedValuesMayUseIndividualComments(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

const (
	// Ready means work may begin.
	Ready = iota
	// Waiting means work is pending.
	Waiting
)
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("unexpected findings:\n%s", formatFindings(findings))
	}
}

func TestUndocumentedValueGroupGetsOneGroupFinding(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

const (
	First = iota
	Second
	Third
)
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	assertFindingFields(t, findings, []string{ruleMissingExported}, []string{"const group"})
	if got := findings[0].line; got != 4 {
		t.Fatalf("group finding line = %d, want declaration line 4", got)
	}
}

func TestGroupedValueSpecificCommentMustStartWithName(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

// Status values returned by operations.
const (
	// A waiting status.
	Waiting = iota
	Ready
)
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	assertFindingFields(t, findings, []string{ruleMalformedExported}, []string{"Waiting"})
}

func TestUngroupedValuesRemainStrict(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

const Missing = 1

// Wrong describes Named.
var Named int
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	assertFindingFields(t, findings,
		[]string{ruleMissingExported, ruleMalformedExported},
		[]string{"Missing", "Named"},
	)
}

func TestMethodsRequireAnExportedReceiverType(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"sample/sample.go": `// Package sample provides examples.
package sample

// Exported is an exported receiver.
type Exported struct{}

func (Exported) Missing() {}

type hidden struct{}

func (hidden) Visible() {}

// Generic is an exported generic receiver.
type Generic[T, U any] struct{}

func (*Generic[T, U]) MissingGeneric() {}

type privateGeneric[T any] struct{}

func (*privateGeneric[T]) VisibleGeneric() {}
`,
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	assertFindingFields(t, findings,
		[]string{ruleMissingExported, ruleMissingExported},
		[]string{"Exported.Missing", "(*Generic).MissingGeneric"},
	)
}

func TestTestOnlyGeneratedAndBuildTaggedFiles(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"onlytest/sample_test.go": "package onlytest\n\nfunc ExportedTestHelper() {}\n",
		"generated/data.go":       "// Code generated by fixture; DO NOT EDIT.\npackage generated\n\nfunc ExportedGenerated() {}\n",
		"tagged/doc.go":           "// Package tagged spans build alternatives.\npackage tagged\n",
		"tagged/unix.go":          "//go:build unix\n\npackage tagged\n\n// UnixValue is available on Unix.\nconst UnixValue = 1\n",
		"tagged/windows.go":       "//go:build windows\n\npackage tagged\n\n// WindowsValue is available on Windows.\nconst WindowsValue = 1\n",
	})
	findings, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("unexpected findings:\n%s", formatFindings(findings))
	}
}

func TestDeterministicFindings(t *testing.T) {
	paths := writeFiles(t, map[string]string{
		"z/z.go": "package z\n\nfunc Zebra() {}\n",
		"a/a.go": "package a\n\nfunc Alpha() {}\n",
	})
	reversed := append([]string(nil), paths...)
	sort.Sort(sort.Reverse(sort.StringSlice(reversed)))

	first, err := audit(paths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := audit(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("findings depend on input order:\nfirst:\n%s\nsecond:\n%s", formatFindings(first), formatFindings(second))
	}
	if got := first[0].String(); !strings.Contains(got, ":1: "+ruleMissingPackage+" [package a]") {
		t.Fatalf("finding lacks stable path:line/rule/symbol fields: %q", got)
	}
}

func TestModes(t *testing.T) {
	path := writeFiles(t, map[string]string{"sample/a.go": "package sample\n"})[0]
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{name: "default report", args: []string{path}, want: 0},
		{name: "report only", args: []string{"-report-only", path}, want: 0},
		{name: "check", args: []string{"-check", path}, want: 1},
		{name: "strict", args: []string{"-strict", path}, want: 1},
		{name: "conflict", args: []string{"-check", "-report-only", path}, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(test.args, &stdout, &stderr); got != test.want {
				t.Fatalf("run exit = %d, want %d; stderr=%q", got, test.want, stderr.String())
			}
			if test.want != 2 && !strings.Contains(stdout.String(), ruleMissingPackage) {
				t.Fatalf("stdout %q does not contain finding", stdout.String())
			}
		})
	}
}

func TestGitGoFilesUsesTrackedAndUntrackedNonignoredScope(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.go\n")
	writeFile(t, filepath.Join(root, "tracked.go"), "package tracked\n")
	writeFile(t, filepath.Join(root, "untracked.go"), "package untracked\n")
	writeFile(t, filepath.Join(root, "odd\nname.go"), "package odd\n")
	writeFile(t, filepath.Join(root, "ignored.go"), "package ignored\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "not Go\n")
	runGit(t, root, "add", "tracked.go")

	paths, err := gitGoFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "odd\nname.go"), filepath.Join(root, "tracked.go"), filepath.Join(root, "untracked.go")}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("gitGoFiles() = %#v, want %#v", paths, want)
	}

	if err := os.Remove(filepath.Join(root, "tracked.go")); err != nil {
		t.Fatal(err)
	}
	paths, err = gitGoFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{filepath.Join(root, "odd\nname.go"), filepath.Join(root, "untracked.go")}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("gitGoFiles() with deleted tracked file = %#v, want %#v", paths, want)
	}
}

func writeFiles(t *testing.T, files map[string]string) []string {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, len(files))
	for name, contents := range files {
		path := filepath.Join(root, name)
		writeFile(t, path, contents)
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", root}, args...)
	if output, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func assertFindingFields(t *testing.T, findings []finding, rules, symbols []string) {
	t.Helper()
	if len(findings) != len(rules) {
		t.Fatalf("got %d findings, want %d:\n%s", len(findings), len(rules), formatFindings(findings))
	}
	for i := range findings {
		if findings[i].rule != rules[i] || findings[i].symbol != symbols[i] {
			t.Errorf("finding %d = rule %q symbol %q, want rule %q symbol %q", i, findings[i].rule, findings[i].symbol, rules[i], symbols[i])
		}
	}
}

func formatFindings(findings []finding) string {
	var lines []string
	for _, finding := range findings {
		lines = append(lines, finding.String())
	}
	return strings.Join(lines, "\n")
}
