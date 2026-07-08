package netplan

import (
	"testing"

	"github.com/mabels/mseg-tester/internal/config"
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
