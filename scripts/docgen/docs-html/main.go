// Command docs-html renders the public HTML twins from their Markdown sources.
// Markdown is the only prose authority; the checked-in HTML exists because the
// current GitHub Pages setup publishes docs/ as static files.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const publicBaseURL = "https://osauer.dev/ibkr/"

type pageSpec struct {
	Source      string
	Description string
	Layout      string
	SocialImage string
}

func (p pageSpec) output() string {
	return strings.TrimSuffix(p.Source, ".md") + ".html"
}

var pages = []pageSpec{
	{
		Source:      "docs/architecture.md",
		Description: "System architecture, protocols, external data flows, process boundaries, and local persistence for ibkr Canary.",
		Layout:      "architecture",
		SocialImage: "diagrams/system-architecture.png",
	},
	{
		Source:      "docs/concepts.md",
		Description: "What the load-bearing market, portfolio, and data-quality context surfaces measure, and how to read them without mis-acting on the output.",
	},
	{
		Source:      "docs/design/gamma-zero-cache-persistence.md",
		Description: "Design and invalidation semantics for the daemon's persistent dealer zero-gamma cache.",
	},
	{
		Source:      "docs/guides/agentic-use.md",
		Description: "Practical agentic workflows, limits, and examples for using ibkr through an MCP host.",
	},
	{
		Source:      "docs/guides/marketplace-readiness.md",
		Description: "Maintainer checklist for packaging, trust, documentation, and release readiness in AI tool marketplaces.",
	},
	{
		Source:      "docs/guides/updating.md",
		Description: "How to update the ibkr binary, Desktop extension, S&P 500 membership, calendars, and local process state.",
	},
	{
		Source:      "docs/reference/config.md",
		Description: "Generated reference for ibkr TOML configuration, policy files, runtime platform settings, and environment variables.",
	},
	{
		Source:      "docs/reference/mcp-resources.md",
		Description: "Reference for the non-tool resources exposed by ibkr mcp, including live quote subscriptions.",
	},
	{
		Source:      "docs/reference/mcp-tools.md",
		Description: "Generated reference for every tool exposed by ibkr mcp, including parameters and invocation guidance.",
	},
	{
		Source:      "docs/reference/protocol.md",
		Description: "Coverage and semantic fingerprints for the clean-room Go implementation of the Interactive Brokers TWS wire protocol.",
	},
	{
		Source:      "docs/specs/regime-backtest-plan.md",
		Description: "Runbook for proving and tuning the ibkr regime and Canary lifecycle against point-in-time evidence.",
	},
	{
		Source:      "docs/specs/risk-regime-dashboard.md",
		Description: "Contract for the broad-market regime dashboard, source quality, cluster logic, lifecycle decisions, and backtesting.",
	},
}

type headingInfo struct {
	Level int
	ID    string
	Text  string
}

type templateData struct {
	Title           string
	Description     string
	Canonical       string
	RootPrefix      string
	Layout          string
	GeneratorNotice template.HTML
	Body            template.HTML
	JSONLD          template.JS
	SocialHead      template.HTML
}

var documentTemplate = template.Must(template.New("document").Parse(`<!doctype html>
{{.GeneratorNotice}}
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} | ibkr</title>
  <meta name="description" content="{{.Description}}">
  <link rel="canonical" href="{{.Canonical}}">
  <link rel="icon" type="image/png" href="{{.RootPrefix}}social/canary-icon.png">
{{.SocialHead}}
  <script type="application/ld+json">{{.JSONLD}}</script>
  <link rel="stylesheet" href="{{.RootPrefix}}shared.css">
</head>
<body class="layout-{{.Layout}}">
  <div class="topline"></div>
  <header class="wrap nav" aria-label="Primary">
    <a class="brand" href="{{.RootPrefix}}index.html" aria-label="ibkr canary home"><img src="{{.RootPrefix}}social/canary-icon.png" width="192" height="192" alt="">ibkr canary</a>
    <nav class="nav-links" aria-label="Site">
      <a href="{{.RootPrefix}}index.html#install">Install</a>
      <a href="https://github.com/osauer/ibkr">GitHub</a>
      <a href="{{.RootPrefix}}reference/mcp-tools.html">MCP tools</a>
      <a href="{{.RootPrefix}}guides/agentic-use.html">Agent guide</a>
    </nav>
  </header>
  <main class="wrap doc">
{{.Body}}  </main>
  <footer>
    <div class="wrap"><a href="{{.RootPrefix}}index.html">ibkr</a><a href="https://github.com/osauer/ibkr">GitHub</a><a href="https://github.com/osauer/ibkr/blob/main/PRIVACY.md">Privacy</a><a href="https://github.com/osauer/ibkr/blob/main/SECURITY.md">Security</a></div>
    <div class="wrap fineprint">Not financial advice. ibkr is analysis software; nothing here is a recommendation to buy or sell any security.</div>
  </footer>
</body>
</html>
`))

type siteRenderer struct {
	root      string
	generated map[string]string
	tracked   map[string]bool
	markdown  goldmark.Markdown
}

