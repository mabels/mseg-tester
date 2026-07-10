package ifdiscover

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile writes content to path, creating parent directories as
// needed -- t.Helper keeps failures pointing at the calling test.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds a fake "/sys/class/net" + "/sys/bus/pci/devices" tree
// under t.TempDir(), modeling: "lo" (never a candidate), "eth0" (a
// plain wired NIC, no wireless markers), and "wlan0" (a Wi-Fi radio,
// bound to a PCI device at pciAddr with the given vendor/device IDs, a
// "phy80211" symlink, and a MAC address) -- close enough to a real
// mt7921e-driven MT7922 card to exercise every Find strategy.
func fixture(t *testing.T) (sysClassNet, sysBusPCIDevices string) {
	t.Helper()
	root := t.TempDir()
	sysClassNet = filepath.Join(root, "class", "net")
	sysBusPCIDevices = filepath.Join(root, "bus", "pci", "devices")

	const pciAddr = "0000:07:00.0"

	// lo: no address/phy80211/wireless at all -- just present, like a
	// real loopback interface's sysfs entry, so Find must skip it
	// deliberately, not just because it lacks markers.
	writeFile(t, filepath.Join(sysClassNet, "lo", "address"), "00:00:00:00:00:00\n")

	// eth0: a plain wired NIC -- has an address, no wireless markers.
	writeFile(t, filepath.Join(sysClassNet, "eth0", "address"), "aa:bb:cc:dd:ee:ff\n")

	// wlan0: the Wi-Fi radio -- address, phy80211 symlink (target
	// doesn't need to exist, Lstat alone is enough), and bound to the
	// PCI device below.
	writeFile(t, filepath.Join(sysClassNet, "wlan0", "address"), "90:7a:be:dc:34:a9\n")
	if err := os.Symlink("../../devices/phy0", filepath.Join(sysClassNet, "wlan0", "phy80211")); err != nil {
		t.Fatal(err)
	}

	// The PCI device wlan0 is bound to.
	writeFile(t, filepath.Join(sysBusPCIDevices, pciAddr, "vendor"), "0x14c3\n")
	writeFile(t, filepath.Join(sysBusPCIDevices, pciAddr, "device"), "0x0616\n")
	writeFile(t, filepath.Join(sysBusPCIDevices, pciAddr, "net", "wlan0"), "")

	// An unrelated PCI device, to prove Find doesn't just grab the first
	// entry in the directory.
	writeFile(t, filepath.Join(sysBusPCIDevices, "0000:05:00.0", "vendor"), "0x8086\n")
	writeFile(t, filepath.Join(sysBusPCIDevices, "0000:05:00.0", "device"), "0x1572\n")

	return sysClassNet, sysBusPCIDevices
}

func TestFindByMAC(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{MAC: "90:7A:BE:DC:34:A9"}) // uppercase -- must match case-insensitively
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("Find = %q, want \"wlan0\"", got)
	}
}

func TestFindByMACNotFound(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	if _, err := Find(sysClassNet, sysBusPCIDevices, Criteria{MAC: "11:22:33:44:55:66"}); err == nil {
		t.Fatal("expected an error for a MAC that matches nothing")
	}
}

func TestFindByPCI(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{PCIVendor: "14c3", PCIDevice: "0616"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("Find = %q, want \"wlan0\"", got)
	}
}

func TestFindByPCIAcceptsHexPrefix(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{PCIVendor: "0x14C3", PCIDevice: "0X0616"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("Find = %q, want \"wlan0\"", got)
	}
}

func TestFindByPCINotFound(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	if _, err := Find(sysClassNet, sysBusPCIDevices, Criteria{PCIVendor: "ffff", PCIDevice: "ffff"}); err == nil {
		t.Fatal("expected an error for a vendor:device pair that matches nothing")
	}
}

func TestFindByPCIPartialPairIsAnError(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	if _, err := Find(sysClassNet, sysBusPCIDevices, Criteria{PCIVendor: "14c3"}); err == nil {
		t.Fatal("expected an error when only pciVendor is set")
	}
	if _, err := Find(sysClassNet, sysBusPCIDevices, Criteria{PCIDevice: "0616"}); err == nil {
		t.Fatal("expected an error when only pciDevice is set")
	}
}

func TestFindAutoPicksFirstWireless(t *testing.T) {
	sysClassNet, sysBusPCIDevices := fixture(t)
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("Find = %q, want \"wlan0\" (the only interface with a phy80211 marker)", got)
	}
}

