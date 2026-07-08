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

func assertValidNetplanYAML(t *testing.T, doc string) {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("rendered netplan is not valid YAML: %v\n%s", err, doc)
	}
}
