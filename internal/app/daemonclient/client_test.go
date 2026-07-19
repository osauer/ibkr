package daemonclient

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

var _ func(Real, context.Context, rpc.NudgesCutoverReviewParams) (*rpc.NudgesCutoverReviewResult, error) = Real.NudgesCutoverReview

func TestNudgesCutoverReviewHasFixedTypedSignature(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	want := rpc.NudgesCutoverReviewResult{
		OK: true, ReviewedAt: reviewedAt, CoverageFrom: reviewedAt.Add(-time.Hour),
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	params := rpc.NudgesCutoverReviewParams{
		Origin:   rpc.NudgeCutoverReviewOriginPairedDevice,
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	got, err := nudgesCutoverReview(context.Background(), params, func(_ context.Context, method string, rawParams, rawOut any) error {
		if method != rpc.MethodNudgesCutoverReview {
			t.Fatalf("method=%q", method)
		}
		typedParams, ok := rawParams.(rpc.NudgesCutoverReviewParams)
		if !ok || typedParams != params {
			t.Fatalf("params=%T %+v", rawParams, rawParams)
		}
		typedOut, ok := rawOut.(*rpc.NudgesCutoverReviewResult)
		if !ok {
			t.Fatalf("result destination=%T", rawOut)
		}
		*typedOut = want
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("result=%+v want=%+v", got, want)
	}
}
