package main

import (
	"html"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestManifestCoversTrackedTwins(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(tracked); err != nil {
		t.Fatal(err)
	}
	if got, want := len(pages), 15; got != want {
		t.Fatalf("manifest has %d pages, want %d", got, want)
	}
}

func TestRewriteDestination(t *testing.T) {
	renderer := newSiteRenderer(repoRoot(t), map[string]bool{
		"SECURITY.md":                  true,
		"docs/reference/config.md":     true,
		"docs/guides/updating.md":      true,
		"docs/diagrams/example.svg":    true,
		"docs/design/internal-only.md": true,
	})
	cases := map[string]string{
		"../reference/config.md?view=full#limits": "../reference/config.html?view=full#limits",
		"../../SECURITY.md#release-integrity":     "https://github.com/osauer/ibkr/blob/main/SECURITY.md#release-integrity",
		"../design/internal-only.md#details":      "https://github.com/osauer/ibkr/blob/main/docs/design/internal-only.md#details",
		"../diagrams/example.svg":                 "../diagrams/example.svg",
		"../../LOCAL.md":                          "../../LOCAL.md",
		"#reference":                              "#reference",
		"https://example.com/a.md#x":              "https://example.com/a.md#x",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := string(renderer.rewriteDestination("docs/guides/updating.md", []byte(input)))
			if got != want {
				t.Fatalf("rewriteDestination(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestRenderIsDeterministicAndGeneratorOwned(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	renderer := newSiteRenderer(root, tracked)
	page := pageForSource(t, "docs/concepts.md")
	first, err := renderer.render(page)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderer.render(page)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("render output is not deterministic")
	}
	text := string(first)
	if strings.Contains(text, "md-source:") {
		t.Fatal("generated output still contains a human-stamp marker")
	}
	if !strings.Contains(text, "Generated from Markdown by scripts/docgen/docs-html. DO NOT EDIT.") {
		t.Fatal("generated output lacks generator ownership notice")
	}
}

func TestConfigRuntimeSettingsRowsMatchRegistry(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := newSiteRenderer(root, tracked).render(pageForSource(t, "docs/reference/config.md"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractRuntimeSettingsRows(string(rendered))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]settingsRow, 0, len(rpc.SettingsKeys()))
	for _, spec := range rpc.SettingsKeys() {
		key := spec.Key
		grammar := ""
		switch spec.Kind {
		case rpc.SettingsKindBool:
			grammar = "true/false/null"
		case rpc.SettingsKindFloat:
			grammar = "number/null"
		case rpc.SettingsKindInt:
			grammar = "integer/null"
		case rpc.SettingsKindDateMap:
			key += ".<SYMBOL>"
			grammar = "YYYY-MM-DD[Tamc/Tbmo]/null"
		default:
			t.Fatalf("unhandled settings kind %q", spec.Kind)
		}
		want = append(want, settingsRow{Key: key, Grammar: grammar, Class: spec.Class, Description: spec.Doc})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public runtime-settings table does not match rpc.SettingsKeys()\n got: %#v\nwant: %#v", got, want)
	}
}

type settingsRow struct {
	Key         string
	Grammar     string
	Class       string
	Description string
}

var (
	settingsRowPattern = regexp.MustCompile(`(?s)<tr>\s*<td><code>(.*?)</code></td>\s*<td><code>(.*?)</code></td>\s*<td>(.*?)</td>\s*<td>(.*?)</td>\s*</tr>`)
	htmlTagPattern     = regexp.MustCompile(`<[^>]+>`)
)

func extractRuntimeSettingsRows(document string) ([]settingsRow, error) {
	start := strings.Index(document, `<h2 id="runtime-platform-settings">`)
	end := strings.Index(document, `<h2 id="environment-variables">`)
	if start < 0 || end < 0 || end <= start {
		return nil, &sectionError{"runtime platform settings section is missing"}
	}
	section := document[start:end]
	matches := settingsRowPattern.FindAllStringSubmatch(section, -1)
	rows := make([]settingsRow, 0, len(matches))
	for _, match := range matches {
		rows = append(rows, settingsRow{
			Key:         normalizedCell(match[1]),
			Grammar:     normalizedCell(match[2]),
			Class:       normalizedCell(match[3]),
			Description: normalizedCell(match[4]),
		})
	}
	return rows, nil
}

func normalizedCell(value string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTagPattern.ReplaceAllString(value, "")))
}

type sectionError struct{ message string }

func (e *sectionError) Error() string { return e.message }

func pageForSource(t *testing.T, source string) pageSpec {
	t.Helper()
	for _, page := range pages {
		if page.Source == source {
			return page
		}
	}
	t.Fatalf("page %s is not in manifest", source)
	return pageSpec{}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
