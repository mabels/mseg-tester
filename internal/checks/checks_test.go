package checks

import (
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/state"
)

// fakeRunOnce swaps runOnceFn for the duration of the calling test and
// restores it afterward -- the same seam internal/selfupdate's tests use
// for goInstall, so Run's batch-retry loop can be tested without a real
// interface, DNS server, or network reachability.
func fakeRunOnce(t *testing.T, fn func(calls int) []state.CheckResult) *int {
	t.Helper()
	orig := runOnceFn
	calls := 0
	runOnceFn = func(seg config.Segment, verbose bool) []state.CheckResult {
		calls++
		return fn(calls)
	}
	t.Cleanup(func() { runOnceFn = orig })
	return &calls
}

func TestRunStopsAfterFirstFullyPassingAttempt(t *testing.T) {
	calls := fakeRunOnce(t, func(int) []state.CheckResult {
		return []state.CheckResult{{Name: "dhcp", Pass: true}, {Name: "dns-default-local", Pass: true}}
	})
	results := Run(config.Segment{Name: "129"}, 3, time.Millisecond, false)
	if *calls != 1 {
		t.Errorf("expected exactly 1 attempt when the first fully passes, got %d", *calls)
	}
	if !allPass(results) {
		t.Errorf("expected all-passing results, got %+v", results)
	}
}

func TestRunRetriesTheWholeBatchOnAnyFailure(t *testing.T) {
	calls := fakeRunOnce(t, func(call int) []state.CheckResult {
		if call < 3 {
			// dhcp passes but dns fails -- the WHOLE batch should be
			// re-run, not just dns.
			return []state.CheckResult{{Name: "dhcp", Pass: true}, {Name: "dns-default-local", Pass: false}}
		}
		return []state.CheckResult{{Name: "dhcp", Pass: true}, {Name: "dns-default-local", Pass: true}}
	})
	results := Run(config.Segment{Name: "129"}, 5, time.Millisecond, false)
	if *calls != 3 {
		t.Errorf("expected exactly 3 attempts (stop as soon as a whole batch passes), got %d", *calls)
	}
	if !allPass(results) {
		t.Errorf("expected the final (passing) attempt's results, got %+v", results)
	}
}

func TestRunGivesUpAfterExhaustingAttempts(t *testing.T) {
	calls := fakeRunOnce(t, func(int) []state.CheckResult {
		return []state.CheckResult{{Name: "dhcp", Pass: false, Detail: "no address"}}
	})
	results := Run(config.Segment{Name: "129"}, 3, time.Millisecond, false)
	if *calls != 3 {
		t.Errorf("expected exactly 3 attempts (all exhausted), got %d", *calls)
	}
	if allPass(results) {
		t.Errorf("expected the still-failing last attempt's results, got %+v", results)
	}
}

func TestAllPass(t *testing.T) {
	cases := []struct {
		name    string
		results []state.CheckResult
		want    bool
	}{
		{"empty is vacuously true", nil, true},
		{"all pass", []state.CheckResult{{Pass: true}, {Pass: true}}, true},
		{"one fails", []state.CheckResult{{Pass: true}, {Pass: false}}, false},
	}
	for _, c := range cases {
		if got := allPass(c.results); got != c.want {
			t.Errorf("%s: allPass() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDNSChecksNilConfigReturnsNothing(t *testing.T) {
	if got := dnsChecks(config.Segment{}); got != nil {
		t.Errorf("expected nil for a segment with no DNSCheck configured, got %+v", got)
	}
}

func TestDNSChecksLocalDefaultsToBothFamilies(t *testing.T) {
	seg := config.Segment{
		DNSServer:  "127.0.0.1",
		DNSServer6: "::1",
		DNSCheck: &config.DNSCheck{
			Local: &config.DNSCheckGroup{Tests: []string{"a.example.", "b.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 4 { // 2 tests x (ipv4 + ipv6)
		t.Fatalf("expected 4 results (2 tests x ipv4+ipv6), got %d: %+v", len(results), results)
	}
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{
		"dns-local-ipv4:a.example.", "dns-local-ipv4:b.example.",
		"dns-local-ipv6:a.example.", "dns-local-ipv6:b.example.",
	} {
		if !names[want] {
			t.Errorf("expected a result named %q, got names %v", want, names)
		}
	}
}

func TestDNSChecksLocalSkipsIPv6WhenDNSServer6Empty(t *testing.T) {
	seg := config.Segment{
		DNSServer: "127.0.0.1",
		DNSCheck: &config.DNSCheck{
			Local: &config.DNSCheckGroup{Tests: []string{"a.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (ipv4 only, no DNSServer6 configured), got %d: %+v", len(results), results)
	}
	if results[0].Name != "dns-local-ipv4:a.example." {
		t.Errorf("Name = %q, want \"dns-local-ipv4:a.example.\"", results[0].Name)
	}
}

func TestDNSChecksLocalServerOverrideSkipsAutomaticDualStack(t *testing.T) {
	seg := config.Segment{
		DNSServer:  "127.0.0.1",
		DNSServer6: "::1",
		DNSCheck: &config.DNSCheck{
			Local: &config.DNSCheckGroup{Server: "127.0.0.1", Tests: []string{"a.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result when Server overrides the group, got %d: %+v", len(results), results)
	}
	if results[0].Name != "dns-local:a.example." {
		t.Errorf("Name = %q, want \"dns-local:a.example.\"", results[0].Name)
	}
}

func TestDNSChecksRemoteGroup(t *testing.T) {
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Remote: &config.DNSCheckGroup{Tests: []string{"a.example.", "b.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if r.Name != "dns-remote:a.example." && r.Name != "dns-remote:b.example." {
			t.Errorf("unexpected result name %q", r.Name)
		}
	}
}

func TestDNSChecksRemoteServerOverride(t *testing.T) {
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Remote: &config.DNSCheckGroup{Server: "127.0.0.1", Tests: []string{"a.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Name != "dns-remote:a.example." {
		t.Errorf("Name = %q, want \"dns-remote:a.example.\"", results[0].Name)
	}
}
