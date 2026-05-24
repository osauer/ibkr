package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadFramesSinceOffsetAtLineBoundaryKeepsNextFrame(t *testing.T) {
	path, firstLineLen := writeWireAssertTestLog(t)

	got, err := readFrames(path, int64(firstLineLen))
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1", len(got))
	}
	if got[0].Seq != 2 {
		t.Fatalf("got seq=%d, want 2", got[0].Seq)
	}
}

func TestReadFramesSinceOffsetInsideLineDropsPartial(t *testing.T) {
	path, firstLineLen := writeWireAssertTestLog(t)

	got, err := readFrames(path, int64(firstLineLen/2))
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1", len(got))
	}
	if got[0].Seq != 2 {
		t.Fatalf("got seq=%d, want 2", got[0].Seq)
	}
}

func writeWireAssertTestLog(t *testing.T) (string, int) {
	t.Helper()

	frames := []WireFrame{
		{Seq: 1, When: time.Unix(1, 0), Direction: "OUT", MsgID: 62, MsgName: "reqAccountSummary", Fields: []string{"62", "1"}},
		{Seq: 2, When: time.Unix(2, 0), Direction: "IN", MsgID: 63, MsgName: "accountSummary", Fields: []string{"63", "1"}},
	}
	var content []byte
	var firstLineLen int
	for i, frame := range frames {
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		line = append(line, '\n')
		if i == 0 {
			firstLineLen = len(line)
		}
		content = append(content, line...)
	}

	path := filepath.Join(t.TempDir(), "wire.jsonl")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path, firstLineLen
}
