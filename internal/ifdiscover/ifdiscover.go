// Package ifdiscover finds which network interface corresponds to a
// piece of physical hardware -- a MAC address, a PCI vendor:device ID
// pair, or (with no criteria at all) simply the first Wi-Fi-capable
// radio present. Exists so a config.yaml "wifi" segment doesn't have to
// hardcode an interface name that's only stable until the next
// Proxmox/QEMU upgrade reshuffles virtual PCI slot numbering: a
// passed-through card's guest-side name is decided by udev's predictable
// naming applied to whatever virtual PCI slot QEMU happened to assign it
// THIS boot, which is Proxmox/QEMU's own bookkeeping, not a property of
// the physical card. MAC address and PCI vendor:device ID are both
// facts about the card itself instead, stable across any of that.
//
// Reads directly from /sys/class/net and /sys/bus/pci/devices -- the
// same data `ip link`/`lspci` themselves read -- so this package has no
// external command dependency at all.
package ifdiscover

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Criteria narrows Find to one specific interface. The zero value means
// "auto" -- see Find's doc comment for what that picks. Only one
// strategy is normally set at once; if more than one is, Find's priority
// order (MAC, then PCI, then auto) applies -- see Find.
type Criteria struct {
	// MAC, if set, must exactly match (case-insensitively) the target
	// interface's hardware address, e.g. "90:7a:be:dc:34:a9".
	MAC string
	// PCIVendor and PCIDevice, if both set, must exactly match (hex,
	// with or without a "0x" prefix, e.g. "14c3"/"0616" for a MediaTek
	// MT7922) the PCI device currently bound to the target interface.
	// Setting only one of the two is an error -- see Find.
	PCIVendor string
	PCIDevice string
}

// Find resolves c to exactly one interface name.
//
// sysClassNet is normally "/sys/class/net"; sysBusPCIDevices is normally
// "/sys/bus/pci/devices" -- both parameterized so tests can point at a
// fixture tree instead of the real filesystem (see ResolveIfName for the
// real-filesystem convenience wrapper production code should actually
// call).
//
// Priority, evaluated in this order: c.MAC if set; else
// c.PCIVendor+c.PCIDevice if both set (only one of the pair set is a
// caller error); else "auto" (c is the zero value) -- the first
// Wi-Fi-capable interface under sysClassNet (has a "phy80211" symlink or
// a "wireless" subdirectory -- either indicates a Wi-Fi radio; not every
// kernel/driver exposes both), excluding "lo", in lexicographically
// sorted order so the choice is deterministic across boots even when
// kernel enumeration order isn't.
func Find(sysClassNet, sysBusPCIDevices string, c Criteria) (string, error) {
	switch {
	case c.MAC != "":
		return findByMAC(sysClassNet, c.MAC)
	case c.PCIVendor != "" || c.PCIDevice != "":
		if c.PCIVendor == "" || c.PCIDevice == "" {
			return "", fmt.Errorf("ifdiscover: pciVendor and pciDevice must both be set (got pciVendor=%q pciDevice=%q)", c.PCIVendor, c.PCIDevice)
		}
		return findByPCI(sysBusPCIDevices, c.PCIVendor, c.PCIDevice)
	default:
		return findFirstWireless(sysClassNet)
	}
}

// ResolveIfName returns ifname directly if non-empty -- config.yaml's
// manual "ifname:" override, highest priority, skips discovery
// entirely. Otherwise resolves mac/pciVendor/pciDevice against the REAL
// filesystem ("/sys/class/net", "/sys/bus/pci/devices") via Find,
// falling back to auto mode (Criteria{}) when none of the three are set
// either -- see Find's doc comment for what "auto" picks. This is the
// function production code (internal/checks, cmd/mseg-tester) actually
// calls; Find itself stays fully unit-testable against fixture paths.
func ResolveIfName(ifname, mac, pciVendor, pciDevice string) (string, error) {
	if ifname != "" {
		return ifname, nil
	}
	return Find("/sys/class/net", "/sys/bus/pci/devices", Criteria{MAC: mac, PCIVendor: pciVendor, PCIDevice: pciDevice})
}