func newSiteRenderer(root string, tracked map[string]bool) *siteRenderer {
	generated := make(map[string]string, len(pages))
	for _, page := range pages {
		generated[filepath.ToSlash(page.Source)] = filepath.ToSlash(page.output())
	}
	return &siteRenderer{
		root:      root,
		generated: generated,
		tracked:   tracked,
		markdown: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
	}
}

func (r *siteRenderer) render(page pageSpec) ([]byte, error) {
	sourcePath := filepath.Join(r.root, filepath.FromSlash(page.Source))
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}

	doc := r.markdown.Parser().Parse(text.NewReader(source))
	headings, err := r.transform(doc, source, page.Source)
	if err != nil {
		return nil, err
	}
	if len(headings) == 0 || headings[0].Level != 1 || headings[0].Text == "" {
		return nil, fmt.Errorf("%s must start with one H1 title", page.Source)
	}
	for _, heading := range headings[1:] {
		if heading.Level == 1 {
			return nil, fmt.Errorf("%s has more than one H1", page.Source)
		}
	}

	var body bytes.Buffer
	if err := r.markdown.Renderer().Render(&body, source, doc); err != nil {
		return nil, err
	}
	bodyHTML := wrapTables(body.String())
	if page.Layout == "architecture" {
		bodyHTML = decorateArchitecture(bodyHTML, headings)
	}

	output := filepath.ToSlash(page.output())
	webPath := strings.TrimPrefix(output, "docs/")
	canonical := publicBaseURL + webPath
	rootPrefix := relativeRootPrefix(output)

	jsonLD, err := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "TechArticle",
		"headline":    headings[0].Text,
		"description": page.Description,
		"url":         canonical,
		"author": map[string]string{
			"@type": "Person",
			"name":  "Oliver Sauer",
			"url":   "https://github.com/osauer",
		},
		"isPartOf": map[string]string{
			"@type": "SoftwareApplication",
			"name":  "ibkr",
			"url":   publicBaseURL,
		},
	})
	if err != nil {
		return nil, err
	}

	layout := page.Layout
	if layout == "" {
		layout = "standard"
	}
	data := templateData{
		Title:           headings[0].Text,
		Description:     page.Description,
		Canonical:       canonical,
		RootPrefix:      rootPrefix,
		Layout:          layout,
		GeneratorNotice: template.HTML("<!-- Generated from Markdown by scripts/docgen/docs-html. DO NOT EDIT. -->"),
		Body:            template.HTML(bodyHTML), // Goldmark escapes source HTML by default.
		JSONLD:          template.JS(jsonLD),     // json.Marshal produces valid script data.
	}
	if page.SocialImage != "" {
		imageURL := publicBaseURL + strings.TrimPrefix(filepath.ToSlash(filepath.Join(filepath.Dir(webPath), page.SocialImage)), "./")
		data.SocialHead = template.HTML(fmt.Sprintf(
			"<meta property=\"og:type\" content=\"article\">\n  <meta property=\"og:title\" content=\"%s | ibkr\">\n  <meta property=\"og:description\" content=\"%s\">\n  <meta property=\"og:image\" content=\"%s\">\n  <meta name=\"twitter:card\" content=\"summary_large_image\">",
			template.HTMLEscapeString(data.Title), template.HTMLEscapeString(data.Description), template.HTMLEscapeString(imageURL),
		))
	}

	var out bytes.Buffer
	if err := documentTemplate.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (r *siteRenderer) transform(doc ast.Node, source []byte, sourcePath string) ([]headingInfo, error) {
	var headings []headingInfo
	err := ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.Heading:
			idValue, ok := n.AttributeString("id")
			if !ok {
				return ast.WalkStop, fmt.Errorf("heading in %s has no generated id", sourcePath)
			}
			id, ok := idValue.([]byte)
			if !ok {
				return ast.WalkStop, fmt.Errorf("heading id in %s has unexpected type %T", sourcePath, idValue)
			}
			headings = append(headings, headingInfo{Level: n.Level, ID: string(id), Text: headingText(n, source)})
		case *ast.Link:
			n.Destination = r.rewriteDestination(sourcePath, n.Destination)
		case *ast.Image:
			n.Destination = r.rewriteDestination(sourcePath, n.Destination)
		}
		return ast.WalkContinue, nil
	})
	return headings, err
}

func headingText(heading *ast.Heading, source []byte) string {
	var out strings.Builder
	for i := 0; i < heading.Lines().Len(); i++ {
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
		segment := heading.Lines().At(i)
		out.Write(segment.Value(source))
	}
	return strings.TrimSpace(out.String())
}

