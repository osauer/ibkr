package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const docsRoot = "docs"

var (
	checkOnly = flag.Bool("check", false, "verify checked-in HTML pages match generated Markdown renderings")

	linkRE        = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	boldRE        = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicRE      = regexp.MustCompile(`\b_([^_]+)_\b|\*([^*]+)\*`)
	orderedListRE = regexp.MustCompile(`^[0-9]+\.\s+(.+)$`)
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "site-html: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var markdownPaths []string
	if err := filepath.WalkDir(docsRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			markdownPaths = append(markdownPaths, path)
		}
		return nil
	}); err != nil {
		return err
	}
	slices.Sort(markdownPaths)

	var problems []string
	for _, markdownPath := range markdownPaths {
		rendered, err := renderFile(markdownPath)
		if err != nil {
			return err
		}
		htmlPath := strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath)) + ".html"
		if *checkOnly {
			got, err := os.ReadFile(htmlPath)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s missing; run `make docs-regen`", htmlPath))
				continue
			}
			if !bytes.Equal(got, rendered) {
				problems = append(problems, fmt.Sprintf("%s out of date; run `make docs-regen`", htmlPath))
			}
			continue
		}
		info, err := os.Stat(markdownPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(htmlPath, rendered, info.Mode().Perm()); err != nil {
			return err
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("\n  - %s", strings.Join(problems, "\n  - "))
	}
	if *checkOnly {
		fmt.Printf("site-html: %d generated HTML pages are current\n", len(markdownPaths))
	} else {
		fmt.Printf("site-html: generated %d HTML pages\n", len(markdownPaths))
	}
	return nil
}

func renderFile(markdownPath string) ([]byte, error) {
	data, err := os.ReadFile(markdownPath)
	if err != nil {
		return nil, err
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	title := documentTitle(source, markdownPath)
	body := renderMarkdown(source, markdownPath)

	dir := filepath.Dir(strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath)) + ".html")
	rel := func(target string) string {
		out, err := filepath.Rel(dir, filepath.Join(docsRoot, target))
		if err != nil {
			return target
		}
		return filepath.ToSlash(out)
	}

	var out strings.Builder
	out.WriteString("<!doctype html>\n")
	out.WriteString("<html lang=\"en\">\n<head>\n")
	out.WriteString("  <meta charset=\"utf-8\">\n")
	out.WriteString("  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	out.WriteString("  <title>")
	out.WriteString(html.EscapeString(title))
	out.WriteString(" | ibkr</title>\n")
	out.WriteString("  <link rel=\"canonical\" href=\"https://osauer.dev/ibkr/")
	canon, _ := filepath.Rel(docsRoot, strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath))+".html")
	out.WriteString(html.EscapeString(filepath.ToSlash(canon)))
	out.WriteString("\">\n")
	out.WriteString("  <style>\n")
	out.WriteString(siteCSS)
	out.WriteString("  </style>\n")
	out.WriteString("</head>\n<body>\n")
	out.WriteString("  <div class=\"topline\"></div>\n")
	out.WriteString("  <header class=\"wrap nav\" aria-label=\"Primary\">\n")
	out.WriteString("    <a class=\"brand\" href=\"")
	out.WriteString(rel("index.html"))
	out.WriteString("\">ibkr</a>\n")
	out.WriteString("    <nav class=\"nav-links\" aria-label=\"Site\">\n")
	out.WriteString("      <a href=\"")
	out.WriteString(rel("index.html"))
	out.WriteString("#install\">Install</a>\n")
	out.WriteString("      <a href=\"https://github.com/osauer/ibkr\">GitHub</a>\n")
	out.WriteString("      <a href=\"")
	out.WriteString(rel("reference/mcp-tools.html"))
	out.WriteString("\">MCP tools</a>\n")
	out.WriteString("      <a href=\"")
	out.WriteString(rel("guides/agentic-use.html"))
	out.WriteString("\">Agent guide</a>\n")
	out.WriteString("    </nav>\n")
	out.WriteString("  </header>\n")
	out.WriteString("  <main class=\"wrap doc\">\n")
	out.WriteString(body)
	out.WriteString("  </main>\n")
	out.WriteString("  <footer><div class=\"wrap\"><a href=\"")
	out.WriteString(rel("index.html"))
	out.WriteString("\">ibkr</a><a href=\"https://github.com/osauer/ibkr\">GitHub</a><a href=\"https://github.com/osauer/ibkr/blob/main/PRIVACY.md\">Privacy</a><a href=\"https://github.com/osauer/ibkr/blob/main/SECURITY.md\">Security</a></div></footer>\n")
	out.WriteString("</body>\n</html>\n")
	return []byte(out.String()), nil
}