func findByMAC(sysClassNet, mac string) (string, error) {
	names, err := listInterfaces(sysClassNet)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(mac)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(sysClassNet, name, "address"))
		if err != nil {
			continue // no address file, or it vanished mid-scan -- skip, not fatal
		}
		if strings.ToLower(strings.TrimSpace(string(b))) == want {
			return name, nil
		}
	}
	return "", fmt.Errorf("ifdiscover: no interface under %s has MAC address %s", sysClassNet, mac)
}

func findByPCI(sysBusPCIDevices, vendor, device string) (string, error) {
	entries, err := os.ReadDir(sysBusPCIDevices)
	if err != nil {
		return "", fmt.Errorf("ifdiscover: reading %s: %w", sysBusPCIDevices, err)
	}
	wantVendor, wantDevice := normalizeHex(vendor), normalizeHex(device)
	for _, e := range entries {
		devDir := filepath.Join(sysBusPCIDevices, e.Name())
		gotVendor, err := readHexFile(filepath.Join(devDir, "vendor"))
		if err != nil || gotVendor != wantVendor {
			continue
		}
		gotDevice, err := readHexFile(filepath.Join(devDir, "device"))
		if err != nil || gotDevice != wantDevice {
			continue
		}
		netEntries, err := os.ReadDir(filepath.Join(devDir, "net"))
		if err != nil || len(netEntries) == 0 {
			return "", fmt.Errorf("ifdiscover: PCI device %s (vendor=%s device=%s) found, but nothing is bound to its net/ subdirectory yet (driver still loading?)", e.Name(), vendor, device)
		}
		return netEntries[0].Name(), nil
	}
	return "", fmt.Errorf("ifdiscover: no PCI device with vendor=%s device=%s found under %s", vendor, device, sysBusPCIDevices)
}

func findFirstWireless(sysClassNet string) (string, error) {
	names, err := ListWireless(sysClassNet)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("ifdiscover: no Wi-Fi-capable interface found under %s (no \"phy80211\" symlink or \"wireless\" subdirectory on any interface)", sysClassNet)
	}
	return names[0], nil
}

// ListWireless returns every Wi-Fi-capable interface name under
// sysClassNet -- same detection Find's "auto" strategy uses (a
// "phy80211" symlink, or a "wireless" subdirectory -- either indicates a
// radio; not every kernel/driver exposes both), excluding "lo", sorted
// for a deterministic order across boots. An empty, non-error result
// means "no Wi-Fi hardware present at all", a perfectly normal case most
// callers don't need to treat specially.
//
// Used by cmd/mseg-tester to know which interface(s) to explicitly force
// administratively DOWN in netplan on any segment that isn't itself
// "wifi" (see internal/netplan.Render's disableWifiIfaces parameter) --
// necessary because a passed-through radio's kernel network device
// exists as soon as its driver binds, independent of whether netplan
// ever declares or associates it, which otherwise leaves it idle-but-
// present rather than genuinely off (confirmed live: this broke
// internal/ifaces.Find's interface-counting heuristic once passthrough
// Wi-Fi actually started working -- see that package's doc comment).
func ListWireless(sysClassNet string) ([]string, error) {
	names, err := listInterfaces(sysClassNet)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if name == "lo" {
			continue
		}
		dir := filepath.Join(sysClassNet, name)
		if _, err := os.Lstat(filepath.Join(dir, "phy80211")); err == nil {
			out = append(out, name)
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, "wireless")); err == nil && fi.IsDir() {
			out = append(out, name)
		}
	}
	return out, nil
}

func listInterfaces(sysClassNet string) ([]string, error) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, fmt.Errorf("ifdiscover: reading %s: %w", sysClassNet, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func readHexFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return normalizeHex(string(b)), nil
}

// normalizeHex lowercases s, trims surrounding whitespace, and strips a
// leading "0x" if present -- PCI sysfs vendor/device files read like
// "0x14c3\n"; config.yaml's pciVendor/pciDevice are written without the
// prefix (e.g. "14c3"), but this accepts either form from either source.
func normalizeHex(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "0x")
}
