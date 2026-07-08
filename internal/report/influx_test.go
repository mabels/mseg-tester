package report

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/state"
)

func TestLineProtocolBasicShape(t *testing.T) {
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	results := []state.Result{
		{
			Segment:   "129",
			Timestamp: ts,
			Version:   "abc123",
			Updated:   true,
			Checks: []state.CheckResult{
				{Name: "dhcp", Pass: true, Detail: "192.168.129.56 (gw 192.168.129.1)"},
				{Name: "dns-local-ipv4:mam-hh-dmz.adviser.com.", Pass: false, Detail: "resolving: timeout"},
			},
		},
	}
	out := lineProtocol(results)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 { // 1 result summary + 2 checks
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), out)
	}

	wantTS := ts.UnixNano()
	if !strings.HasPrefix(lines[0], "mseg_tester_result,segment=129 ") {
		t.Errorf("line 0 measurement/tags wrong: %q", lines[0])
	}
	if !strings.Contains(lines[0], "pass=false") {
		t.Errorf("expected overall pass=false (one check failed), got %q", lines[0])
	}
	if !strings.Contains(lines[0], "updated=true") {
		t.Errorf("expected updated=true, got %q", lines[0])
	}
	if !strings.Contains(lines[0], `version="abc123"`) {
		t.Errorf("expected version field, got %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], " "+strconv.FormatInt(wantTS, 10)) {
		t.Errorf("expected trailing timestamp %d, got %q", wantTS, lines[0])
	}

	if !strings.HasPrefix(lines[1], "mseg_tester_check,segment=129,check=dhcp ") {
		t.Errorf("line 1 measurement/tags wrong: %q", lines[1])
	}
	if !strings.Contains(lines[1], `detail="192.168.129.56 (gw 192.168.129.1)"`) {
		t.Errorf("expected detail field, got %q", lines[1])
	}

	if !strings.Contains(lines[2], "check=dns-local-ipv4:mam-hh-dmz.adviser.com.") {
		t.Errorf("expected check name tag with colons/dots intact, got %q", lines[2])
	}
	if !strings.Contains(lines[2], "pass=false") {
		t.Errorf("expected pass=false, got %q", lines[2])
	}
}

func TestEscapeTagEscapesCommaEqualsSpace(t *testing.T) {
	got := escapeTag("a,b=c d")
	want := `a\,b\=c\ d`
	if got != want {
		t.Errorf("escapeTag() = %q, want %q", got, want)
	}
}

func TestEscapeFieldStringEscapesBackslashAndQuote(t *testing.T) {
	got := escapeFieldString(`say "hi"\now`)
	want := `say \"hi\"\\now`
	if got != want {
		t.Errorf("escapeFieldString() = %q, want %q", got, want)
	}
}

func TestEscapeFieldStringDoesNotMangleNewlines(t *testing.T) {
	// Regression guard for the %q pitfall noted in influx.go: a raw
	// newline should pass through untouched (not become the two
	// characters backslash+n, which line protocol would NOT interpret
	// as an escape).
	got := escapeFieldString("line1\nline2")
	if got != "line1\nline2" {
		t.Errorf("escapeFieldString() unexpectedly altered a raw newline: %q", got)
	}
}