func documentTitle(source, markdownPath string) string {
	for line := range strings.SplitSeq(source, "\n") {
		if title, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(stripInlineMarkup(title))
		}
	}
	base := strings.TrimSuffix(filepath.Base(markdownPath), filepath.Ext(markdownPath))
	return strings.ReplaceAll(base, "-", " ")
}

func renderMarkdown(source, markdownPath string) string {
	lines := strings.Split(source, "\n")
	var out strings.Builder
	headingIDs := map[string]int{}

	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			i++
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++
			}
			out.WriteString("<pre><code>")
			out.WriteString(html.EscapeString(strings.Join(code, "\n")))
			out.WriteString("</code></pre>\n")
			continue
		}
		if headingLevel(trimmed) > 0 {
			level := headingLevel(trimmed)
			text := strings.TrimSpace(trimmed[level+1:])
			id := uniqueSlug(text, headingIDs)
			out.WriteString(fmt.Sprintf("<h%d id=\"%s\">%s</h%d>\n", level, id, renderInline(text, markdownPath), level))
			i++
			continue
		}
		if isTableStart(lines, i) {
			next, table := renderTable(lines, i, markdownPath)
			out.WriteString(table)
			i = next
			continue
		}
		if _, ok := unorderedListItem(trimmed); ok {
			out.WriteString("<ul>\n")
			for i < len(lines) {
				item, ok := unorderedListItem(strings.TrimSpace(lines[i]))
				if !ok {
					break
				}
				out.WriteString("<li>")
				out.WriteString(renderInline(item, markdownPath))
				out.WriteString("</li>\n")
				i++
			}
			out.WriteString("</ul>\n")
			continue
		}
		if _, ok := orderedListItem(trimmed); ok {
			out.WriteString("<ol>\n")
			for i < len(lines) {
				item, ok := orderedListItem(strings.TrimSpace(lines[i]))
				if !ok {
					break
				}
				out.WriteString("<li>")
				out.WriteString(renderInline(item, markdownPath))
				out.WriteString("</li>\n")
				i++
			}
			out.WriteString("</ol>\n")
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			var quote []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				quote = append(quote, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
				i++
			}
			out.WriteString("<blockquote><p>")
			out.WriteString(renderInline(strings.Join(quote, " "), markdownPath))
			out.WriteString("</p></blockquote>\n")
			continue
		}
		if isRule(trimmed) {
			out.WriteString("<hr>\n")
			i++
			continue
		}

		var paragraph []string
		for i < len(lines) && !startsBlock(lines, i) {
			paragraph = append(paragraph, strings.TrimSpace(lines[i]))
			i++
		}
		out.WriteString("<p>")
		out.WriteString(renderInline(strings.Join(paragraph, " "), markdownPath))
		out.WriteString("</p>\n")
	}

	return out.String()
}

func headingLevel(line string) int {
	for level := 1; level <= 6; level++ {
		prefix := strings.Repeat("#", level) + " "
		if strings.HasPrefix(line, prefix) {
			return level
		}
	}
	return 0
}

func startsBlock(lines []string, i int) bool {
	if i >= len(lines) {
		return true
	}
	trimmed := strings.TrimSpace(lines[i])
	if trimmed == "" {
		return true
	}
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, ">") || headingLevel(trimmed) > 0 || isRule(trimmed) || isTableStart(lines, i) {
		return true
	}
	if _, ok := unorderedListItem(trimmed); ok {
		return true
	}
	if _, ok := orderedListItem(trimmed); ok {
		return true
	}
	return false
}

func unorderedListItem(line string) (string, bool) {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return strings.TrimSpace(line[2:]), true
	}
	return "", false
}

func orderedListItem(line string) (string, bool) {
	match := orderedListRE.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	return strings.TrimSpace(match[1]), true
}

func isRule(line string) bool {
	return line == "---" || line == "***" || line == "___"
}

func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(lines[i]), "|") && isTableSeparator(lines[i+1])
}

func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return false
	}
	for _, r := range strings.Trim(trimmed, "| ") {
		if r != '-' && r != ':' && r != '|' && !unicode.IsSpace(r) {
			return false
		}
	}
	return strings.Contains(trimmed, "-")
}

func renderTable(lines []string, start int, markdownPath string) (int, string) {
	headers := tableCells(lines[start])
	var rows [][]string
	i := start + 2
	for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
		rows = append(rows, tableCells(lines[i]))
		i++
	}

	var out strings.Builder
	out.WriteString("<table>\n<thead><tr>")
	for _, cell := range headers {
		out.WriteString("<th>")
		out.WriteString(renderInline(cell, markdownPath))
		out.WriteString("</th>")
	}
	out.WriteString("</tr></thead>\n<tbody>\n")
	for _, row := range rows {
		out.WriteString("<tr>")
		for _, cell := range row {
			out.WriteString("<td>")
			out.WriteString(renderInline(cell, markdownPath))
			out.WriteString("</td>")
		}
		out.WriteString("</tr>\n")
	}
	out.WriteString("</tbody>\n</table>\n")
	return i, out.String()
}

