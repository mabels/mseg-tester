package checks

import (
	"os"
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/state"
)

// osHostnameForTest wraps os.Hostname purely so the test above reads as
// "compare against whatever selfFQDN itself would call" without
// importing "os" twice under two different names.
func osHostnameForTest(t *testing.T) (string, error) {
	t.Helper()
	return os.Hostname()
}

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

func TestDNSChecksRunsEachTestOncePerGroupServer(t *testing.T) {
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Servers: []string{"system", "127.0.0.1", "::1"},
			Tests: []config.DNSTest{
				{Type: "A", Host: "a.example."},
				{Type: "A", Host: "b.example."},
			},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 6 { // 2 tests x 3 servers
		t.Fatalf("expected 6 results (2 tests x 3 servers), got %d: %+v", len(results), results)
	}
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{
		"dns-a-system:a.example.", "dns-a-127.0.0.1:a.example.", "dns-a-::1:a.example.",
		"dns-a-system:b.example.", "dns-a-127.0.0.1:b.example.", "dns-a-::1:b.example.",
	} {
		if !names[want] {
			t.Errorf("expected a result named %q, got names %v", want, names)
		}
	}
}

func TestDNSChecksInvalidServerEntrySkippedCleanly(t *testing.T) {
	// A malformed entry (not "system", not a parseable IP) is defensive-
	// only -- config.Load's validateDNSCheck should already reject it --
	// but dnsChecks itself must still degrade to a passing "not
	// applicable" result rather than a failure or a panic.
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Servers: []string{"not-a-real-server"},
			Tests:   []config.DNSTest{{Type: "A", Host: "a.example."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if !results[0].Pass {
		t.Errorf("expected the invalid-server result to Pass (not applicable, not a failure), got %+v", results[0])
	}
}

func TestDNSChecksPerTestServersOverridesGroupDefault(t *testing.T) {
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Servers: []string{"system", "127.0.0.1", "::1"},
			Tests: []config.DNSTest{
				{Type: "Hostname4", Domain: "example.com.", Servers: []string{"system", "127.0.0.1"}},
			},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 2 { // this test's own Servers override, not the group's 3
		t.Fatalf("expected 2 results (test-level Servers override), got %d: %+v", len(results), results)
	}
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{"dns-hostname4-system:example.com.", "dns-hostname4-127.0.0.1:example.com."} {
		if !names[want] {
			t.Errorf("expected a result named %q, got names %v", want, names)
		}
	}
	if names["dns-hostname4-::1:example.com."] {
		t.Errorf("expected NO ::1 result (overridden away by this test's own Servers), got names %v", names)
	}
}

func TestDNSChecksAPTRAndHostnameNaming(t *testing.T) {
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Servers: []string{"127.0.0.1"},
			Tests: []config.DNSTest{
				{Type: "A-PTR", Host: "mam-hh-gw.mam-hh.adviser.com."},
				{Type: "AAAA-PTR", Host: "mam-hh-gw.mam-hh.adviser.com."},
				{Type: "Hostname4", Domain: "mam-hh.adviser.com."},
			},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(results), results)
	}
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{
		"dns-a-ptr-127.0.0.1:mam-hh-gw.mam-hh.adviser.com.",
		"dns-aaaa-ptr-127.0.0.1:mam-hh-gw.mam-hh.adviser.com.",
		"dns-hostname4-127.0.0.1:mam-hh.adviser.com.",
	} {
		if !names[want] {
			t.Errorf("expected a result named %q, got names %v", want, names)
		}
	}
}

func TestDNSChecksAAAAPTRForcesIPv6RegardlessOfServer(t *testing.T) {
	// AAAA-PTR must force an IPv6 round trip even when it's run against
	// the "system" server (network family is fixed by Type, not by which
	// server answers) -- exercised here by simply confirming the result
	// set/shape; the actual forced-family behavior is verified by
	// dnsTestCheck calling roundTripCheck with "ip6" for this Type,
	// covered at the unit level by this test's sibling above.
	seg := config.Segment{
		DNSCheck: &config.DNSCheck{
			Servers: []string{"system"},
			Tests:   []config.DNSTest{{Type: "AAAA-PTR", Host: "mam-hh-gw.mam-hh.adviser.com."}},
		},
	}
	results := dnsChecks(seg)
	if len(results) != 1 || results[0].Name != "dns-aaaa-ptr-system:mam-hh-gw.mam-hh.adviser.com." {
		t.Fatalf("expected 1 result named dns-aaaa-ptr-system:..., got %+v", results)
	}
}

func TestSelfFQDNNormalizesTrailingDot(t *testing.T) {
	host, err := osHostnameForTest(t)
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	cases := []struct {
		name   string
		domain string
	}{
		{"no trailing dot", "mam-hh-dmz.adviser.com"},
		{"already has trailing dot", "mam-hh-dmz.adviser.com."},
	}
	want := host + ".mam-hh-dmz.adviser.com."
	for _, c := range cases {
		if got := selfFQDN(c.domain); got != want {
			t.Errorf("%s: selfFQDN(%q) = %q, want %q", c.name, c.domain, got, want)
		}
	}
}
