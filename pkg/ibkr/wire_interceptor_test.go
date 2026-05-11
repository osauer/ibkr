package ibkr

import "testing"

func TestWireInterceptorApplyOutboundOverrides(t *testing.T) {
	w := &WireInterceptor{
		enabled:   true,
		autoApply: true,
		overrides: map[int]*messageOverride{
			9: {
				MsgID: 9,
				Operations: []OverrideOperation{{
					Action: "set",
					Index:  1,
					Value:  "10",
				}},
			},
		},
		maxAttempts: 3,
	}

	fields := []string{"9", "8", "1"}
	updated, changed := w.ApplyOutboundOverrides(9, fields)
	if !changed {
		t.Fatalf("expected override to apply")
	}
	if got := updated[1]; got != "10" {
		t.Fatalf("expected index 1 to be 10, got %s", got)
	}
}

func TestWireInterceptorApplyOutboundOverridesAutoApplyDisabled(t *testing.T) {
	w := &WireInterceptor{
		enabled:   true,
		autoApply: false,
		overrides: map[int]*messageOverride{
			9: {
				MsgID: 9,
				Operations: []OverrideOperation{{
					Action: "delete",
					Index:  1,
				}},
			},
		},
		maxAttempts: 3,
	}

	fields := []string{"9", "8", "1"}
	updated, changed := w.ApplyOutboundOverrides(9, fields)
	if changed {
		t.Fatalf("expected override to be ignored when auto-apply disabled")
	}
	if len(updated) != len(fields) {
		t.Fatalf("fields mutated despite auto-apply disabled")
	}
}
