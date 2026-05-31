package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

var (
	rootRobotsURL = flag.String("robots", "https://osauer.dev/robots.txt", "root robots.txt URL that governs osauer.dev URLs")
	liveSitemap   = flag.String("live-sitemap", "https://osauer.dev/ibkr/sitemap.xml", "deployed ibkr XML sitemap URL")
	localSitemap  = flag.String("local-sitemap", "docs/sitemap.xml", "local XML sitemap whose URLs must be deployed")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "public-discovery-check: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	client := &http.Client{Timeout: 15 * time.Second}

	var problems []string
	checkRootRobots(client, &problems)
	checkDeployedFile(client, &problems, *liveSitemap, "application/xml")
	checkDeployedFile(client, &problems, "https://osauer.dev/ibkr/llms.txt", "text/plain")
	checkDeployedFile(client, &problems, "https://osauer.dev/ibkr/llms-full.txt", "text/plain")
	checkDeployedFile(client, &problems, "https://osauer.dev/ibkr/.well-known/mcp/server.json", "application/json")
	checkLiveSitemapURLs(client, &problems)

	if len(problems) > 0 {
		return errors.New("\n  - " + strings.Join(problems, "\n  - "))
	}

	fmt.Printf("public-discovery-check: %s advertises %s and deployed discovery files are reachable\n", *rootRobotsURL, *liveSitemap)
	return nil
}

func checkRootRobots(client *http.Client, problems *[]string) {
	body, contentType, err := getText(client, *rootRobotsURL)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		*problems = append(*problems, fmt.Sprintf("%s content-type = %q, want text/plain", *rootRobotsURL, contentType))
	}

	sitemaps := robotsSitemaps(body)
	for _, want := range []string{"https://osauer.dev/sitemap.xml", *liveSitemap} {
		if !slices.Contains(sitemaps, want) {
			*problems = append(*problems, fmt.Sprintf("%s missing Sitemap: %s", *rootRobotsURL, want))
		}
	}
}

func checkDeployedFile(client *http.Client, problems *[]string, url, contentTypePrefix string) {
	resp, err := client.Head(url)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s HEAD failed: %v", url, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		*problems = append(*problems, fmt.Sprintf("%s status = %s, want 200 OK", url, resp.Status))
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, contentTypePrefix) {
		*problems = append(*problems, fmt.Sprintf("%s content-type = %q, want %s", url, contentType, contentTypePrefix))
	}
}

func checkLiveSitemapURLs(client *http.Client, problems *[]string) {
	liveBody, _, err := getText(client, *liveSitemap)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	liveURLs, err := sitemapURLs([]byte(liveBody))
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s is invalid XML: %v", *liveSitemap, err))
		return
	}

	localData, err := os.ReadFile(*localSitemap)
	if err != nil {
		*problems = append(*problems, err.Error())
		return
	}
	localURLs, err := sitemapURLs(localData)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s is invalid XML: %v", *localSitemap, err))
		return
	}

	for _, want := range localURLs {
		if !slices.Contains(liveURLs, want) {
			*problems = append(*problems, fmt.Sprintf("%s missing local sitemap URL %s", *liveSitemap, want))
		}
	}
}

func getText(client *http.Client, url string) (string, string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", "", fmt.Errorf("%s GET failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s status = %s, want 200 OK", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if err != nil {
		return "", "", fmt.Errorf("%s read failed: %w", url, err)
	}
	if len(data) > 512*1024 {
		return "", "", fmt.Errorf("%s is larger than 512 KiB", url)
	}
	return string(data), resp.Header.Get("Content-Type"), nil
}

func robotsSitemaps(body string) []string {
	var out []string
	for line := range strings.Lines(body) {
		field, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(field), "sitemap") {
			continue
		}
		if value, _, ok := strings.Cut(value, "#"); ok {
			out = append(out, strings.TrimSpace(value))
			continue
		}
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func sitemapURLs(data []byte) ([]string, error) {
	var parsed struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(parsed.URLs))
	for _, entry := range parsed.URLs {
		if loc := strings.TrimSpace(entry.Loc); loc != "" {
			out = append(out, loc)
		}
	}
	return out, nil
}
