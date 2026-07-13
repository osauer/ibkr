package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDocumentedEnvReadsFindsLiteralAndConstant(t *testing.T) {
	root := t.TempDir()
	source := `package sample
import "os"
const envNamed = "IBKR_NAMED"
func read() { _, _ = os.LookupEnv(envNamed); _ = os.Getenv("IBKR_LITERAL") }
`
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	err := validateDocumentedEnvReads(root, nil)
	if err == nil || !strings.Contains(err.Error(), "IBKR_LITERAL") || !strings.Contains(err.Error(), "IBKR_NAMED") {
		t.Fatalf("missing-doc error = %v, want both environment names", err)
	}

	documented := []envVar{{Name: "IBKR_LITERAL"}, {Name: "IBKR_NAMED"}}
	if err := validateDocumentedEnvReads(root, documented); err != nil {
		t.Fatalf("documented reads rejected: %v", err)
	}
}

func TestValidateDocumentedEnvReadsIgnoresTestsAndNonIBKRNames(t *testing.T) {
	root := t.TempDir()
	production := `package sample
import "os"
func read() { _ = os.Getenv("HOME") }
`
	testSource := `package sample
import "os"
func testRead() { _ = os.Getenv("IBKR_TEST_ONLY") }
`
	for name, body := range map[string]string{"sample.go": production, "sample_test.go": testSource} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateDocumentedEnvReads(root, nil); err != nil {
		t.Fatalf("non-public reads rejected: %v", err)
	}
}

func TestParseStructRowsRecursesNestedAndMapStructs(t *testing.T) {
	root := t.TempDir()
	source := `package sample
type Root struct {
	// Top is a top-level scalar.
	Top     string          ` + "`toml:\"top\"`" + `
	Nested  Inner           ` + "`toml:\"nested\"`" + `
	Ptr     *Inner          ` + "`toml:\"ptr\"`" + `
	Presets map[string]Leaf ` + "`toml:\"presets\"`" + `
}
type Inner struct {
	Deep Leaf ` + "`toml:\"deep\"`" + `
}
type Leaf struct {
	// Value is the leaf value.
	Value int ` + "`toml:\"value\"`" + `
}
`
	path := filepath.Join(root, "sample.go")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := parseStructRows(path, "Root")
	if err != nil {
		t.Fatalf("parseStructRows: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Path] = r.Doc
	}
	for _, want := range []string{
		"top",
		"nested.deep.value",
		"ptr.deep.value",
		"presets.<name>.value",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing row %q; got %v", want, got)
		}
	}
	if got["top"] != "Top is a top-level scalar." {
		t.Errorf("top doc = %q", got["top"])
	}
	if row := (tomlField{Path: "presets.<name>.value"}); row.Section() != "presets.<name>" || row.Field() != "value" {
		t.Errorf("section/field split = %q/%q", row.Section(), row.Field())
	}
	if _, err := parseStructRows(path, "NoSuchStruct"); err == nil {
		t.Error("unknown root struct must error")
	}
}
