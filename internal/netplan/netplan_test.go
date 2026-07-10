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
	out := Render("ens18", seg)

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
	out := Render("ens18", seg)

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
	out := Render("ens18", seg)
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
	out := Render("ens18", seg)

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
	if strings.Contains(out, "ethernets:") || strings.Contains(out, "vlans:") {
		t.Errorf("wifi segment shouldn't touch the trunk NIC at all -- no ethernets:/vlans: stanza expected, got:\n%s", out)
	}
	if strings.Contains(out, "ens18") {
		t.Errorf("wifi segment's netplan shouldn't mention the trunk interface at all, got:\n%s", out)
	}
	assertValidNetplanYAML(t, out)
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
	out := Render("ens18", seg)
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
