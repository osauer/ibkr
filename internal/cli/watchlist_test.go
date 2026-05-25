package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/watchlist"
)

func TestRunWatchlistAddListJSONWithoutDaemon(t *testing.T) {
	oldPath := watchlistDefaultPath
	t.Cleanup(func() { watchlistDefaultPath = oldPath })
	path := filepath.Join(t.TempDir(), "watchlist.json")
	watchlistDefaultPath = func() (string, error) { return path, nil }

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "watch", []string{"IBM", "--add", "--json"}); code != 0 {
		t.Fatalf("watch add exit = %d, stderr=%q", code, stderr.String())
	}
	var add watchlist.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &add); err != nil {
		t.Fatalf("decode add JSON: %v\n%s", err, stdout.String())
	}
	if len(add.Symbols) != 1 || add.Symbols[0] != "IBM" {
		t.Fatalf("add snapshot = %+v, want IBM", add)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), env, "watch", []string{"--list", "--json"}); code != 0 {
		t.Fatalf("watch list exit = %d, stderr=%q", code, stderr.String())
	}
	var list watchlist.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, stdout.String())
	}
	if len(list.Symbols) != 1 || list.Symbols[0] != "IBM" {
		t.Fatalf("list snapshot = %+v, want IBM", list)
	}
}

func TestRunWatchlistClearText(t *testing.T) {
	oldPath := watchlistDefaultPath
	t.Cleanup(func() { watchlistDefaultPath = oldPath })
	path := filepath.Join(t.TempDir(), "watchlist.json")
	watchlistDefaultPath = func() (string, error) { return path, nil }

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "watch", []string{"IBM", "--add"}); code != 0 {
		t.Fatalf("watch add exit = %d, stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), env, "watch", []string{"--clear"}); code != 0 {
		t.Fatalf("watch clear exit = %d, stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "No symbols in watchlist") {
		t.Fatalf("clear output missing empty-list message:\n%s", out)
	}
}