func tableCells(line string) []string {
	trimmed := strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(trimmed, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func renderInline(text, markdownPath string) string {
	if strings.HasPrefix(text, "*") && strings.HasSuffix(text, "*") && !strings.HasPrefix(text, "**") && len(text) > 2 {
		return "<em>" + renderInline(strings.TrimSuffix(strings.TrimPrefix(text, "*"), "*"), markdownPath) + "</em>"
	}
	matches := linkRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return renderInlineWithoutLinks(text)
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(renderInlineWithoutLinks(text[last:match[0]]))
		label := text[match[2]:match[3]]
		href := text[match[4]:match[5]]
		out.WriteString("<a href=\"")
		out.WriteString(html.EscapeString(rewriteLink(href, markdownPath)))
		out.WriteString("\">")
		out.WriteString(renderInlineWithoutLinks(label))
		out.WriteString("</a>")
		last = match[1]
	}
	out.WriteString(renderInlineWithoutLinks(text[last:]))
	return out.String()
}

func renderInlineWithoutLinks(text string) string {
	var out strings.Builder
	for {
		start := strings.Index(text, "`")
		if start < 0 {
			out.WriteString(formatPlainInline(text))
			return out.String()
		}
		out.WriteString(formatPlainInline(text[:start]))
		rest := text[start+1:]
		before, after, ok := strings.Cut(rest, "`")
		if !ok {
			out.WriteString(html.EscapeString(text[start:]))
			return out.String()
		}
		out.WriteString("<code>")
		out.WriteString(html.EscapeString(before))
		out.WriteString("</code>")
		text = after
	}
}

func formatPlainInline(text string) string {
	escaped := html.EscapeString(text)
	escaped = boldRE.ReplaceAllString(escaped, "<strong>$1</strong>")
	escaped = italicRE.ReplaceAllStringFunc(escaped, func(match string) string {
		if strings.HasPrefix(match, "_") && strings.HasSuffix(match, "_") {
			return "<em>" + strings.Trim(match, "_") + "</em>"
		}
		if strings.HasPrefix(match, "*") && strings.HasSuffix(match, "*") {
			return "<em>" + strings.Trim(match, "*") + "</em>"
		}
		return match
	})
	return escaped
}

func rewriteLink(href, markdownPath string) string {
	if href == "" || strings.HasPrefix(href, "#") || strings.Contains(href, "://") || strings.HasPrefix(href, "mailto:") {
		return href
	}
	pathPart, anchor, _ := strings.Cut(href, "#")
	if !strings.HasSuffix(strings.ToLower(pathPart), ".md") {
		if target := rewriteNonMarkdownLink(pathPart, anchor, markdownPath); target != "" {
			return target
		}
		return href
	}

	sourceDir := filepath.Dir(markdownPath)
	target := filepath.Clean(filepath.Join(sourceDir, filepath.FromSlash(pathPart)))
	if rel, err := filepath.Rel(docsRoot, target); err == nil && !strings.HasPrefix(rel, "..") {
		htmlTarget := strings.TrimSuffix(target, filepath.Ext(target)) + ".html"
		currentHTMLDir := filepath.Dir(strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath)) + ".html")
		out, err := filepath.Rel(currentHTMLDir, htmlTarget)
		if err == nil {
			if anchor != "" {
				return filepath.ToSlash(out) + "#" + anchor
			}
			return filepath.ToSlash(out)
		}
	}

	repoPath := filepath.ToSlash(target)
	if anchor != "" {
		return "https://github.com/osauer/ibkr/blob/main/" + repoPath + "#" + anchor
	}
	return "https://github.com/osauer/ibkr/blob/main/" + repoPath
}

func rewriteNonMarkdownLink(pathPart, anchor, markdownPath string) string {
	if pathPart == "" {
		return ""
	}
	sourceDir := filepath.Dir(markdownPath)
	target := filepath.Clean(filepath.Join(sourceDir, filepath.FromSlash(pathPart)))
	info, err := os.Stat(target)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		indexPath := filepath.Join(target, "index.html")
		if _, err := os.Stat(indexPath); err == nil {
			currentHTMLDir := filepath.Dir(strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath)) + ".html")
			out, err := filepath.Rel(currentHTMLDir, indexPath)
			if err == nil {
				return filepath.ToSlash(out) + anchorSuffix(anchor)
			}
		}
		return "https://github.com/osauer/ibkr/tree/main/" + filepath.ToSlash(target)
	}
	if isUnderDocs(target) {
		currentHTMLDir := filepath.Dir(strings.TrimSuffix(markdownPath, filepath.Ext(markdownPath)) + ".html")
		out, err := filepath.Rel(currentHTMLDir, target)
		if err == nil {
			return filepath.ToSlash(out) + anchorSuffix(anchor)
		}
	}
	return "https://github.com/osauer/ibkr/blob/main/" + filepath.ToSlash(target) + anchorSuffix(anchor)
}

