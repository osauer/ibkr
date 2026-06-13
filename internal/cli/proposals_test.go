package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunProposalsGroupHelp(t *testing.T) {
	t.Parallel()
	for _, help := range []string{"--help", "-h", "-help", "help"} {
		t.Run(help, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "proposals", []string{help}); code != 0 {
				t.Fatalf("Run(proposals, %s)=%d, want 0", help, code)
			}
			got := stdout.String()
			for _, want := range []string{
				"ibkr proposals",
				"Daemon-owned close/reduce-only protection proposals",
				"status|refresh|list|preview|submit|ignore",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("proposals help missing %q:\n%s", want, got)
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}
