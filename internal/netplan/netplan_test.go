package netplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mabels/mseg-tester/internal/config"
	"gopkg.in/yaml.v3"
)

func TestIfaceName(t *testing.T) {
	cases := []struct {
		name       string
		trunkIface string
		seg        config.Segment
		want       string
	}{
		{
			name:       "tagged vlan segment",
			trunkIface: "ens18",
			seg:        config.Segment{Name: "129", Type: "vlan"},
			want:       "ens18.129",
		},
		{
			name:       "the native segment itself",
			trunkIface: "ens18",
			seg:        config.Segment{Name: "128", Type: "native"},
			want:       "ens18",
		},
		{
			name:       "ifname override wins regardless of type",
			trunkIface: "ens18",
			seg:        config.Segment{Name: "129", Type: "vlan", IfName: "eth1.99"},
			want:       "eth1.99",
		},
		{
			name:       "wifi segment uses its required ifname override -- no trunk-derived default exists",
			trunkIface: "ens18",
			seg:        config.Segment{Name: "wifi-128", Type: "wifi", IfName: "wlan0"},
			want:       "wlan0",
		},
	}
	for _, c := range cases {
		if got := IfaceName(c.trunkIface, c.seg); got != c.want {
			t.Errorf("%s: IfaceName() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenderNativeSegment(t *testing.T) {
	seg := config.Segment{Name: "128", Type: "native"}
	out := Render("ens18", seg, nil)

	if !strings.Contains(out, "    ens18:") {
		t.Errorf("expected the trunk interface configured directly, got:\n%s", out)
	}
	if strings.Contains(out, "vlans:") {
		t.Errorf("expected no vlans stanza for a native segment, got:\n%s", out)
	}
	if !strings.Contains(out, "dhcp4: true") || !strings.Contains(out, "accept-ra: true") {
		t.Errorf("expected dhcp4+accept-ra enabled, got:\n%s", out)
	}
	if !strings.Contains(out, "link-local: [ipv4, ipv6]") {
		t.Errorf("expected explicit link-local so RS can be sourced (see OVN solicited-RA-only note), got:\n%s", out)
	}
	if strings.Contains(out, "optional: true") {
		t.Errorf("native segment IS the trunk -- wait-online should legitimately wait for it, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderVLANSegment(t *testing.T) {
	seg := config.Segment{Name: "129", Type: "vlan"}
	out := Render("ens18", seg, nil)

	if !strings.Contains(out, "ens18.129:") {
		t.Errorf("expected a tagged VLAN sub-interface, got:\n%s", out)
	}
	if !strings.Contains(out, "optional: true") {
		t.Errorf("expected the bare trunk NIC marked optional (it never gets an address), got:\n%s", out)
	}
	if !strings.Contains(out, "id: 129") {
		t.Errorf("expected VLAN id to match the segment name, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderVLANSegmentBareTrunkDoesNotAutoconfigureFromNativeSegmentRA(t *testing.T) {
	// Regression, confirmed live: this trunk NIC physically carries
	// every VLAN, including whatever segment (if any) is this trunk's
	// native/untagged one -- that segment's real router advertisements
	// still arrive untagged on the bare trunk no matter which TAGGED
	// segment mseg-tester currently considers active. Without an
	// explicit accept-ra: false (and link-local: []) on the bare trunk
	// itself, the kernel autoconfigured a real global SLAAC address on
	// it from those RAs regardless -- parse this out structurally (not
	// just substring-search) since both "true" and "false" appear
	// legitimately elsewhere in the same document (the VLAN
	// sub-interface's own accept-ra: true).
	seg := config.Segment{Name: "129", Type: "vlan"}
	out := Render("ens18", seg, nil)

	var parsed struct {
		Network struct {
			Ethernets map[string]struct {
				AcceptRA  *bool    `yaml:"accept-ra"`
				LinkLocal []string `yaml:"link-local"`
			} `yaml:"ethernets"`
		} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out)
	}
	trunk, ok := parsed.Network.Ethernets["ens18"]
	if !ok {
		t.Fatalf("expected an ethernets.ens18 entry, got:\n%s", out)
	}
	if trunk.AcceptRA == nil || *trunk.AcceptRA {
		t.Errorf("expected the bare trunk's accept-ra explicitly false, got %v, out:\n%s", trunk.AcceptRA, out)
	}
	if len(trunk.LinkLocal) != 0 {
		t.Errorf("expected the bare trunk's link-local explicitly empty, got %v, out:\n%s", trunk.LinkLocal, out)
	}
}

func TestRenderIfNameOverride(t *testing.T) {
	seg := config.Segment{Name: "130", Type: "vlan", IfName: "customvlan0"}
	out := Render("ens18", seg, nil)
	if !strings.Contains(out, "customvlan0:") {
		t.Errorf("expected the IfName override to be used as the VLAN sub-interface name, got:\n%s", out)
	}
}

func TestRenderWifiSegment(t *testing.T) {
	seg := config.Segment{
		Name:   "wifi-128",
		Type:   "wifi",
		IfName: "wlan0",
		SSID:   "MAM-HH",
		PSK:    "von Winsen nach Hamburg",
	}
	out := Render("ens18", seg, nil)

	if !strings.Contains(out, "wifis:") {
		t.Errorf("expected a wifis: stanza, got:\n%s", out)
	}
	if !strings.Contains(out, "wlan0:") {
		t.Errorf("expected the ifname override used as the wifi interface name, got:\n%s", out)
	}
	if !strings.Contains(out, "access-points:") || !strings.Contains(out, `"MAM-HH":`) {
		t.Errorf("expected an access-points entry keyed by SSID, got:\n%s", out)
	}
	if !strings.Contains(out, `password: "von Winsen nach Hamburg"`) {
		t.Errorf("expected the PSK as a quoted password, got:\n%s", out)
	}
	if strings.Contains(out, "vlans:") {
		t.Errorf("wifi segment doesn't ride a VLAN -- no vlans: stanza expected, got:\n%s", out)
	}
	// The trunk NIC (net0/virtio) exists as a kernel device whether or
	// not netplan mentions it -- so a wifi segment's netplan MUST
	// explicitly force it off (activation-mode: off), not simply omit
	// it, or internal/ifaces.Find's interface-counting heuristic (and
	// potentially the trunk itself) can end up in an unexpected state.
	// See Render's disableWifiIfaces doc comment.
	if !strings.Contains(out, "ethernets:") {
		t.Errorf("expected an ethernets: stanza explicitly forcing the trunk NIC off, got:\n%s", out)
	}
	if !strings.Contains(out, "ens18:") {
		t.Errorf("expected the trunk interface (ens18) named in that ethernets: stanza, got:\n%s", out)
	}
	if !strings.Contains(out, "activation-mode: off") {
		t.Errorf("expected the trunk NIC forced off via activation-mode: off, got:\n%s", out)
	}
	if !strings.Contains(out, "link-local: []") {
		t.Errorf("expected the trunk NIC's link-local address assignment also disabled, got:\n%s", out)
	}
	if !strings.Contains(out, "accept-ra: false") {
		t.Errorf("expected the trunk NIC's accept-ra also disabled, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderWifiSegmentDisablesOtherWifiIfaces(t *testing.T) {
	seg := config.Segment{
		Name: "wifi-128", Type: "wifi", IfName: "wlan0",
		SSID: "MAM-HH", PSK: "secret",
	}
	out := Render("ens18", seg, []string{"wlan1"})

	if !strings.Contains(out, "wlan1:") {
		t.Errorf("expected the other radio (wlan1) named for explicit shutdown, got:\n%s", out)
	}
	// Both wlan0 (active) and wlan1 (forced off) must be under the SAME
	// "wifis:" key -- a second top-level "wifis:" key would silently
	// clobber the first in YAML.
	if strings.Count(out, "wifis:") != 1 {
		t.Errorf("expected exactly one \"wifis:\" key, got %d, out:\n%s", strings.Count(out, "wifis:"), out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderVLANSegmentDisablesWifiIface(t *testing.T) {
	seg := config.Segment{Name: "129", Type: "vlan"}
	out := Render("ens18", seg, []string{"wlan0"})

	if !strings.Contains(out, "wifis:") {
		t.Errorf("expected a wifis: stanza forcing wlan0 off, got:\n%s", out)
	}
	if !strings.Contains(out, "wlan0:") || !strings.Contains(out, "activation-mode: off") {
		t.Errorf("expected wlan0 explicitly forced off, got:\n%s", out)
	}
	if !strings.Contains(out, "link-local: []") {
		t.Errorf("expected wlan0's link-local address assignment also disabled, got:\n%s", out)
	}
	if !strings.Contains(out, "accept-ra: false") {
		t.Errorf("expected wlan0's accept-ra also disabled, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderNativeSegmentDisablesWifiIface(t *testing.T) {
	seg := config.Segment{Name: "128", Type: "native"}
	out := Render("ens18", seg, []string{"wlan0"})

	if !strings.Contains(out, "wifis:") || !strings.Contains(out, "wlan0:") || !strings.Contains(out, "activation-mode: off") {
		t.Errorf("expected wlan0 explicitly forced off, got:\n%s", out)
	}
	if !strings.Contains(out, "link-local: []") {
		t.Errorf("expected wlan0's link-local address assignment also disabled, got:\n%s", out)
	}
	if !strings.Contains(out, "accept-ra: false") {
		t.Errorf("expected wlan0's accept-ra also disabled, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
}

func TestRenderNoDisableListMeansNoExtraStanza(t *testing.T) {
	seg := config.Segment{Name: "128", Type: "native"}
	out := Render("ens18", seg, nil)
	if strings.Contains(out, "wifis:") {
		t.Errorf("expected no wifis: stanza at all when disableWifiIfaces is empty, got:\n%s", out)
	}
}

func TestRenderWifiEscapesSSIDAndPSK(t *testing.T) {
	// SSID/PSK are free-text, operator-supplied values (PSK itself
	// usually arrives via a "${VAR}" expansion -- see
	// config.Segment.PSK's doc comment) -- unlike every other value this
	// package embeds (segment/interface names, always safe bare
	// identifiers by construction), these need real YAML escaping. A
	// password containing a double quote, a colon, and a backslash (all
	// YAML-significant) is the sharpest practical test of that.
	seg := config.Segment{
		Name:   "wifi-131",
		Type:   "wifi",
		IfName: "wlan0",
		SSID:   "MAM-HH-US",
		PSK:    `weird"pass: word\here`,
	}
	out := Render("ens18", seg, nil)
	assertValidNetplanYAML(t, out)

	var parsed struct {
		Network struct {
			Wifis map[string]struct {
				AccessPoints map[string]struct {
					Password string `yaml:"password"`
				} `yaml:"access-points"`
			} `yaml:"wifis"`
		} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out)
	}
	ap, ok := parsed.Network.Wifis["wlan0"].AccessPoints["MAM-HH-US"]
	if !ok {
		t.Fatalf("expected access-points[\"MAM-HH-US\"] in parsed output:\n%s", out)
	}
	if ap.Password != `weird"pass: word\here` {
		t.Errorf("round-tripped password = %q, want the original PSK back exactly", ap.Password)
	}
}

func assertValidNetplanYAML(t *testing.T, doc string) {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("rendered netplan is not valid YAML: %v\n%s", err, doc)
	}
}

// withTempNetplanPath points package-level path at a file under t.TempDir()
// for the duration of the calling test, restoring the real path afterward
// -- Write must never touch the real /etc/netplan/90-mseg-tester.yaml
// during tests.
func withTempNetplanPath(t *testing.T) {
	t.Helper()
	real := path
	path = filepath.Join(t.TempDir(), "90-mseg-tester.yaml")
	t.Cleanup(func() { path = real })
}

func TestWriteCreatesPerSegmentFileAndHardLinksPath(t *testing.T) {
	withTempNetplanPath(t)
	seg := config.Segment{Name: "129", Type: "vlan"}

	if err := Write("ens18", seg, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}

	segContent, err := os.ReadFile(segPath(seg))
	if err != nil {
		t.Fatalf("expected %s to exist: %v", segPath(seg), err)
	}
	pathContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if string(segContent) != string(pathContent) {
		t.Fatalf("path and segPath content differ:\npath:    %s\nsegPath: %s", pathContent, segContent)
	}

	segInfo, err := os.Stat(segPath(seg))
	if err != nil {
		t.Fatalf("Stat segPath: %v", err)
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat path: %v", err)
	}
	if !os.SameFile(segInfo, pathInfo) {
		t.Errorf("expected path to be a hard link to segPath (same inode), got two distinct files")
	}
}

func TestWriteKeepsPriorSegmentFilesAndRepointsPath(t *testing.T) {
	withTempNetplanPath(t)
	seg128 := config.Segment{Name: "128", Type: "native"}
	seg129 := config.Segment{Name: "129", Type: "vlan"}

	if err := Write("ens18", seg128, nil); err != nil {
		t.Fatalf("Write(128): %v", err)
	}
	if err := Write("ens18", seg129, nil); err != nil {
		t.Fatalf("Write(129): %v", err)
	}

	// 128's own debug file must still exist, untouched by 129's Write --
	// this is the whole point of the feature: inspecting what a segment
	// last rendered even after the cycle has moved past it.
	if _, err := os.Stat(segPath(seg128)); err != nil {
		t.Fatalf("expected %s to still exist after writing a different segment: %v", segPath(seg128), err)
	}

	// path must now point at 129's file, not 128's.
	pathInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat path: %v", err)
	}
	seg129Info, err := os.Stat(segPath(seg129))
	if err != nil {
		t.Fatalf("Stat segPath(129): %v", err)
	}
	if !os.SameFile(pathInfo, seg129Info) {
		t.Errorf("expected path to now be a hard link to segPath(129), not segPath(128)")
	}
}

func TestSegPathDoesNotEndInYAMLExtension(t *testing.T) {
	// Deliberate: netplan's own "/etc/netplan/*.yaml" glob must never
	// pick up these per-segment debug files as live config -- see
	// Write's doc comment.
	got := segPath(config.Segment{Name: "130"})
	if strings.HasSuffix(got, ".yaml") {
		t.Errorf("segPath() = %q, must NOT end in \".yaml\" or netplan would treat it as live config", got)
	}
}
