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
