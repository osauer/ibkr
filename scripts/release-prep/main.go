package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	releaseVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$`)
	versionFieldRE        = regexp.MustCompile(`(?m)^(\s*"version"\s*:\s*")[^"]+(")`)
	softwareVersionRE     = regexp.MustCompile(`(?m)^(\s*"softwareVersion"\s*:\s*")[^"]+(")`)
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "release-prep: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: go run ./scripts/release-prep vX.Y.Z")
	}
	releaseVersion := args[0]
	if !releaseVersionPattern.MatchString(releaseVersion) {
		return fmt.Errorf("RELEASE_VERSION must look like vX.Y.Z (got %q)", releaseVersion)
	}
	version := strings.TrimPrefix(releaseVersion, "v")

	var changed []string
	for _, path := range []string{
		".claude-plugin/plugin.json",
		"server.json",
		"docs/mcp-server.json",
		"docs/.well-known/mcp/server.json",
	} {
		fileChanged, err := replaceExactlyOne(path, versionFieldRE, version)
		if err != nil {
			return err
		}
		if fileChanged {
			changed = append(changed, path)
		}
	}

	for _, path := range []string{
		"docs/index.html",
		"docs/interactive-brokers-mcp-server/index.html",
	} {
		fileChanged, err := replaceExactlyOne(path, softwareVersionRE, version)
		if err != nil {
			return err
		}
		if fileChanged {
			changed = append(changed, path)
		}
	}

	today := berlinDate()
	for _, url := range []string{
		"https://osauer.dev/ibkr/",
		"https://osauer.dev/ibkr/ibkr-mcp/",
		"https://osauer.dev/ibkr/interactive-brokers-mcp-server/",
		"https://osauer.dev/ibkr/tws-mcp-server/",
		"https://osauer.dev/ibkr/ib-gateway-mcp/",
		"https://osauer.dev/ibkr/claude-desktop-interactive-brokers/",
		"https://osauer.dev/ibkr/ibkr-claude-desktop-mcp/",
		"https://osauer.dev/ibkr/connect-claude-to-ibkr/",
		"https://osauer.dev/ibkr/analyze-interactive-brokers-portfolio-with-ai/",
		"https://osauer.dev/ibkr/read-only-mcp-server/",
	} {
		fileChanged, err := replaceExactlyOne("docs/sitemap.xml", sitemapLastmodRE(url), today)
		if err != nil {
			return err
		}
		if fileChanged {
			changed = appendChangedOnce(changed, "docs/sitemap.xml")
		}
	}

	if len(changed) == 0 {
		fmt.Printf("release-prep: public discovery metadata already at %s\n", version)
		return nil
	}
	fmt.Printf("release-prep: set public discovery metadata to %s\n", version)
	for _, path := range changed {
		fmt.Printf("  %s\n", path)
	}
	return nil
}

func replaceExactlyOne(path string, pattern *regexp.Regexp, value string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	matches := pattern.FindAllStringIndex(string(data), -1)
	if len(matches) != 1 {
		return false, fmt.Errorf("%s: expected exactly one match for %q, found %d", path, pattern.String(), len(matches))
	}

	next := pattern.ReplaceAllString(string(data), "${1}"+value+"${2}")
	if next == string(data) {
		return false, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(next), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func sitemapLastmodRE(url string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)(<loc>` + regexp.QuoteMeta(url) + `</loc>\s*<lastmod>)[^<]+(</lastmod>)`)
}

func appendChangedOnce(paths []string, path string) []string {
	if slices.Contains(paths, path) {
		return paths
	}
	return append(paths, path)
}

func berlinDate() string {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		location = time.Local
	}
	return time.Now().In(location).Format("2006-01-02")
}
