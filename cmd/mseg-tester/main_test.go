package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mabels/mseg-tester/internal/bootstrap"
	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/ifaces"
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

func TestEffectiveDelayUsesWaitDelayOnReportSegmentWhenWaitConfigured(t *testing.T) {
	cfg := config.Config{
		RebootDelay: "10s",
		Report:      &config.Report{Wait: &config.Wait{On: "129", WaitDelay: "10m"}},
	}
	got := effectiveDelay(cfg, true /* isReportSegment */, "129")
	if got != 10*time.Minute {
		t.Errorf("effectiveDelay = %v, want waitDelay (10m)", got)
	}
}

func TestEffectiveDelayUsesRebootDelayWhenNotReportSegment(t *testing.T) {
	cfg := config.Config{
		RebootDelay: "10s",
		Report:      &config.Report{Wait: &config.Wait{On: "129", WaitDelay: "10m"}},
	}
	got := effectiveDelay(cfg, false /* isReportSegment */, "128")
	if got != 10*time.Second {
		t.Errorf("effectiveDelay = %v, want plain rebootDelay (10s) on a non-report segment", got)
	}
}

func TestEffectiveDelayUsesRebootDelayWhenWaitIsNil(t *testing.T) {
	cfg := config.Config{RebootDelay: "10s", Report: &config.Report{URL: "https://example.com"}}
	got := effectiveDelay(cfg, true, "129")
	if got != 10*time.Second {
		t.Errorf("effectiveDelay = %v, want plain rebootDelay when Report.Wait is nil", got)
	}
}

func TestEffectiveDelayUsesRebootDelayWhenWaitOnNamesADifferentSegment(t *testing.T) {
	cfg := config.Config{
		RebootDelay: "10s",
		Report:      &config.Report{Wait: &config.Wait{On: "130", WaitDelay: "10m"}}, // not "129"
	}
	got := effectiveDelay(cfg, true, "129")
	if got != 10*time.Second {
		t.Errorf("effectiveDelay = %v, want plain rebootDelay when Wait.On doesn't match this segment", got)
	}
}

func TestEffectiveDelayUsesRebootDelayWhenReportIsNil(t *testing.T) {
	cfg := config.Config{RebootDelay: "10s"}
	got := effectiveDelay(cfg, true, "129")
	if got != 10*time.Second {
		t.Errorf("effectiveDelay = %v, want plain rebootDelay when Report isn't configured at all", got)
	}
}

func TestMatchActiveSegmentFindsNativeSegmentWithAddress(t *testing.T) {
	boot := testBootstrap(t)
	boot.TrunkInterface = "ens18"
	cfg := config.Config{Segments: []config.Segment{
		{Name: "128", Type: "native"},
		{Name: "129", Type: "vlan"},
	}}
	list := []ifaces.Iface{
		{Name: "ens18", Up: true, Addrs: []ifaces.Addr{{IP: mustParseIP(t, "192.168.128.5")}}},
	}
	got, err := matchActiveSegment(list, boot, cfg, false)
	if err != nil {
		t.Fatalf("matchActiveSegment: %v", err)
	}
	if got != "128" {
		t.Errorf("matchActiveSegment = %q, want \"128\" (the native segment riding the bare trunk)", got)
	}
}

func TestMatchActiveSegmentFindsTaggedVLANSegmentWithAddress(t *testing.T) {
	boot := testBootstrap(t)
	boot.TrunkInterface = "ens18"
	cfg := config.Config{Segments: []config.Segment{
		{Name: "128", Type: "native"},
		{Name: "129", Type: "vlan"},
	}}
	list := []ifaces.Iface{
		{Name: "ens18", Up: true}, // bare trunk, no address -- optional: true, as netplan.Render writes it
		{Name: "ens18.129", Parent: "ens18", Up: true, Addrs: []ifaces.Addr{{IP: mustParseIP(t, "192.168.129.56")}}},
	}
	got, err := matchActiveSegment(list, boot, cfg, false)
	if err != nil {
		t.Fatalf("matchActiveSegment: %v", err)
	}
	if got != "129" {
		t.Errorf("matchActiveSegment = %q, want \"129\"", got)
	}
}

func TestMatchActiveSegmentIgnoresDownInterfaces(t *testing.T) {
	boot := testBootstrap(t)
	boot.TrunkInterface = "ens18"
	cfg := config.Config{Segments: []config.Segment{{Name: "128", Type: "native"}}}
	list := []ifaces.Iface{
		{Name: "ens18", Up: false, Addrs: []ifaces.Addr{{IP: mustParseIP(t, "192.168.128.5")}}},
	}
	if _, err := matchActiveSegment(list, boot, cfg, false); err == nil {
		t.Error("expected an error -- the only matching interface isn't UP")
	}
}

func TestMatchActiveSegmentIgnoresLinkLocalOnlyAddresses(t *testing.T) {
	boot := testBootstrap(t)
	boot.TrunkInterface = "ens18"
	cfg := config.Config{Segments: []config.Segment{{Name: "128", Type: "native"}}}
	list := []ifaces.Iface{
		{Name: "ens18", Up: true, Addrs: []ifaces.Addr{{IP: mustParseIP(t, "fe80::1")}}},
	}
	if _, err := matchActiveSegment(list, boot, cfg, false); err == nil {
		t.Error("expected an error -- a link-local-only address isn't a real, routable one")
	}
}

func TestMatchActiveSegmentErrorsWhenNothingMatches(t *testing.T) {
	boot := testBootstrap(t)
	boot.TrunkInterface = "ens18"
	cfg := config.Config{Segments: []config.Segment{
		{Name: "128", Type: "native"},
		{Name: "129", Type: "vlan"},
	}}
	list := []ifaces.Iface{{Name: "lo", Up: true}}
	if _, err := matchActiveSegment(list, boot, cfg, false); err == nil {
		t.Error("expected an error when no segment's expected interface has a usable address")
	}
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("net.ParseIP(%q) failed", s)
	}
	return ip
}
