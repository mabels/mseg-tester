package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const minimalYAML = `
segments:
  - name: "128"
    type: native
    routingCheck: "1.1.1.1:443"
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsVarFromEnvVarsMap(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${INFLUX_TOKEN}"
`)
	cfg, err := Load(path, map[string]string{"INFLUX_TOKEN": "secret-value"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report == nil || cfg.Report.Influx == nil {
		t.Fatalf("expected report.influx to be parsed, got %+v", cfg.Report)
	}
	if cfg.Report.Influx.Token != "secret-value" {
		t.Errorf("Token = %q, want the expanded value", cfg.Report.Influx.Token)
	}
}

func TestLoadExpandsVarFromRealEnvWhenMapNil(t *testing.T) {
	t.Setenv("MSEG_TESTER_CONFIG_TEST_TOKEN", "from-real-env")
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${MSEG_TESTER_CONFIG_TEST_TOKEN}"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report.Influx.Token != "from-real-env" {
		t.Errorf("Token = %q, want the real-env fallback value", cfg.Report.Influx.Token)
	}
}

func TestLoadLeavesUnresolvedVarAsLiteralPlaceholder(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${THIS_VAR_IS_NOT_SET_ANYWHERE}"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report.Influx.Token != "${THIS_VAR_IS_NOT_SET_ANYWHERE}" {
		t.Errorf("Token = %q, want the placeholder left untouched", cfg.Report.Influx.Token)
	}
}

func TestLoadWithNoVarReferencesUnaffected(t *testing.T) {
	path := writeTemp(t, minimalYAML)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Segments) != 1 || cfg.Segments[0].Name != "128" {
		t.Errorf("unexpected segments: %+v", cfg.Segments)
	}
}

func TestLoadAcceptsValidWifiSegment(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    ssid: "${WIFI_128_SSID}"
    psk: "${WIFI_128_PSK}"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, map[string]string{
		"WIFI_128_SSID": "MAM-HH",
		"WIFI_128_PSK":  "von Winsen nach Hamburg",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	seg, ok := cfg.BySegmentName("wifi-128")
	if !ok {
		t.Fatalf("expected segment \"wifi-128\" to be parsed")
	}
	if seg.SSID != "MAM-HH" {
		t.Errorf("SSID = %q, want the expanded value", seg.SSID)
	}
	if seg.PSK != "von Winsen nach Hamburg" {
		t.Errorf("PSK = %q, want the expanded value", seg.PSK)
	}
}

func TestLoadAcceptsWifiSegmentWithNoIdentificationAtAll(t *testing.T) {
	// Unlike the pre-ifdiscover design, a "wifi" segment with none of
	// ifname/mac/pciVendor+pciDevice set is now valid -- it just means
	// "auto" (internal/ifdiscover.Find picks the first Wi-Fi-capable
	// interface at runtime). Only ssid/psk stay required.
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	seg, ok := cfg.BySegmentName("wifi-128")
	if !ok {
		t.Fatalf("expected segment \"wifi-128\" to be parsed")
	}
	if seg.IfName != "" || seg.MAC != "" || seg.PCIVendor != "" || seg.PCIDevice != "" {
		t.Errorf("expected every identification field to stay empty, got %+v", seg)
	}
}

func TestLoadAcceptsWifiSegmentIdentifiedByMAC(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    mac: "90:7a:be:dc:34:a9"
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	seg, ok := cfg.BySegmentName("wifi-128")
	if !ok {
		t.Fatalf("expected segment \"wifi-128\" to be parsed")
	}
	if seg.MAC != "90:7a:be:dc:34:a9" {
		t.Errorf("MAC = %q, want the configured value", seg.MAC)
	}
}

func TestLoadAcceptsWifiSegmentIdentifiedByPCI(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    pciVendor: "14c3"
    pciDevice: "0616"
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	seg, ok := cfg.BySegmentName("wifi-128")
	if !ok {
		t.Fatalf("expected segment \"wifi-128\" to be parsed")
	}
	if seg.PCIVendor != "14c3" || seg.PCIDevice != "0616" {
		t.Errorf("PCIVendor/PCIDevice = %q/%q, want \"14c3\"/\"0616\"", seg.PCIVendor, seg.PCIDevice)
	}
}

func TestLoadRejectsPartialPCIPair(t *testing.T) {
	onlyVendor := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    pciVendor: "14c3"
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	if _, err := Load(onlyVendor, nil); err == nil {
		t.Fatal("expected an error when only pciVendor is set")
	}

	onlyDevice := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    pciDevice: "0616"
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	if _, err := Load(onlyDevice, nil); err == nil {
		t.Fatal("expected an error when only pciDevice is set")
	}
}

func TestLoadRejectsWifiSegmentMissingSSID(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected an error for a wifi segment missing ssid")
	}
}

