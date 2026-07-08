package netplan

import "testing"

func TestIfaceName(t *testing.T) {
	cases := []struct {
		name          string
		trunkIface    string
		segment       string
		nativeSegment string
		want          string
	}{
		{name: "no native segment configured", trunkIface: "ens18", segment: "129", nativeSegment: "", want: "ens18.129"},
		{name: "tagged segment, native segment is something else", trunkIface: "ens18", segment: "129", nativeSegment: "128", want: "ens18.129"},
		{name: "the native segment itself", trunkIface: "ens18", segment: "128", nativeSegment: "128", want: "ens18"},
	}
	for _, c := range cases {
		if got := IfaceName(c.trunkIface, c.segment, c.nativeSegment); got != c.want {
			t.Errorf("%s: IfaceName() = %q, want %q", c.name, got, c.want)
		}
	}
}
