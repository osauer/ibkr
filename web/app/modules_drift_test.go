package appweb

import (
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const serviceWorkerFile = "service-worker.js"

var staticImportRE = regexp.MustCompile(`(?m)^\s*import\s+(?:[^"'\n;]*?\s+from\s+)?["']([^"']+)["']\s*;`)

func TestJavaScriptAssetsMatchEmbedAndImportGraph(t *testing.T) {
	t.Parallel()

	diskFiles := diskJavaScriptFiles(t)
	embeddedFiles := embeddedJavaScriptFilesFromDirective(t)
	for _, filename := range sortedSetDifference(diskFiles, embeddedFiles) {
		t.Errorf("JavaScript file %s exists on disk but is missing from assets.go go:embed", filename)
	}
	for _, filename := range sortedSetDifference(embeddedFiles, diskFiles) {
		t.Errorf("JavaScript file %s is listed in assets.go go:embed but is missing on disk", filename)
	}
	if !diskFiles[serviceWorkerFile] || !embeddedFiles[serviceWorkerFile] {
		t.Fatalf("%s must exist on disk and in assets.go go:embed before it can be excluded from the SPA module graph", serviceWorkerFile)
	}

	spaModules := maps.Clone(diskFiles)
	delete(spaModules, serviceWorkerFile)
	reachable := transitivelyImportedModules(t, "app.js")
	for _, filename := range sortedSetDifference(spaModules, reachable) {
		t.Errorf("SPA module %s exists on disk and is embedded but is unreachable from app.js", filename)
	}
	for _, filename := range sortedSetDifference(reachable, spaModules) {
		t.Errorf("app.js import graph reaches %s, but it is not an embedded on-disk SPA module", filename)
	}
}

// A query string on the app.js script tag gives the entry module a different
// URL than the bare "./app.js" the feature modules import, so the browser
// evaluates app.js twice: double event listeners, double main(), and a
// second doomed pairing attempt on every QR scan.
func TestIndexHTMLLoadsEntryModuleWithoutQuery(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, `<script src="/app.js" type="module">`) {
		t.Fatalf("index.html must load /app.js without a query string")
	}
	if strings.Contains(html, "/app.js?") {
		t.Fatalf("index.html must not reference app.js with a query string (module identity is URL-keyed)")
	}
}

func diskJavaScriptFiles(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read web/app directory: %v", err)
	}
	files := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() && path.Ext(entry.Name()) == ".js" {
			files[entry.Name()] = true
		}
	}
	return files
}

func embeddedJavaScriptFilesFromDirective(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "assets.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse assets.go: %v", err)
	}
	files := make(map[string]bool)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if !strings.HasPrefix(comment.Text, "//go:embed ") {
				continue
			}
			for filename := range strings.FieldsSeq(strings.TrimPrefix(comment.Text, "//go:embed ")) {
				if path.Ext(filename) != ".js" {
					continue
				}
				if path.Base(filename) != filename || strings.ContainsAny(filename, "*?[") {
					t.Fatalf("assets.go must list JavaScript files explicitly at the web/app root; got %q", filename)
				}
				files[filename] = true
			}
		}
	}
	return files
}

func transitivelyImportedModules(t *testing.T, entry string) map[string]bool {
	t.Helper()
	reachable := make(map[string]bool)
	var visit func(string)
	visit = func(filename string) {
		if reachable[filename] {
			return
		}
		reachable[filename] = true
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read imported SPA module %s: %v", filename, err)
		}
		for _, match := range staticImportRE.FindAllStringSubmatch(string(data), -1) {
			specifier := match[1]
			if !strings.HasPrefix(specifier, "./") {
				t.Fatalf("SPA module %s imports non-root specifier %q; use an exact ./filename.js import", filename, specifier)
			}
			target := strings.TrimPrefix(specifier, "./")
			if path.Base(target) != target || path.Ext(target) != ".js" {
				t.Fatalf("SPA module %s imports invalid module path %q; imports must name an exact root-level .js file", filename, specifier)
			}
			visit(target)
		}
	}
	visit(entry)
	return reachable
}

func sortedSetDifference(left, right map[string]bool) []string {
	difference := make(map[string]bool)
	for filename := range left {
		if !right[filename] {
			difference[filename] = true
		}
	}
	return slices.Sorted(maps.Keys(difference))
}