func TestLoadRejectsWifiSegmentMissingPSK(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    ssid: "MAM-HH"
    routingCheck: "1.1.1.1:443"
`)
	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected an error for a wifi segment missing psk")
	}
}

func TestVLANSegmentNamesExcludesWifi(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "129"
    type: vlan
    routingCheck: "1.1.1.1:443"
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.VLANSegmentNames()
	want := []string{"128", "129"}
	if len(got) != len(want) {
		t.Fatalf("VLANSegmentNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("VLANSegmentNames = %v, want %v", got, want)
			break
		}
	}
}

func TestCycleNamesIncludesWifi(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.CycleNames()
	want := []string{"128", "wifi-128"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("CycleNames = %v, want %v", got, want)
	}
}

func TestWifiPCIDevicesDedupsSharedRadio(t *testing.T) {
	// Mirrors this project's own examples/config.yaml: wifi-128/130/131
	// all identify the SAME physical card -- should collapse to one
	// entry, not three.
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    pciVendor: "14c3"
    pciDevice: "0616"
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
  - name: "wifi-130"
    type: wifi
    pciVendor: "14c3"
    pciDevice: "0616"
    ssid: "FOR_130"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.WifiPCIDevices()
	want := []string{"14c3:0616"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("WifiPCIDevices = %v, want %v", got, want)
	}
}

func TestWifiPCIDevicesMultipleDistinctDevicesInOrder(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-130"
    type: wifi
    pciVendor: "14c3"
    pciDevice: "0616"
    ssid: "FOR_130"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
  - name: "wifi-131"
    type: wifi
    pciVendor: "8086"
    pciDevice: "2725"
    ssid: "MAM-HH-US"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.WifiPCIDevices()
	want := []string{"14c3:0616", "8086:2725"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("WifiPCIDevices = %v, want %v", got, want)
	}
}

func TestWifiPCIDevicesSkipsSegmentsIdentifiedByIfnameOrMAC(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "wifi-128"
    type: wifi
    ifname: wlan0
    ssid: "MAM-HH"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
  - name: "wifi-130"
    type: wifi
    mac: "90:7a:be:dc:34:a9"
    ssid: "FOR_130"
    psk: "secret"
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WifiPCIDevices(); len(got) != 0 {
		t.Errorf("WifiPCIDevices = %v, want empty (neither segment declares pciVendor/pciDevice)", got)
	}
}

func TestWifiPCIDevicesEmptyForNonWifiSegments(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
  - name: "129"
    type: vlan
    routingCheck: "1.1.1.1:443"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WifiPCIDevices(); len(got) != 0 {
		t.Errorf("WifiPCIDevices = %v, want empty", got)
	}
}

func TestWaitDelayOrDefaultNilWaitReturnsTenMinutes(t *testing.T) {
	var w *Wait
	if got := w.WaitDelayOrDefault(); got != 10*time.Minute {
		t.Errorf("WaitDelayOrDefault() on a nil *Wait = %v, want 10m", got)
	}
}

func TestWaitDelayOrDefaultEmptyReturnsTenMinutes(t *testing.T) {
	w := &Wait{On: "129"}
	if got := w.WaitDelayOrDefault(); got != 10*time.Minute {
		t.Errorf("WaitDelayOrDefault() with empty WaitDelay = %v, want 10m", got)
	}
}

func TestWaitDelayOrDefaultMalformedReturnsTenMinutes(t *testing.T) {
	w := &Wait{On: "129", WaitDelay: "not-a-duration"}
	if got := w.WaitDelayOrDefault(); got != 10*time.Minute {
		t.Errorf("WaitDelayOrDefault() with malformed WaitDelay = %v, want 10m", got)
	}
}

func TestWaitDelayOrDefaultParsesExplicitValue(t *testing.T) {
	w := &Wait{On: "129", WaitDelay: "15m"}
	if got := w.WaitDelayOrDefault(); got != 15*time.Minute {
		t.Errorf("WaitDelayOrDefault() = %v, want 15m", got)
	}
}

func TestLoadParsesReportWaitSection(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  wait:
    on: "129"
    waitDelay: "15m"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report == nil || cfg.Report.Wait == nil {
		t.Fatalf("expected cfg.Report.Wait to be parsed, got %+v", cfg.Report)
	}
	if cfg.Report.Wait.On != "129" {
		t.Errorf("Report.Wait.On = %q, want %q", cfg.Report.Wait.On, "129")
	}
	if cfg.Report.Wait.WaitDelay != "15m" {
		t.Errorf("Report.Wait.WaitDelay = %q, want %q", cfg.Report.Wait.WaitDelay, "15m")
	}
}

func TestLoadWithNoReportSectionLeavesWaitNil(t *testing.T) {
	path := writeTemp(t, minimalYAML)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report != nil {
		t.Errorf("expected cfg.Report to be nil when config.yaml has no report section, got %+v", cfg.Report)
	}
}

func TestLoadWithReportButNoWaitSubsectionLeavesItNil(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  url: "https://example.com/report"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report == nil {
		t.Fatal("expected cfg.Report to be parsed")
	}
	if cfg.Report.Wait != nil {
		t.Errorf("expected cfg.Report.Wait to be nil when report has no wait subsection, got %+v", cfg.Report.Wait)
	}
}