func isUnderDocs(path string) bool {
	rel, err := filepath.Rel(docsRoot, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func anchorSuffix(anchor string) string {
	if anchor == "" {
		return ""
	}
	return "#" + anchor
}

func stripInlineMarkup(text string) string {
	text = strings.ReplaceAll(text, "`", "")
	text = strings.ReplaceAll(text, "**", "")
	text = strings.Trim(text, "# ")
	return text
}

func uniqueSlug(text string, seen map[string]int) string {
	base := slug(text)
	count := seen[base]
	seen[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, count)
}

func slug(text string) string {
	text = strings.ToLower(stripInlineMarkup(text))
	var out strings.Builder
	lastDash := false
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
			lastDash = false
		case r == '-' || unicode.IsSpace(r):
			if !lastDash && out.Len() > 0 {
				out.WriteByte('-')
				lastDash = true
			}
		}
	}
	return cmp.Or(strings.Trim(out.String(), "-"), "section")
}

const siteCSS = `
    :root {
      --paper: #f7f5ef;
      --paper-strong: #fffdf7;
      --ink: #101827;
      --muted: #42526a;
      --line: #d8d2c4;
      --green: #087f6b;
      --green-dark: #055f52;
      --terminal: #0e1626;
    }
    * { box-sizing: border-box; }
    html { scroll-behavior: smooth; }
    body {
      margin: 0;
      background: var(--paper);
      color: var(--ink);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      line-height: 1.58;
    }
    a { color: var(--green-dark); text-underline-offset: 0.18em; }
    .topline { height: 12px; background: var(--green); }
    .wrap { width: min(1040px, calc(100% - 40px)); margin: 0 auto; }
    .nav {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 24px;
      padding: 22px 0 14px;
    }
    .brand {
      color: var(--ink);
      font-size: 28px;
      font-weight: 800;
      letter-spacing: 0;
      text-decoration: none;
    }
    .nav-links { display: flex; align-items: center; gap: 22px; color: var(--muted); font-size: 15px; font-weight: 650; }
    .nav-links a { color: var(--muted); text-decoration: none; }
    .doc {
      padding: 28px 0 70px;
    }
    .doc h1 {
      max-width: 820px;
      margin: 0 0 18px;
      font-size: clamp(36px, 5vw, 56px);
      line-height: 1.05;
      letter-spacing: 0;
    }
    .doc h2 {
      margin: 44px 0 14px;
      padding-top: 18px;
      border-top: 1px solid var(--line);
      font-size: clamp(26px, 3vw, 34px);
      line-height: 1.15;
    }
    .doc h3 { margin: 30px 0 10px; font-size: 22px; line-height: 1.22; }
    .doc h4 { margin: 24px 0 8px; font-size: 18px; }
    .doc p, .doc li { color: #26364d; font-size: 17px; }
    .doc ul, .doc ol { padding-left: 26px; }
    .doc li { margin: 6px 0; }
    .doc code {
      border: 1px solid #d5cdbc;
      border-radius: 5px;
      background: var(--paper-strong);
      padding: 0.08em 0.28em;
      color: #172033;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 0.92em;
    }
    .doc pre {
      overflow-x: auto;
      border-radius: 8px;
      background: var(--terminal);
      padding: 18px;
      border: 1px solid #26364c;
    }
    .doc pre code {
      border: 0;
      background: transparent;
      padding: 0;
      color: #eef3ff;
      font-size: 14px;
    }
    .doc blockquote {
      margin: 20px 0;
      padding: 10px 18px;
      border-left: 4px solid var(--green);
      background: var(--paper-strong);
    }
    .doc table {
      width: 100%;
      border-collapse: collapse;
      margin: 18px 0 26px;
      background: var(--paper-strong);
      font-size: 15px;
    }
    .doc th, .doc td {
      border: 1px solid var(--line);
      padding: 10px 12px;
      vertical-align: top;
      text-align: left;
    }
    .doc th { color: var(--ink); background: #ece7db; }
    footer {
      border-top: 1px solid var(--line);
      background: var(--paper-strong);
      padding: 28px 0;
    }
    footer .wrap { display: flex; flex-wrap: wrap; gap: 18px; }
    @media (max-width: 760px) {
      .nav { align-items: flex-start; flex-direction: column; }
      .nav-links { flex-wrap: wrap; gap: 12px; }
      .wrap { width: min(100% - 28px, 1040px); }
    }
`
