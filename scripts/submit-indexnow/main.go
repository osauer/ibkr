package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	sitemapPath = flag.String("sitemap", "docs/sitemap.xml", "XML sitemap to submit")
	keyFile     = flag.String("key-file", "docs/indexnow.txt", "IndexNow key file")
	keyLocation = flag.String("key-location", "https://osauer.dev/ibkr/indexnow.txt", "public IndexNow key URL")
	endpoint    = flag.String("endpoint", "https://api.indexnow.org/indexnow", "IndexNow POST endpoint")
	host        = flag.String("host", "osauer.dev", "host that owns the submitted URLs")
	dryRun      = flag.Bool("dry-run", false, "print payload without submitting")
)

type urlset struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
}

type payload struct {
	Host        string   `json:"host"`
	Key         string   `json:"key"`
	KeyLocation string   `json:"keyLocation"`
	URLList     []string `json:"urlList"`
}

func main() {
	flag.Parse()
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "submit-indexnow: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	keyData, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	key := strings.TrimSpace(string(keyData))
	if key == "" {
		return fmt.Errorf("%s is empty", *keyFile)
	}

	urls, err := sitemapURLs(*sitemapPath)
	if err != nil {
		return err
	}
	if len(urls) == 0 {
		return fmt.Errorf("%s has no URLs", *sitemapPath)
	}

	body, err := json.MarshalIndent(payload{
		Host:        *host,
		Key:         key,
		KeyLocation: *keyLocation,
		URLList:     urls,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	if *dryRun {
		fmt.Printf("%s\n", body)
		return nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, *endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected status %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	fmt.Printf("indexnow: submitted %d URLs to %s (%s)\n", len(urls), *endpoint, resp.Status)
	return nil
}

func sitemapURLs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sitemap: %w", err)
	}
	var parsed urlset
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse sitemap: %w", err)
	}
	urls := make([]string, 0, len(parsed.URLs))
	for _, entry := range parsed.URLs {
		loc := strings.TrimSpace(entry.Loc)
		if strings.HasPrefix(loc, "https://osauer.dev/ibkr/") {
			urls = append(urls, loc)
		}
	}
	return urls, nil
}
