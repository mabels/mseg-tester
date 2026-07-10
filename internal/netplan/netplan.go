// Package netplan writes the ONE netplan file this tool owns:
// /etc/netplan/90-mseg-tester.yaml. It is always a full, wholesale
// rewrite -- there is no incremental "add this VLAN, remove that one"
// step, because netplan itself doesn't work that way: on `netplan apply`
// (or, here, on the next boot), whatever's NOT declared in the merged
// config across all netplan files simply isn't (re)created. Writing a
// fresh file naming only the one target segment's VLAN is what "disable
// the others" actually means in practice for a VLAN sub-interface --
// there's genuinely nothing else to tear down, since it doesn't exist
// until netplan creates it.
//
// A PCI-passthrough Wi-Fi radio is a different story: its kernel network
// device exists as soon as its driver binds, entirely independent of
// netplan, so merely not mentioning it in a given segment's file doesn't
// actually leave it "off" -- confirmed live, and why Render/Write take a
// disableWifiIfaces parameter that explicitly forces every OTHER
// Wi-Fi-capable interface (and, on a "wifi" segment, the trunk NIC too)
// down via "activation-mode: off" rather than relying on omission alone.
//
// Most segments ride as an 802.1Q-tagged VLAN sub-interface on top of the
// trunk NIC (Proxmox hands this VM one NIC, trunked with every segment's
// VLAN tag, set up once by hand -- see the cloud-init/README notes on
// that one-time step). One segment, though, may instead be the trunk
// port's NATIVE/untagged VLAN -- see IfaceName below.
package netplan

import (
	"fmt"
	"os"
	"strings"

	"github.com/mabels/mseg-tester/internal/config"
	"gopkg.in/yaml.v3"
)

const path = "/etc/netplan/90-mseg-tester.yaml"

// IfaceName returns the interface Write configures (and the same one
// internal/checks looks addresses up on) for seg. seg.IfName, if set,
// overrides the derived name entirely. Otherwise: the trunk interface
// itself, with no ".segment" sub-interface at all, when seg.Type ==
// "native" (its traffic never carries an 802.1Q tag); a tagged VLAN
// sub-interface, "<trunkInterface>.<seg.Name>", otherwise. seg.Type is
// config.yaml's single source of truth for which segment (if any) is
// the trunk's native/untagged one.
func IfaceName(trunkInterface string, seg config.Segment) string {
	if seg.IfName != "" {
		return seg.IfName
	}
	if seg.Type == "native" {
		return trunkInterface
	}
	return fmt.Sprintf("%s.%s", trunkInterface, seg.Name)
}

// yamlDoubleQuote renders s as a double-quoted YAML scalar (proper
// backslash/quote escaping via yaml.v3's own encoder, not hand-rolled) --
// used for SSID/PSK, the only free-text, operator-supplied values this
// package ever has to embed into generated YAML. Every other embedded
// value (segment names, interface names) is a safe bare identifier by
// construction, so this is the one place embedding needed real escaping.
func yamlDoubleQuote(s string) string {
	n := yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: s}
	b, err := yaml.Marshal(&n)
	if err != nil {
		// Unreachable for a plain scalar string, but fall back to
		// something still-valid rather than panicking either way.
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimRight(string(b), "\n")
}

// offDeviceEntries renders one device entry per name, each forced
// administratively off ("activation-mode: off") -- the shared body used
// under both an "ethernets:" and a "wifis:" stanza (the schema for both
// is otherwise the same set of keys). See disableWifiIfaces' doc comment
// on Render for why this is needed at all, rather than just omitting the
// device. link-local: [] and accept-ra: false close off the two other
// ways this "off" device could still end up with an address/participate
// on the network even though it's never brought up under normal
// operation (activation-mode: off) -- an IPv6 link-local address alone
// is enough for local discovery/neighbor traffic, and accept-ra would
// otherwise still process a router advertisement into a real SLAAC
// address exactly the way every ACTIVE segment here deliberately wants
// (see the "native"/"vlan" branches' own accept-ra: true) if this device
// somehow received one anyway.
func offDeviceEntries(names []string) string {
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "    %s:\n      dhcp4: false\n      dhcp6: false\n      accept-ra: false\n      link-local: []\n      activation-mode: off\n", name)
	}
	return b.String()
}

