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
