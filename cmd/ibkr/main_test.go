package main

import "testing"

func TestIsWatchDaemonInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"list stays local", []string{"--list"}, false},
		{"list json stays local", []string{"--list", "--json"}, false},
		{"add stays local", []string{"AAPL", "--add"}, false},
		{"default watch needs daemon", nil, true},
		{"json default needs daemon", []string{"--json"}, true},
		{"timeout default needs daemon", []string{"--timeout", "2s"}, true},
		{"quotes needs daemon", []string{"--quotes"}, true},
		{"quotes true needs daemon", []string{"--quotes=true"}, true},
		{"watch needs daemon", []string{"--watch"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWatchDaemonInvocation(tc.args); got != tc.want {
				t.Fatalf("isWatchDaemonInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestIsBacktestDaemonInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"offline canary stays local", []string{"canary", "--input", "rows.jsonl"}, false},
		{"capture opportunity needs daemon", []string{"capture-opportunity", "--symbols", "SPY"}, true},
		{"export opportunity bars needs daemon", []string{"export-opportunity-bars", "--symbols", "SPY"}, true},
		{"subcommand help stays local", []string{"export-opportunity-bars", "--help"}, false},
		{"top level help stays local", []string{"--help"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBacktestDaemonInvocation(tc.args); got != tc.want {
				t.Fatalf("isBacktestDaemonInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
