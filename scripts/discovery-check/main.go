package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	pluginManifestPath = ".claude-plugin/plugin.json"
	rootServerPath     = "server.json"
	docsMCPPath        = "docs/mcp-server.json"
	wellKnownMCPPath   = "docs/.well-known/mcp/server.json"
	docsSitemapPath    = "docs/sitemap.xml"
	docsLLMSPath       = "docs/llms.txt"
	indexNowKeyPath    = "docs/indexnow.txt"
)

var (
	serverNamePattern = regexp.MustCompile(`^[a-z0-9.-]+/[A-Za-z0-9._-]+$`)
	jsonLDScriptRE    = regexp.MustCompile(`(?is)<script\b[^>]*type=["']application/ld\+json["'][^>]*>(.*?)</script>`)

	jsonLDPages = []string{
		"docs/index.html",
		"docs/interactive-brokers-mcp-server/index.html",
	}

	requiredSitemapURLs = []string{
		"https://osauer.dev/ibkr/",
		"https://osauer.dev/ibkr/ibkr-mcp/",
		"https://osauer.dev/ibkr/interactive-brokers-mcp-server/",
		"https://osauer.dev/ibkr/tws-mcp-server/",
		"https://osauer.dev/ibkr/ib-gateway-mcp/",
		"https://osauer.dev/ibkr/claude-desktop-interactive-brokers/",
		"https://osauer.dev/ibkr/ibkr-claude-desktop-mcp/",
		"https://osauer.dev/ibkr/connect-claude-to-ibkr/",
		"https://osauer.dev/ibkr/best-ibkr-mcp-server-claude-code/",
		"https://osauer.dev/ibkr/analyze-interactive-brokers-portfolio-with-ai/",
		"https://osauer.dev/ibkr/read-only-mcp-server/",
		"https://osauer.dev/ibkr/guides/agentic-use.html",
		"https://osauer.dev/ibkr/reference/mcp-tools.html",
		"https://osauer.dev/ibkr/reference/mcp-resources.html",
	}

	requiredLLMSURLs = []string{
		"https://osauer.dev/ibkr/",
		"https://osauer.dev/ibkr/ibkr-mcp/",
		"https://osauer.dev/ibkr/interactive-brokers-mcp-server/",
		"https://osauer.dev/ibkr/tws-mcp-server/",
		"https://osauer.dev/ibkr/ib-gateway-mcp/",
		"https://osauer.dev/ibkr/claude-desktop-interactive-brokers/",
		"https://osauer.dev/ibkr/ibkr-claude-desktop-mcp/",
		"https://osauer.dev/ibkr/connect-claude-to-ibkr/",
		"https://osauer.dev/ibkr/best-ibkr-mcp-server-claude-code/",
		"https://osauer.dev/ibkr/analyze-interactive-brokers-portfolio-with-ai/",
		"https://osauer.dev/ibkr/read-only-mcp-server/",
		"https://github.com/osauer/ibkr",
		"https://osauer.dev/ibkr/reference/mcp-tools.html",
		"https://osauer.dev/ibkr/reference/mcp-resources.html",
	}
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "discovery-check: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var problems []string

	version, err := jsonStringField(pluginManifestPath, "version")
	if err != nil {
		problems = append(problems, err.Error())
	}
	if version == "" {
		problems = append(problems, ".claude-plugin/plugin.json version is empty")
	}

	checkRegistryServer(&problems, version)
	checkDocsMCPMetadata(&problems, version)
	checkMirroredMCPMetadata(&problems)
	checkJSONLDVersions(&problems, version)
	checkSitemap(&problems)
	checkLLMS(&problems)
	checkIndexNowKey(&problems)

	if len(problems) > 0 {
		return errors.New("\n  - " + strings.Join(problems, "\n  - "))
	}

	fmt.Printf("discovery-check: version %s across public discovery surfaces\n", version)
	return nil
}

