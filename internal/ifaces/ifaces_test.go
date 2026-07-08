package ifaces

import (
	"testing"

	"github.com/mabels/mseg-tester/internal/config"
)

// vlanSegmentTranscript is a real `ip a` transcript captured on a
// verify-mseg-tester VM sitting on tagged VLAN segment 129 -- lo, the bare
// trunk NIC (ens18, no address), and the VLAN sub-interface actually
// carrying traffic (ens18.129@ens18).
const vlanSegmentTranscript = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host noprefixroute
       valid_lft forever preferred_lft forever
2: ens18: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc pfifo_fast state UP group default qlen 1000
    link/ether bc:24:11:27:ce:bf brd ff:ff:ff:ff:ff:ff
    altname enp0s18
    altname enxbc241127cebf
    inet6 fe80::be24:11ff:fe27:cebf/64 scope link proto kernel_ll
       valid_lft forever preferred_lft forever
3: ens18.129@ens18: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000
    link/ether bc:24:11:27:ce:bf brd ff:ff:ff:ff:ff:ff
    inet 192.168.129.56/24 metric 100 brd 192.168.129.255 scope global dynamic ens18.129
       valid_lft 86271sec preferred_lft 86271sec
    inet6 fd00:192:168:129:be24:11ff:fe27:cebf/64 scope global mngtmpaddr noprefixroute
       valid_lft forever preferred_lft forever
    inet6 fe80::be24:11ff:fe27:cebf/64 scope link proto kernel_ll
       valid_lft forever preferred_lft forever
`

// nativeSegmentTranscript is the same box, but for the native/untagged
// segment: only lo and the bare trunk NIC exist -- no VLAN sub-interface
// at all, and the trunk NIC itself carries the address.
const nativeSegmentTranscript = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host noprefixroute
       valid_lft forever preferred_lft forever
2: ens18: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc pfifo_fast state UP group default qlen 1000
    link/ether bc:24:11:27:ce:bf brd ff:ff:ff:ff:ff:ff
    inet 192.168.128.42/24 metric 100 brd 192.168.128.255 scope global dynamic ens18
       valid_lft 86271sec preferred_lft 86271sec
    inet6 fd00:192:168:128:be24:11ff:fe27:cebf/64 scope global mngtmpaddr noprefixroute
       valid_lft forever preferred_lft forever
    inet6 fe80::be24:11ff:fe27:cebf/64 scope link proto kernel_ll
       valid_lft forever preferred_lft forever
`

func TestParseVLANSegment(t *testing.T) {
	list, err := Parse(vlanSegmentTranscript)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 interfaces (lo, ens18, ens18.129), got %d: %+v", len(list), list)
	}
	byName := map[string]Iface{}
	for _, f := range list {
		byName[f.Name] = f
	}
	if _, ok := byName["lo"]; !ok {
		t.Error("expected \"lo\" to be parsed")
	}
	trunk, ok := byName["ens18"]
	if !ok {
		t.Fatal("expected \"ens18\" to be parsed")
	}
	if trunk.Parent != "" {
		t.Errorf("trunk interface should have no parent, got %q", trunk.Parent)
	}
	vlan, ok := byName["ens18.129"]
	if !ok {
		t.Fatal("expected \"ens18.129\" to be parsed")
	}
	if vlan.Parent != "ens18" {
		t.Errorf("vlan sub-interface Parent = %q, want \"ens18\"", vlan.Parent)
	}
	if !vlan.Up {
		t.Error("expected ens18.129 to be Up")
	}
	var v4, v6global int
	for _, a := range vlan.Addrs {
		if a.IP == nil {
			t.Errorf("unparsed address: %q", a.CIDR)
			continue
		}
		if a.IP.To4() != nil {
			v4++
		} else if !a.IP.IsLinkLocalUnicast() {
			v6global++
		}
	}
	if v4 != 1 {
		t.Errorf("expected 1 IPv4 address on ens18.129, got %d", v4)
	}
	if v6global != 1 {
		t.Errorf("expected 1 global IPv6 address on ens18.129, got %d", v6global)
	}
}

func TestFindVLANSegment(t *testing.T) {
	list, err := Parse(vlanSegmentTranscript)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	iface, err := Find(list, config.Segment{Name: "129", Type: "vlan"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if iface.Name != "ens18.129" {
		t.Errorf("Find() = %q, want \"ens18.129\"", iface.Name)
	}
}

func TestFindNativeSegment(t *testing.T) {
	list, err := Parse(nativeSegmentTranscript)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	iface, err := Find(list, config.Segment{Name: "128", Type: "native"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if iface.Name != "ens18" {
		t.Errorf("Find() = %q, want \"ens18\"", iface.Name)
	}
}

func TestFindIfNameOverride(t *testing.T) {
	list, err := Parse(vlanSegmentTranscript)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	iface, err := Find(list, config.Segment{Name: "129", Type: "vlan", IfName: "ens18.129"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if iface.Name != "ens18.129" {
		t.Errorf("Find() = %q, want \"ens18.129\"", iface.Name)
	}

	if _, err := Find(list, config.Segment{Name: "129", Type: "vlan", IfName: "does-not-exist"}); err == nil {
		t.Error("expected an error for a configured ifname not present in `ip a` output")
	}
}

func TestFindAmbiguousTopology(t *testing.T) {
	// Three non-loopback interfaces shouldn't happen in practice (netplan.Write
	// never brings up more than trunk + one VLAN sub-interface), but Find should
	// fail loudly rather than guess.
	transcript := vlanSegmentTranscript + `4: ens19: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc pfifo_fast state UP group default qlen 1000
    link/ether bc:24:11:27:ce:c0 brd ff:ff:ff:ff:ff:ff
    inet 10.0.0.5/24 scope global ens19
       valid_lft forever preferred_lft forever
`
	list, err := Parse(transcript)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Find(list, config.Segment{Name: "129", Type: "vlan"}); err == nil {
		t.Error("expected an error when more than 2 non-loopback interfaces are present")
	}
}