func TestFindAutoSkipsLoAndPlainEthernet(t *testing.T) {
	// Same fixture as above, but confirms "eth0" (present, has an
	// address, no wireless markers) and "lo" are both correctly passed
	// over rather than accidentally matched.
	sysClassNet, sysBusPCIDevices := fixture(t)
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got == "lo" || got == "eth0" {
		t.Errorf("Find = %q, should never pick lo or a non-wireless interface", got)
	}
}

func TestFindAutoNoWirelessInterface(t *testing.T) {
	root := t.TempDir()
	sysClassNet := filepath.Join(root, "class", "net")
	sysBusPCIDevices := filepath.Join(root, "bus", "pci", "devices")
	writeFile(t, filepath.Join(sysClassNet, "lo", "address"), "00:00:00:00:00:00\n")
	writeFile(t, filepath.Join(sysClassNet, "eth0", "address"), "aa:bb:cc:dd:ee:ff\n")
	if _, err := Find(sysClassNet, sysBusPCIDevices, Criteria{}); err == nil {
		t.Fatal("expected an error when no interface has any wireless marker")
	}
}

func TestFindAutoRecognizesLegacyWirelessDir(t *testing.T) {
	// Some older drivers expose a "wireless/" subdirectory instead of a
	// "phy80211" symlink -- Find should recognize either.
	root := t.TempDir()
	sysClassNet := filepath.Join(root, "class", "net")
	sysBusPCIDevices := filepath.Join(root, "bus", "pci", "devices")
	writeFile(t, filepath.Join(sysClassNet, "lo", "address"), "00:00:00:00:00:00\n")
	if err := os.MkdirAll(filepath.Join(sysClassNet, "wlan0", "wireless"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(sysClassNet, sysBusPCIDevices, Criteria{})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("Find = %q, want \"wlan0\" (legacy wireless/ subdirectory)", got)
	}
}

func TestListWirelessFindsOnlyWirelessInterfaces(t *testing.T) {
	sysClassNet, _ := fixture(t)
	got, err := ListWireless(sysClassNet)
	if err != nil {
		t.Fatalf("ListWireless: %v", err)
	}
	if len(got) != 1 || got[0] != "wlan0" {
		t.Errorf("ListWireless = %v, want [\"wlan0\"] (lo and eth0 have no wireless markers)", got)
	}
}

func TestListWirelessEmptyWhenNoRadioPresent(t *testing.T) {
	root := t.TempDir()
	sysClassNet := filepath.Join(root, "class", "net")
	writeFile(t, filepath.Join(sysClassNet, "lo", "address"), "00:00:00:00:00:00\n")
	writeFile(t, filepath.Join(sysClassNet, "eth0", "address"), "aa:bb:cc:dd:ee:ff\n")
	got, err := ListWireless(sysClassNet)
	if err != nil {
		t.Fatalf("ListWireless: %v (no matching hardware should not be an error)", err)
	}
	if len(got) != 0 {
		t.Errorf("ListWireless = %v, want empty", got)
	}
}

func TestListWirelessSortedAcrossMultipleRadios(t *testing.T) {
	root := t.TempDir()
	sysClassNet := filepath.Join(root, "class", "net")
	writeFile(t, filepath.Join(sysClassNet, "lo", "address"), "00:00:00:00:00:00\n")
	if err := os.MkdirAll(filepath.Join(sysClassNet, "wlan1", "wireless"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysClassNet, "wlan0", "wireless"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ListWireless(sysClassNet)
	if err != nil {
		t.Fatalf("ListWireless: %v", err)
	}
	want := []string{"wlan0", "wlan1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ListWireless = %v, want %v (sorted)", got, want)
	}
}

func TestResolveIfNamePrefersLiteralOverride(t *testing.T) {
	// Must not touch the real filesystem at all when ifname is already
	// set -- this is the one ResolveIfName case safe to run without a
	// fixture (mac/pciVendor/pciDevice below are deliberately bogus and
	// would fail Find if they were ever consulted).
	got, err := ResolveIfName("wlan0", "00:00:00:00:00:00", "ffff", "ffff")
	if err != nil {
		t.Fatalf("ResolveIfName: %v", err)
	}
	if got != "wlan0" {
		t.Errorf("ResolveIfName = %q, want the literal ifname override untouched", got)
	}
}