// offEthernetsStanza renders a complete "ethernets:" stanza forcing name
// off -- used by the "wifi" branch to force the trunk NIC off, since
// leaving it merely undeclared isn't enough once anything else on the
// box (not just Wi-Fi -- see disableWifiIfaces' doc comment on Render
// for the general problem) could otherwise leave it lingering.
func offEthernetsStanza(name string) string {
	return "  ethernets:\n" + offDeviceEntries([]string{name})
}

// offWifisStanza renders a complete "wifis:" stanza forcing every named
// interface off, or "" if names is empty -- see disableWifiIfaces' doc
// comment on Render.
func offWifisStanza(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "  wifis:\n" + offDeviceEntries(names)
}

// Render builds the netplan YAML content Write would install for seg,
// without touching disk -- split out so it can be inspected offline (see
// cmd/mseg-tester's "render-netplan" subcommand), e.g. when a VM is stuck
// at boot and there's no shell to read /etc/netplan/90-mseg-tester.yaml
// from directly.
//
// disableWifiIfaces names every Wi-Fi-capable interface that ISN'T this
// segment's own (see cmd/mseg-tester's caller, which derives it via
// internal/ifdiscover.ListWireless) -- each gets an explicit
// "activation-mode: off" entry, forcing it administratively down, rather
// than simply being left out of this file the way earlier versions of
// this package worked. That distinction matters: a PCI-passthrough Wi-Fi
// radio's kernel network device exists (and is visible to
// internal/ifaces.Find, which counts it) as soon as its driver binds --
// entirely independent of whether netplan ever mentions it -- so merely
// omitting it doesn't actually keep it "off" the way the same omission
// does for e.g. a tagged VLAN sub-interface that genuinely doesn't exist
// until netplan creates it. Confirmed live: switching between a "wifi"
// segment and a wired one left the previously-active radio still
// present (if not necessarily still associated) until this was added.
// Empty/nil is fine -- most boxes have at most one radio, so this is
// usually empty on a "wifi" segment's own render and a 1-element slice
// otherwise.
func Render(trunkInterface string, seg config.Segment, disableWifiIfaces []string) string {
	var content string
	switch seg.Type {
	case "native":
		// The active segment IS the trunk's native/untagged VLAN --
		// configure the trunk interface directly. No vlans stanza at
		// all: there's nothing 802.1Q about this segment's traffic.
		content = fmt.Sprintf(`# Generated by mseg-tester -- do not hand-edit, it is overwritten
# wholesale every time the active segment changes. See internal/netplan.
#
# Segment %s is this trunk's NATIVE/untagged VLAN -- its traffic arrives
# directly on %s, not on a tagged sub-interface, so %s is configured here
# exactly as any normal single-segment NIC would be.
#
# dhcp6 stays false -- this network uses SLAAC (router advertisements),
# not DHCPv6. accept-ra/link-local are set explicitly rather than left to
# netplan's defaults: OVN on this network can only ever deliver a
# SOLICITED router advertisement (its own periodic RA is unconditionally
# dropped -- see internal/checks' package doc), so a client MUST have a
# link-local address to source its router solicitation from, or SLAAC
# never completes at all. link-local: [] has broken this before.
network:
  version: 2
  ethernets:
    %s:
      dhcp4: true
      dhcp6: false
      accept-ra: true
      link-local: [ipv4, ipv6]
%s`, seg.Name, trunkInterface, trunkInterface, trunkInterface, offWifisStanza(disableWifiIfaces))

	case "wifi":
		// The active segment is a dedicated Wi-Fi radio (config.Segment.Type
		// "wifi", see its doc comment) -- passed through to this VM as its
		// own PCI/USB device, not riding the trunk NIC's VLANs at all.
		// seg.IfName is REQUIRED for this type (enforced by config.Load):
		// unlike "vlan" there's no trunk-derived default name for a
		// standalone radio. The trunk interface is explicitly forced off
		// below (activation-mode: off) rather than simply left out of
		// this file, same rationale as disableWifiIfaces' doc comment on
		// Render -- it exists as a kernel device (net0/virtio) whether or
		// not netplan mentions it, so "not mentioned" alone doesn't mean
		// "off".
		//
		// dhcp6/accept-ra/link-local: same SLAAC rationale as every other
		// branch here. SSID/password go through yamlDoubleQuote rather
		// than a bare %s -- unlike every other value embedded by this
		// package, these are free-text and operator-supplied (from
		// config.yaml's ssid/psk, the latter itself expanded from a
		// "${VAR}" -- see config.Segment.PSK's doc comment), so they need
		// real YAML escaping, not just interpolation.
		content = fmt.Sprintf(`# Generated by mseg-tester -- do not hand-edit, it is overwritten
# wholesale every time the active segment changes. See internal/netplan.
#
# Segment %s is a dedicated Wi-Fi radio (config.Segment.Type "wifi") --
# %s associates directly to an access point. The trunk NIC (%s) is
# explicitly forced off below -- see Render's disableWifiIfaces doc
# comment for why that's not just belt-and-suspenders.
#
# dhcp6 stays false -- this network uses SLAAC (router advertisements),
# not DHCPv6. accept-ra/link-local are set explicitly for the same reason
# as every other segment here -- see the "native" branch's comment above.
network:
  version: 2
  wifis:
    %s:
      dhcp4: true
      dhcp6: false
      accept-ra: true
      link-local: [ipv4, ipv6]
      access-points:
        %s:
          password: %s
%s%s`, seg.Name, seg.IfName, trunkInterface, seg.IfName, yamlDoubleQuote(seg.SSID), yamlDoubleQuote(seg.PSK),
			offDeviceEntries(disableWifiIfaces), offEthernetsStanza(trunkInterface))

	default: // "vlan"
		ifaceName := IfaceName(trunkInterface, seg)
		vlanID := seg.Name // ovn-fabric's own convention too: the segment name IS the VLAN ID

		content = fmt.Sprintf(`# Generated by mseg-tester -- do not hand-edit, it is overwritten
# wholesale every time the active segment changes. See internal/netplan.
#
# The trunk interface itself carries no address of its own here -- it
# exists purely so the tagged VLAN sub-interface below can ride on top of
# it. "optional: true" tells systemd-networkd-wait-online not to block
# boot waiting for IT specifically to become "routable" (it never will,
# it's a pure VLAN carrier) -- without this, boot can hang for a long
# time (observed: "no limit" on systemd-networkd-wait-online.service)
# waiting on an interface that was never going to get an address.
#
# dhcp6 stays false -- this network uses SLAAC (router advertisements),
# not DHCPv6. accept-ra/link-local are set explicitly rather than left to
# netplan's defaults: OVN on this network can only ever deliver a
# SOLICITED router advertisement (its own periodic RA is unconditionally
# dropped -- see internal/checks' package doc), so a client MUST have a
# link-local address to source its router solicitation from, or SLAAC
# never completes at all. link-local: [] has broken this before.
network:
  version: 2
  ethernets:
    %s:
      dhcp4: false
      dhcp6: false
      optional: true
  vlans:
    %s:
      id: %s
      link: %s
      dhcp4: true
      dhcp6: false
      accept-ra: true
      link-local: [ipv4, ipv6]
%s`, trunkInterface, ifaceName, vlanID, trunkInterface, offWifisStanza(disableWifiIfaces))
	}
	return content
}

// Write generates and installs the netplan config for exactly one active
// segment. Does NOT call `netplan apply` -- this tool's whole model is
// "write config, then reboot" (see cmd/mseg-tester's run loop), so a live
// apply here would just be redundant work before the reboot throws it
// away and re-derives the same state from disk anyway. disableWifiIfaces
// is passed straight through to Render -- see its doc comment.
func Write(trunkInterface string, seg config.Segment, disableWifiIfaces []string) error {
	content := Render(trunkInterface, seg, disableWifiIfaces)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("netplan: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("netplan: renaming %s -> %s: %w", tmp, path, err)
	}
	return nil
}
