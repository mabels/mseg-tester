package checks

import (
	"strings"
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/state"
)

func TestWithRetryPassesOnFirstAttemptNoRetry(t *testing.T) {
	calls := 0
	res := withRetry(3, time.Millisecond, func() state.CheckResult {
		calls++
		return state.CheckResult{Name: "x", Pass: true, Detail: "ok"}
	})
	if !res.Pass {
		t.Fatalf("expected Pass=true, got %+v", res)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call when the first attempt passes, got %d", calls)
	}
}

func TestWithRetryStopsAsSoonAsOnePasses(t *testing.T) {
	calls := 0
	res := withRetry(5, time.Millisecond, func() state.CheckResult {
		calls++
		if calls < 3 {
			return state.CheckResult{Name: "x", Pass: false, Detail: "not yet"}
		}
		return state.CheckResult{Name: "x", Pass: true, Detail: "ok"}
	})
	if !res.Pass {
		t.Fatalf("expected Pass=true, got %+v", res)
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 calls (stop as soon as it passes), got %d", calls)
	}
}

func TestWithRetryFailsAfterExhaustingAttempts(t *testing.T) {
	calls := 0
	res := withRetry(3, time.Millisecond, func() state.CheckResult {
		calls++
		return state.CheckResult{Name: "x", Pass: false, Detail: "nope"}
	})
	if res.Pass {
		t.Fatalf("expected Pass=false, got %+v", res)
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 calls (all attempts used), got %d", calls)
	}
	if !strings.Contains(res.Detail, "failed all 3 attempts") {
		t.Errorf("expected Detail to note the exhausted attempt count, got %q", res.Detail)
	}
}

func TestWithRetrySingleAttemptDoesNotAnnotateDetail(t *testing.T) {
	res := withRetry(1, time.Millisecond, func() state.CheckResult {
		return state.CheckResult{Name: "x", Pass: false, Detail: "nope"}
	})
	if res.Detail != "nope" {
		t.Errorf("expected Detail unchanged with attempts=1, got %q", res.Detail)
	}
}
