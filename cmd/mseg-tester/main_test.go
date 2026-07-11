package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/bootstrap"
	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/state"
)

func testBootstrap(t *testing.T) bootstrap.Bootstrap {
	t.Helper()
	return bootstrap.Bootstrap{
		TrunkInterface:  "ens18",
		UpdateSegment:   "129",
		SoftwareRepo:    "mabels/mseg-tester",
		StateDir:        t.TempDir(),
		ConfigLocalPath: "config.yaml", // unused by advanceToResolvable directly except in error messages
	}
}

// autoWifiSegment has no ifname/mac/pciVendor/pciDevice set at all --
// "auto" resolution (internal/ifdiscover.Find's default case) always
// fails on a machine with no real /sys/class/net wifi hardware, which
// is every test-runner this suite runs on (Linux CI with no radio, or a
// non-Linux dev machine where /sys/class/net doesn't even exist) --
// exactly the same failure mode as a passed-through radio whose driver
// never probed successfully this boot.
func autoWifiSegment(name string) config.Segment {
	return config.Segment{Name: name, Type: "wifi", SSID: "test", PSK: "test"}
}

func vlanSegment(name string) config.Segment {
	return config.Segment{Name: name, Type: "vlan"}
}

func TestAdvanceToResolvableSkipsUnresolvableWifiSegment(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Segments: []config.Segment{
		vlanSegment("128"),
		autoWifiSegment("wifi-128"),
		vlanSegment("129"),
	}}
	active := state.Active{Segment: "128", Cycle: []string{"128", "wifi-128", "129"}}

	next, nextSeg, err := advanceToResolvable(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("advanceToResolvable: unexpected error: %v", err)
	}
	if next != "129" {
		t.Fatalf("expected to skip wifi-128 and land on 129, got %q", next)
	}
	if nextSeg.Name != "129" {
		t.Fatalf("returned segment name = %q, want 129", nextSeg.Name)
	}

	// The skipped segment must still have a recorded (failing) result --
	// see advanceToResolvable's doc comment on why this matters (report
	// visibility even though wifi-128 never actually got booted into).
	b, err := os.ReadFile(filepath.Join(boot.StateDir, "wifi-128.result.yaml"))
	if err != nil {
		t.Fatalf("expected wifi-128.result.yaml to be written: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("wifi-128.result.yaml is empty")
	}
}

func TestAdvanceToResolvableNoSkipWhenNextIsResolvable(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Segments: []config.Segment{
		vlanSegment("128"),
		vlanSegment("129"),
	}}
	active := state.Active{Segment: "128", Cycle: []string{"128", "129"}}

	next, _, err := advanceToResolvable(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("advanceToResolvable: unexpected error: %v", err)
	}
	if next != "129" {
		t.Fatalf("next = %q, want 129 (no skip expected)", next)
	}

	if _, err := os.Stat(filepath.Join(boot.StateDir, "129.result.yaml")); !os.IsNotExist(err) {
		t.Fatalf("129.result.yaml should NOT exist -- it wasn't skipped")
	}
}

func TestAdvanceToResolvableErrorsWhenEveryDeviceUnresolvable(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Segments: []config.Segment{
		autoWifiSegment("wifi-128"),
		autoWifiSegment("wifi-130"),
	}}
	active := state.Active{Segment: "wifi-128", Cycle: []string{"wifi-128", "wifi-130"}}

	_, _, err := advanceToResolvable(cfg, boot, active, false)
	if err == nil {
		t.Fatalf("expected an error when every segment in the cycle fails to resolve")
	}
}

func TestAdvanceToResolvableUnknownSegmentIsFatal(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Segments: []config.Segment{
		vlanSegment("128"),
	}}
	// Cycle names a segment config.yaml doesn't actually declare --
	// active.yaml/config.yaml disagreeing, a real config error, not
	// something to skip past silently.
	active := state.Active{Segment: "128", Cycle: []string{"128", "129"}}

	_, _, err := advanceToResolvable(cfg, boot, active, false)
	if err == nil {
		t.Fatalf("expected an error for a cycle entry not declared in config.yaml")
	}
}

func TestUpdateSegmentThrottledFalseWhenReportIsNil(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{}
	active := state.Active{Segment: boot.UpdateSegment}

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false when Report (and therefore Report.Wait) is nil")
	}
}

func TestUpdateSegmentThrottledFalseWhenReportWaitIsNil(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{URL: "https://example.com/report"}}
	active := state.Active{Segment: boot.UpdateSegment}

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false when Report is set but Report.Wait is nil")
	}
}

func TestUpdateSegmentThrottledFalseWhenNotUpdateSegment(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{Wait: &config.Wait{On: boot.UpdateSegment, WaitDelay: "10m"}}}
	active := state.Active{Segment: "128"} // not boot.UpdateSegment

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false when the active segment isn't boot.UpdateSegment")
	}
}

func TestUpdateSegmentThrottledFalseWhenWaitOnNamesADifferentSegment(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{Wait: &config.Wait{On: "130", WaitDelay: "10m"}}} // not boot.UpdateSegment ("129")
	active := state.Active{Segment: boot.UpdateSegment}

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false when Wait.On names a different segment")
	}
}

func TestUpdateSegmentThrottledFalseWhenNoPriorLastWait(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{Wait: &config.Wait{On: boot.UpdateSegment, WaitDelay: "10m"}}}
	active := state.Active{Segment: boot.UpdateSegment}

	// No state.SaveLastWait call at all -- first time this segment's
	// throttled work would ever run.
	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false when the throttled work has never run on this segment before")
	}
}

func TestUpdateSegmentThrottledTrueWithinWaitDelay(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{Wait: &config.Wait{On: boot.UpdateSegment, WaitDelay: "10m"}}}
	active := state.Active{Segment: boot.UpdateSegment}

	if err := state.SaveLastWait(boot.StateDir, state.LastWait{Segment: boot.UpdateSegment, Ran: time.Now().Add(-1 * time.Minute)}); err != nil {
		t.Fatalf("SaveLastWait: %v", err)
	}

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if !throttled {
		t.Error("expected true when less than waitDelay has elapsed since the last run")
	}
}

func TestUpdateSegmentThrottledFalseAfterWaitDelayElapses(t *testing.T) {
	boot := testBootstrap(t)
	cfg := config.Config{Report: &config.Report{Wait: &config.Wait{On: boot.UpdateSegment, WaitDelay: "10m"}}}
	active := state.Active{Segment: boot.UpdateSegment}

	if err := state.SaveLastWait(boot.StateDir, state.LastWait{Segment: boot.UpdateSegment, Ran: time.Now().Add(-11 * time.Minute)}); err != nil {
		t.Fatalf("SaveLastWait: %v", err)
	}

	throttled, err := updateSegmentThrottled(cfg, boot, active, false)
	if err != nil {
		t.Fatalf("updateSegmentThrottled: %v", err)
	}
	if throttled {
		t.Error("expected false once waitDelay has elapsed since the last run")
	}
}
