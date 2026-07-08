// Package ifaces discovers which real network interface is carrying a
// segment's traffic by shelling out to `ip a` and parsing its plain-text
// output -- deliberately NOT netlink/ioctls (which is what net.Interfaces()
// uses under the hood): parsing `ip a` is simpler to reason about, trivial
// to unit-test against a captured real transcript, and doesn't require
// trusting that the name internal/netplan.IfaceName WOULD derive is the
// name the kernel actually assigned. VLAN sub-interfaces show up in `ip a`
// as e.g. "ens18.129@ens18", not a bare string -- this package reads that
// directly rather than assuming it.
//
// mseg-tester's own netplan.Write only ever brings up at most three
// interfaces on this box at any one time: "lo", the trunk NIC itself, and
// -- only when the active segment is a tagged VLAN, not the trunk's
// native/untagged one -- exactly one "<trunk>.<segment>@<trunk>" VLAN
// sub-interface. That small, fixed universe is what Find below relies on:
// "whichever non-loopback interface is actually the right one" can be
// determined without matching any name at all, in either topology --
// see Find's doc comment.
package ifaces

import (
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"

	"github.com/mabels/mseg-tester/internal/config"
)

// Addr is one address line under an interface block in `ip a`'s output.
type Addr struct {
	CIDR string // e.g. "192.168.129.56/24", exactly as `ip a` printed it
	IP   net.IP // parsed from CIDR (stripped of the /prefix) -- nil if unparseable, never fatal
}

// Iface is one interface block from `ip a`.
type Iface struct {
	Name   string // e.g. "ens18.129" -- the "@parent" suffix, if any, is split into Parent below
	Parent string // e.g. "ens18" -- empty unless this is a VLAN sub-interface riding on another
	Up     bool   // true if "UP" appears in the interface's <FLAGS> list
	Addrs  []Addr
}

// List runs `ip a` and parses its output.
func List() ([]Iface, error) {
	out, err := exec.Command("ip", "a").Output()
	if err != nil {
		return nil, fmt.Errorf("ifaces: running `ip a`: %w", err)
	}
	return Parse(string(out))
}

// headerRE matches an interface block's first line, e.g.:
//
//	2: ens18: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
//	3: ens18.129@ens18: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
var headerRE = regexp.MustCompile(`^\d+:\s+([^@:\s]+)(?:@([^:\s]+))?:\s+<([^>]*)>`)

// addrRE matches an address line under an interface block, e.g.:
//
//	inet 192.168.129.56/24 metric 100 brd 192.168.129.255 scope global dynamic ens18.129
//	inet6 fd00:192:168:129:be24:11ff:fe27:cebf/64 scope global mngtmpaddr noprefixroute
var addrRE = regexp.MustCompile(`^\s+inet6?\s+(\S+)`)

// Parse parses `ip a`'s plain-text output into one Iface per interface
// block -- split out from List so tests can feed it a captured real
// transcript without shelling out to `ip` at all.
func Parse(output string) ([]Iface, error) {
	var (
		result []Iface
		cur    *Iface
	)
	flush := func() {
		if cur != nil {
			result = append(result, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(output, "\n") {
		if m := headerRE.FindStringSubmatch(line); m != nil {
			flush()
			up := false
			for _, f := range strings.Split(m[3], ",") {
				if f == "UP" {
					up = true
					break
				}
			}
			cur = &Iface{Name: m[1], Parent: m[2], Up: up}
			continue
		}
		if cur == nil {
			continue
		}
		if m := addrRE.FindStringSubmatch(line); m != nil {
			cidr := m[1]
			ipStr := cidr
			if i := strings.IndexByte(cidr, '/'); i >= 0 {
				ipStr = cidr[:i]
			}
			cur.Addrs = append(cur.Addrs, Addr{CIDR: cidr, IP: net.ParseIP(ipStr)})
		}
	}
	flush()
	if len(result) == 0 {
		return nil, fmt.Errorf("ifaces: no interfaces parsed from `ip a` output")
	}
	return result, nil
}

// Find returns the interface actually carrying seg's traffic.
//
// If seg.IfName is set, that exact name is looked up directly (still an
// error if it isn't present -- an explicit override should exist).
//
// Otherwise it's discovered from the fixed "lo, trunk, at most one VLAN
// sub-interface" universe described in the package doc, with no name
// assumed or compared against config at all: exactly one non-loopback
// interface means seg is the trunk's native/untagged VLAN (its traffic
// rides the trunk NIC directly); exactly two means seg is a tagged VLAN
// (the bare trunk NIC, address-less and marked "optional" by
// internal/netplan.Write, plus the "name@parent" VLAN sub-interface
// that's actually carrying seg's traffic -- identified by having a
// Parent at all, not by matching any particular name).
func Find(list []Iface, seg config.Segment) (Iface, error) {
	if seg.IfName != "" {
		for _, f := range list {
			if f.Name == seg.IfName {
				return f, nil
			}
		}
		return Iface{}, fmt.Errorf("ifaces: configured ifname %q not found in `ip a` output", seg.IfName)
	}

	var nonLo []Iface
	for _, f := range list {
		if f.Name != "lo" {
			nonLo = append(nonLo, f)
		}
	}

	switch len(nonLo) {
	case 0:
		return Iface{}, fmt.Errorf("ifaces: no non-loopback interfaces found in `ip a` output")
	case 1:
		// Native/untagged segment: traffic rides the trunk NIC directly,
		// no VLAN sub-interface exists at all.
		return nonLo[0], nil
	case 2:
		// Tagged VLAN segment: one is the bare trunk NIC, the other is
		// the VLAN sub-interface ("name@parent") actually carrying seg's
		// traffic.
		for _, f := range nonLo {
			if f.Parent != "" {
				return f, nil
			}
		}
		return Iface{}, fmt.Errorf("ifaces: found 2 non-loopback interfaces but neither looks like a VLAN sub-interface (no \"@parent\"): %s, %s", nonLo[0].Name, nonLo[1].Name)
	default:
		names := make([]string, len(nonLo))
		for i, f := range nonLo {
			names[i] = f.Name
		}
		return Iface{}, fmt.Errorf("ifaces: expected 1 (native segment) or 2 (trunk + VLAN sub-interface) non-loopback interfaces, found %d: %s", len(nonLo), strings.Join(names, ", "))
	}
}
