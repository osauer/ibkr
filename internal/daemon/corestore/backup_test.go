package corestore

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupPublishesStandaloneMainFile(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	wantJSON := []byte(`{"ok":true,"source":"backup"}`)
	doc, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
		ScopeKey: "market",
		Kind:     "current",
		JSON:     wantJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHead, err := s.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}

	backupDir := privateTempDir(t)
	backupPath := filepath.Join(backupDir, "daemon-backup.db")
	info, err := s.Backup(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Head != wantHead {
		t.Fatalf("backup head=%+v want %+v", info.Head, wantHead)
	}
	assertSQLiteHeaderVersions(t, backupPath, 1, 1)
	assertNoBackupTempsOrSidecars(t, backupDir, filepath.Base(backupPath))

	if err := os.Chmod(backupDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backupDir, 0o700) })
	if mode, err := os.Stat(backupDir); err != nil {
		t.Fatal(err)
	} else if mode.Mode().Perm()&0o222 != 0 {
		t.Fatalf("backup directory mode=%o, want no write bits", mode.Mode().Perm())
	}

	for range 3 {
		verified, err := VerifyBackup(ctx, backupPath, wantHead)
		if err != nil {
			t.Fatal(err)
		}
		if verified.Head != wantHead || !verified.Integrity.OK() {
			t.Fatalf("verified backup=%+v", verified)
		}
		assertNoBackupTempsOrSidecars(t, backupDir, filepath.Base(backupPath))
	}

	db, err := sql.Open("sqlite", sqliteDSN(backupPath, defaultBusyTimeout, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var (
		gotRevision int64
		gotJSON     []byte
	)
	queryErr := db.QueryRowContext(ctx, `
SELECT revision, document_json
FROM state_documents
WHERE scope_key = ? AND kind = ?`, "market", "current").Scan(&gotRevision, &gotJSON)
	closeErr := db.Close()
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if gotRevision != doc.Revision || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("backup content revision=%d json=%s", gotRevision, gotJSON)
	}
	assertNoBackupTempsOrSidecars(t, backupDir, filepath.Base(backupPath))
}

func assertSQLiteHeaderVersions(t *testing.T, path string, wantWrite, wantRead byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 20)
	_, readErr := io.ReadFull(f, header)
	closeErr := f.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		t.Fatalf("invalid SQLite header %q", header[:16])
	}
	if header[18] != wantWrite || header[19] != wantRead {
		t.Fatalf("SQLite header write/read versions=%d/%d want %d/%d", header[18], header[19], wantWrite, wantRead)
	}
}

func assertNoBackupTempsOrSidecars(t *testing.T, dir, backupBase string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == backupBase+"-wal" || name == backupBase+"-shm" || strings.HasPrefix(name, "."+backupBase+".tmp-") {
			t.Fatalf("unexpected backup temporary artifact or sidecar %q", name)
		}
	}
}
