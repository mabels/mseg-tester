package netplan

import (
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
