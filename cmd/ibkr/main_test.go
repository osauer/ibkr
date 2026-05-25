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
		{"add stays local", []string{"AAPL", "--add"}, false},
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