func (r *siteRenderer) rewriteDestination(sourcePath string, destination []byte) []byte {
	raw := string(destination)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Path == "" || strings.HasPrefix(raw, "#") {
		return destination
	}

	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(sourcePath), filepath.FromSlash(parsed.Path))))
	if output, ok := r.generated[resolved]; ok {
		rel, err := filepath.Rel(filepath.Dir(strings.TrimSuffix(sourcePath, ".md")+".html"), output)
		if err == nil {
			parsed.Path = filepath.ToSlash(rel)
			return []byte(parsed.String())
		}
	}

	if !strings.HasPrefix(resolved, "docs/") && r.tracked[resolved] {
		github := &url.URL{
			Scheme:   "https",
			Host:     "github.com",
			Path:     "/osauer/ibkr/blob/main/" + resolved,
			RawQuery: parsed.RawQuery,
			Fragment: parsed.Fragment,
		}
		return []byte(github.String())
	}
	if strings.HasPrefix(resolved, "docs/") && strings.HasSuffix(resolved, ".md") && r.tracked[resolved] {
		github := &url.URL{
			Scheme:   "https",
			Host:     "github.com",
			Path:     "/osauer/ibkr/blob/main/" + resolved,
			RawQuery: parsed.RawQuery,
			Fragment: parsed.Fragment,
		}
		return []byte(github.String())
	}
	return destination
}

func wrapTables(body string) string {
	body = strings.ReplaceAll(body, "<table>\n", "<div class=\"tblwrap\">\n<table>\n")
	body = strings.ReplaceAll(body, "</table>\n", "</table>\n</div>\n")
	return body
}

func decorateArchitecture(body string, headings []headingInfo) string {
	var toc strings.Builder
	toc.WriteString("<nav class=\"toc\" aria-label=\"On this page\">\n<span class=\"toc-label\">On this page</span>\n<ul>\n")
	section := 0
	for _, heading := range headings {
		if heading.Level != 2 {
			continue
		}
		section++
		fmt.Fprintf(&toc, "<li><a href=\"#%s\"><span class=\"n\">%02d</span>%s</a></li>\n",
			template.HTMLEscapeString(heading.ID), section, template.HTMLEscapeString(heading.Text))
	}
	toc.WriteString("</ul>\n</nav>\n")

	firstH2 := strings.Index(body, "<h2 ")
	if firstH2 >= 0 {
		body = body[:firstH2] + toc.String() + body[firstH2:]
	}
	section = 0
	for _, heading := range headings {
		if heading.Level != 2 {
			continue
		}
		section++
		open := fmt.Sprintf("<h2 id=\"%s\">", heading.ID)
		replacement := fmt.Sprintf("<h2 id=\"%s\"><span class=\"secno\" aria-hidden=\"true\">%02d</span>", heading.ID, section)
		body = strings.Replace(body, open, replacement, 1)
	}
	return body
}

func relativeRootPrefix(output string) string {
	rel, err := filepath.Rel(filepath.Dir(output), "docs")
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel) + "/"
}

func trackedFiles(root string) (map[string]bool, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	tracked := map[string]bool{}
	for item := range bytes.SplitSeq(out, []byte{0}) {
		if len(item) > 0 {
			tracked[filepath.ToSlash(string(item))] = true
		}
	}
	return tracked, nil
}

func validateManifest(tracked map[string]bool) error {
	declared := map[string]bool{}
	for _, page := range pages {
		source := filepath.ToSlash(page.Source)
		output := filepath.ToSlash(page.output())
		if declared[source] {
			return fmt.Errorf("duplicate manifest source %s", source)
		}
		declared[source] = true
		if !tracked[source] {
			return fmt.Errorf("manifest source is not tracked: %s", source)
		}
		if !tracked[output] {
			return fmt.Errorf("manifest output is not tracked: %s", output)
		}
	}

	var undeclared []string
	for path := range tracked {
		if !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
			continue
		}
		if tracked[strings.TrimSuffix(path, ".md")+".html"] && !declared[path] {
			undeclared = append(undeclared, path)
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		return fmt.Errorf("tracked Markdown/HTML twins missing from manifest: %s", strings.Join(undeclared, ", "))
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func run(root string, check bool) error {
	tracked, err := trackedFiles(root)
	if err != nil {
		return err
	}
	if err := validateManifest(tracked); err != nil {
		return err
	}
	renderer := newSiteRenderer(root, tracked)
	var stale []string
	for _, page := range pages {
		want, err := renderer.render(page)
		if err != nil {
			return fmt.Errorf("render %s: %w", page.Source, err)
		}
		output := filepath.Join(root, filepath.FromSlash(page.output()))
		if check {
			got, err := os.ReadFile(output)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				stale = append(stale, page.output())
			}
			continue
		}
		if err := writeAtomic(output, want); err != nil {
			return fmt.Errorf("write %s: %w", page.output(), err)
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("generated HTML is stale: %s; run make docs-html-regen", strings.Join(stale, ", "))
	}
	return nil
}

func main() {
	check := flag.Bool("check", false, "verify generated HTML without writing files")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}
	if err := run(absRoot, *check); err != nil {
		fatal(err)
	}
	if *check {
		fmt.Printf("docs-html-check: %d generated page(s) match Markdown sources\n", len(pages))
	} else {
		fmt.Printf("docs-html-regen: generated %d page(s)\n", len(pages))
	}
}

func fatal(err error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