func checkIndexNowKey(problems *[]string) {
	data, err := os.ReadFile(indexNowKeyPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	key := strings.TrimSpace(string(data))
	if utf8.RuneCountInString(key) < 8 || utf8.RuneCountInString(key) > 128 {
		*problems = append(*problems, indexNowKeyPath+" key must be 8 to 128 characters")
	}
	for _, r := range key {
		if r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		*problems = append(*problems, indexNowKeyPath+" key contains unsupported character")
		return
	}
}

func checkRegistryServer(problems *[]string, version string) {
	obj, err := readJSONObject(rootServerPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}

	checkStringValue(problems, rootServerPath, obj, "version", version)
	checkStringValue(problems, rootServerPath, obj, "websiteUrl", "https://osauer.dev/ibkr/")

	name, _ := obj["name"].(string)
	if !serverNamePattern.MatchString(name) || strings.Count(name, "/") != 1 {
		*problems = append(*problems, fmt.Sprintf("%s name must be reverse-DNS with exactly one slash (got %q)", rootServerPath, name))
	}

	description, _ := obj["description"].(string)
	if utf8.RuneCountInString(description) == 0 {
		*problems = append(*problems, rootServerPath+" description is empty")
	}
	if utf8.RuneCountInString(description) > 100 {
		*problems = append(*problems, fmt.Sprintf("%s description must be <= 100 characters", rootServerPath))
	}

	repo, ok := obj["repository"].(map[string]any)
	if !ok {
		*problems = append(*problems, rootServerPath+" repository must be an object")
		return
	}
	checkStringValue(problems, rootServerPath+" repository", repo, "url", "https://github.com/osauer/ibkr")
	checkStringValue(problems, rootServerPath+" repository", repo, "source", "github")
	if id, _ := repo["id"].(string); id == "" {
		*problems = append(*problems, rootServerPath+" repository.id is empty")
	}
}

func checkDocsMCPMetadata(problems *[]string, version string) {
	for _, path := range []string{docsMCPPath, wellKnownMCPPath} {
		obj, err := readJSONObject(path)
		if err != nil {
			*problems = append(*problems, err.Error())
			continue
		}

		checkStringValue(problems, path, obj, "version", version)
		checkStringValue(problems, path, obj, "homepage", "https://osauer.dev/ibkr/")
		checkStringValue(problems, path, obj, "repository", "https://github.com/osauer/ibkr")

		install, ok := obj["install"].(map[string]any)
		if !ok {
			*problems = append(*problems, path+" install must be an object")
		} else {
			checkStringValue(problems, path+" install", install, "mcpb", "https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb")
		}

		transport, ok := obj["transport"].(map[string]any)
		if !ok {
			*problems = append(*problems, path+" transport must be an object")
		} else {
			checkStringValue(problems, path+" transport", transport, "type", "stdio")
			checkStringValue(problems, path+" transport", transport, "command", "ibkr")
			args, ok := transport["args"].([]any)
			if !ok || len(args) != 1 || args[0] != "mcp" {
				*problems = append(*problems, path+` transport.args must be ["mcp"]`)
			}
		}

		safety, ok := obj["safety"].(map[string]any)
		if !ok {
			*problems = append(*problems, path+" safety must be an object")
			continue
		}
		for _, field := range []string{"order_entry_surface", "can_place_orders", "can_modify_orders", "can_cancel_orders"} {
			if value, ok := safety[field].(bool); !ok || value {
				*problems = append(*problems, fmt.Sprintf("%s safety.%s must be false", path, field))
			}
		}
	}
}

func checkMirroredMCPMetadata(problems *[]string) {
	left, err := readJSONAny(docsMCPPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	right, err := readJSONAny(wellKnownMCPPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	if !reflect.DeepEqual(left, right) {
		*problems = append(*problems, docsMCPPath+" and "+wellKnownMCPPath+" must contain matching JSON")
	}
}

func checkJSONLDVersions(problems *[]string, version string) {
	for _, path := range jsonLDPages {
		data, err := os.ReadFile(path)
		if err != nil {
			*problems = append(*problems, err.Error())
			continue
		}

		matches := jsonLDScriptRE.FindAllSubmatch(data, -1)
		if len(matches) == 0 {
			*problems = append(*problems, path+" has no application/ld+json script")
			continue
		}

		var versions []string
		for _, match := range matches {
			var value any
			if err := json.Unmarshal(bytes.TrimSpace(match[1]), &value); err != nil {
				*problems = append(*problems, fmt.Sprintf("%s has invalid JSON-LD: %v", path, err))
				continue
			}
			collectStringKey(value, "softwareVersion", &versions)
		}
		if len(versions) == 0 {
			*problems = append(*problems, path+" JSON-LD has no softwareVersion")
			continue
		}
		for _, got := range versions {
			if got != version {
				*problems = append(*problems, fmt.Sprintf("%s JSON-LD softwareVersion = %q, want %q", path, got, version))
			}
		}
	}
}

func checkSitemap(problems *[]string) {
	data, err := os.ReadFile(docsSitemapPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}

	var sitemap struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(data, &sitemap); err != nil {
		*problems = append(*problems, fmt.Sprintf("%s is invalid XML: %v", docsSitemapPath, err))
		return
	}

	seen := make(map[string]bool, len(sitemap.URLs))
	for _, entry := range sitemap.URLs {
		loc := strings.TrimSpace(entry.Loc)
		seen[loc] = true
		checkPublicURLFile(problems, docsSitemapPath, loc)
	}
	for _, url := range requiredSitemapURLs {
		if !seen[url] {
			*problems = append(*problems, docsSitemapPath+" missing "+url)
		}
	}
}

func checkLLMS(problems *[]string) {
	data, err := os.ReadFile(docsLLMSPath)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	text := string(data)
	for _, url := range requiredLLMSURLs {
		if !strings.Contains(text, url) {
			*problems = append(*problems, docsLLMSPath+" missing "+url)
		}
		checkPublicURLFile(problems, docsLLMSPath, url)
	}
}

func checkPublicURLFile(problems *[]string, source string, rawURL string) {
	const prefix = "https://osauer.dev/ibkr/"
	if !strings.HasPrefix(rawURL, prefix) {
		return
	}
	rel := strings.TrimPrefix(rawURL, prefix)
	if rel == "" {
		rel = "index.html"
	} else if strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}
	path := filepath.Clean(filepath.Join("docs", filepath.FromSlash(rel)))
	if path != "docs" && !strings.HasPrefix(path, "docs"+string(os.PathSeparator)) {
		*problems = append(*problems, fmt.Sprintf("%s public URL escapes docs root: %s", source, rawURL))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s public URL has no checked-in file: %s -> %s", source, rawURL, path))
		return
	}
	if info.IsDir() {
		*problems = append(*problems, fmt.Sprintf("%s public URL maps to directory, not file: %s -> %s", source, rawURL, path))
	}
}

func checkStringValue(problems *[]string, path string, obj map[string]any, field string, want string) {
	got, ok := obj[field].(string)
	if !ok {
		*problems = append(*problems, fmt.Sprintf("%s %s must be a string", path, field))
		return
	}
	if got != want {
		*problems = append(*problems, fmt.Sprintf("%s %s = %q, want %q", path, field, got, want))
	}
}

func collectStringKey(value any, key string, out *[]string) {
	switch value := value.(type) {
	case map[string]any:
		if got, ok := value[key].(string); ok {
			*out = append(*out, got)
		}
		for _, child := range value {
			collectStringKey(child, key, out)
		}
	case []any:
		for _, child := range value {
			collectStringKey(child, key, out)
		}
	}
}

func jsonStringField(path string, field string) (string, error) {
	obj, err := readJSONObject(path)
	if err != nil {
		return "", err
	}
	value, ok := obj[field].(string)
	if !ok {
		return "", fmt.Errorf("%s %s must be a string", path, field)
	}
	return value, nil
}

func readJSONObject(path string) (map[string]any, error) {
	var obj map[string]any
	if err := readJSON(path, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func readJSONAny(path string) (any, error) {
	var value any
	if err := readJSON(path, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%s is invalid JSON: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s has trailing JSON tokens", path)
	}
	return nil
}
