// Package config reads config.yaml -- the CONTENT-level, frequently-tuned
// half of this tool's configuration: segment list, per-segment test
// targets, reboot timing, where to report results. Fetched from the
// private repo named in bootstrap.yaml (see internal/configsync) rather
// than provisioned once by cloud-init -- changing any of this is a commit
// to that private repo, not a VM rebuild.
//
// The other, rarely-changing half -- which local NIC is the trunk, which
// segment can reach the internet, where the two repos are -- lives in
// internal/bootstrap instead, since those facts have to be known BEFORE
// this file can even be fetched.
//
// Load expands "${VAR}" references in the raw file text (internal/envfile)
// before parsing it as YAML -- lets a value like report.influx.token be
// written as "${INFLUX_TOKEN}" instead of a literal secret, so config.yaml
// itself stays safe to keep in a shared or even public repo (see
// bootstrap.Bootstrap.EnvFile) while the real value lives in a small,
// local-only .env file instead.
package config

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mabels/mseg-tester/internal/envfile"
	"gopkg.in/yaml.v3"
)

// GeoCheck optionally confirms a segment resolves/egresses as if it were
// physically in a particular place -- the same concern the "It's always
// DNS" write-up raised: a WireGuard-exit segment should look like it's
// really in that region, not just that DNS answered at all. Expect is
// matched as a case-insensitive substring of the response body; leave it
// empty to just confirm the request succeeds without checking WHERE it
// looks like it came from.
type GeoCheck struct {
	URL    string `yaml:"url"`
	Expect string `yaml:"expect,omitempty"`
}

// DNSTest is one DNS check to run -- see DNSCheck.Tests. Type selects
// which kind of test this is, which address family it forces (regardless
// of which server in Servers/DNSCheck.Servers ends up answering it), and
// which of Host/Domain apply:
//
//   - "A" / "AAAA": Host is forward-resolved (A or AAAA respectively) and
//     the check passes as soon as at least one answer comes back -- no
//     round trip, no expected value, just "does this name resolve".
//   - "A-PTR" / "AAAA-PTR": Host is forward-resolved (A for "A-PTR", AAAA
//     for "AAAA-PTR"), the first answer is reverse-resolved (PTR), and
//     the PTR name is expected to match Host -- a full forward-confirmed
//     reverse DNS (FCrDNS) round trip against a fixed, known name (e.g. a
//     segment's own gateway). Replaces what used to be
//     Segment.ReverseCheck/ReverseCheck6.
//   - "Hostname4" / "Hostname6": Domain (no Host) -- the exact same
//     FCrDNS round trip as "A-PTR"/"AAAA-PTR" (forcing A or AAAA
//     respectively), except against THIS running VM's own dynamically-
//     registered name (os.Hostname(), whatever DHCP told the resolver to
//     register) under Domain, instead of a fixed name -- proving dynamic
//     DNS registration (both directions) actually works for this
//     specific roaming host. Replaces what used to be a separate
//     Segment.SelfDNSDomain field. "Hostname6" exists for symmetry but
//     isn't used anywhere yet -- this network has no IPv6 DHCP, so
//     there's no dynamic AAAA/PTR6 registration to round-trip yet.
//
// Every test runs once per server named in Servers (falling back to
// DNSCheck.Servers if Servers is empty here) -- see DNSCheck's doc
// comment for what "system"/"ipv4"/"ipv6" each mean. Servers only ever
// picks WHICH RESOLVER answers; the address family tested is always
// fixed by Type, so e.g. an "A-PTR" test still forces an IPv4 round trip
// even when run against the "ipv6" server.
type DNSTest struct {
	Type string `yaml:"type"`
	// Host is required for "A", "AAAA", "A-PTR", and "AAAA-PTR" -- the
	// name to test/round-trip.
	Host string `yaml:"host,omitempty"`
	// Domain is required for "Hostname4"/"Hostname6" -- just the domain
	// (e.g. "mam-hh-dmz.adviser.com."), no hostname; os.Hostname()
	// supplies that part at check time.
	Domain string `yaml:"domain,omitempty"`
	// Servers OPTIONALLY overrides DNSCheck.Servers for just this one
	// test. Nil/empty means "use DNSCheck.Servers".
	Servers []string `yaml:"servers,omitempty"`
}

// DNSCheck is a segment's DNS test configuration: every test in Tests
// runs once per entry named in Servers (or that test's own Servers
// override) -- each entry is either:
//
//   - "system": the OS's default resolver (whatever /etc/resolv.conf
//     points at -- normally this segment's own resolver too, via DHCP),
//     proving plain unconfigured resolution works.
//   - a literal IP address (v4 or v6, e.g. "192.168.130.5",
//     "fd00:192:168:130::5", or a public resolver like "1.1.1.1"),
//     dialed directly on port 53 (bypassing /etc/resolv.conf) -- proves
//     THIS specific server answers, regardless of what the OS ended up
//     with via DHCP. Any address reachable from this segment works, not
//     just its own local resolver -- list as many as you want checked.
//
// At least one of Servers required (or every test must set its own).
type DNSCheck struct {
	Servers []string  `yaml:"servers,omitempty"`
	Tests   []DNSTest `yaml:"tests"`
}

// Segment is one entry in the cycle -- everything needed to configure
// netplan for it and to check it once it's up.
type Segment struct {
	// Name is both this segment's cycle identifier AND the VLAN ID,
	// e.g. "130" -- kept as one field since ovn-fabric's own segments
	// already use the VLAN ID as the name (see topology.ts), and there's
	// no reason for this tool to invent a second identifier scheme.
	Name string `yaml:"name"`
	// Type is "native" (this trunk's untagged/native VLAN -- its traffic
	// arrives directly on the trunk interface, no 802.1Q tag at all),
	// "vlan" (a normal 802.1Q-tagged sub-interface, the common case), or
	// "wifi" (a dedicated Wi-Fi radio passed through to this VM,
	// associating to SSID/PSK below instead of carrying any VLAN at all).
	// Required on every segment: at most one may be "native" (a trunk has
	// at most one native VLAN), enforced by Load. Drives interface naming
	// (internal/netplan.IfaceName) AND, for cmd/verify-mseg-tester, which
	// VLAN becomes the Proxmox NIC's "tag=" vs. "trunks=" (wifi segments
	// don't participate in that at all -- they're not on the trunk NIC)
	// -- this field is the single source of truth for "which segment is
	// native," replacing what used to be a separate, hand-kept
	// bootstrap.yaml field of the same meaning.
	Type string `yaml:"type"`
	// IfName overrides the interface name internal/netplan.IfaceName
	// would otherwise derive (normally "<trunkInterface>" for the native
	// segment, "<trunkInterface>.<Name>" for a tagged one). OPTIONAL for
	// "native"/"vlan" -- only needed if your naming convention differs.
	//
	// For "wifi", there's no trunk-derived default at all, so this is one
	// of THREE ways to identify the radio (internal/ifdiscover.Find),
	// checked in this priority order: IfName (a literal name, e.g.
	// "wlan0" -- skips discovery entirely, but only stays correct until
	// something reshuffles this VM's virtual PCI slot numbering and udev
	// renames it); MAC (a specific card's hardware address -- stable
	// across that, since it's a fact about the physical card, not this
	// boot's virtual bus enumeration); PCIVendor+PCIDevice (ditto, via
	// vendor:device ID instead of MAC); or, if none of the three are set,
	// "auto" -- the first Wi-Fi-capable interface found at all. None are
	// required; a "wifi" segment with all four unset just means "auto".
	IfName string `yaml:"ifname,omitempty"`
	// MAC is one of three OPTIONAL ways to identify a "wifi" segment's
	// radio when IfName isn't set -- see IfName's doc comment for the
	// full priority order. The interface's hardware address, e.g.
	// "90:7a:be:dc:34:a9" (as shown by `ip link` on whatever host the
	// physical card was in before passthrough, or on the guest once it's
	// up once).
	MAC string `yaml:"mac,omitempty"`
	// PCIVendor and PCIDevice are the other OPTIONAL identification
	// method -- see IfName's doc comment. Both must be set together (Load
	// rejects just one) -- the PCI vendor:device ID pair of the physical
	// card, hex, with or without a "0x" prefix, e.g. "14c3"/"0616" for a
	// MediaTek MT7922 (`lspci -nnk` on the Proxmox host, before
	// passthrough, shows this as "[14c3:0616]").
	PCIVendor string `yaml:"pciVendor,omitempty"`
	PCIDevice string `yaml:"pciDevice,omitempty"`
	// SSID is REQUIRED when Type is "wifi" (ignored otherwise) -- the
	// network name internal/netplan.Render's wifi branch associates to.
	SSID string `yaml:"ssid,omitempty"`
	// PSK is REQUIRED when Type is "wifi" (ignored otherwise) -- the
	// network's password. Written as "${VAR}" (e.g. "${WIFI_128_PSK}")
	// and expanded at Load time the same way report.influx.token is --
	// see Load's doc comment and internal/envfile -- so the literal
	// secret lives only in a local .env file, never in config.yaml
	// itself. Convention used throughout this project's own config:
	// WIFI_<segment name>_SSID / WIFI_<segment name>_PSK, e.g.
	// WIFI_128_SSID/WIFI_128_PSK for the segment named "128"'s Wi-Fi
	// counterpart -- not enforced by code, just a naming convention that
	// keeps each pair unambiguous at a glance in .env.
	PSK string `yaml:"psk,omitempty"`
	// DNSCheck configures every DNS test for this segment -- forward
	// lookups, gateway PTR round trips, and this VM's own dynamic-DNS
	// round trip are all just different DNSTest.Type values in one flat
	// list now (see DNSCheck/DNSTest above and internal/checks). Optional:
	// nil runs no DNS checks at all for this segment.
	DNSCheck *DNSCheck `yaml:"dnsCheck,omitempty"`
	// GeoCheck is optional -- see GeoCheck above. Nil skips it.
	GeoCheck *GeoCheck `yaml:"geoCheck,omitempty"`
	// RoutingCheck is a plain external address/host expected to be
	// reachable (TCP-dial or ICMP -- see internal/checks) -- confirms
	// this segment's egress uplink actually carries traffic, not just
	// that DNS resolved.
	RoutingCheck string `yaml:"routingCheck"`
	// RoutingCheck6 is OPTIONAL -- an IPv6 equivalent of RoutingCheck,
	// e.g. "[2606:4700:4700::1111]:443" (brackets required, same as any
	// Go "host:port" literal with an IPv6 host). The "routing6" check
	// is skipped when this is empty.
	RoutingCheck6 string `yaml:"routingCheck6,omitempty"`
}

// InfluxReport optionally sends every accumulated result straight into an
// InfluxDB v2 bucket via its line-protocol /api/v2/write API -- an
// alternative to (or alongside) Report.URL's generic JSON webhook, handy
// when there's already an InfluxDB instance collecting everything else
// on the network and no separate collector is worth standing up just
// for this. See internal/report.PushInflux.
type InfluxReport struct {
	// URL is the InfluxDB v2 base URL, e.g. "https://mam-influx.adviser.com"
	// -- no trailing slash needed, "/api/v2/write" is appended automatically.
	URL string `yaml:"url"`
	// Org is the InfluxDB organization name, e.g. "homeassistant".
	Org string `yaml:"org"`
	// Bucket is the InfluxDB bucket name to write into.
	Bucket string `yaml:"bucket"`
	// Token is an API token scoped to WRITE-ONLY access on Bucket --
	// create one with `influx auth create --write-bucket <bucket-id>`,
	// never the InfluxDB admin/all-access token: this value lives in
	// plaintext in config.yaml on every VM in the cycle, the same trust
	// level as bootstrap.yaml's configToken, and a write-only,
	// single-bucket token is all that's ever needed here.
	Token string `yaml:"token"`
}

// Report is where (and how) to send accumulated results -- only ever
// attempted while the active segment is bootstrap.Bootstrap.UpdateSegment
// (the one segment that can reach anywhere outside the segment being
// tested), same reachability constraint as self-update and config-sync.
type Report struct {
	// URL results are POSTed to, as a JSON array of state.Result -- e.g.
	// any small webhook/collector willing to accept that shape. Optional
	// -- empty skips this path (may be used alongside Influx below, or
	// left empty if Influx is set and nothing else needs the raw JSON).
	URL string `yaml:"url,omitempty"`
	// Influx is OPTIONAL -- see InfluxReport. Nil skips it.
	Influx *InfluxReport `yaml:"influx,omitempty"`
}

// Config is the content-level configuration fetched from the private repo
// (see package doc above) -- everything in here is safe to change on its
// own schedule, independent of any bootstrap.yaml/VM-level fact.
type Config struct {
	// RebootDelay is how long to wait, on EVERY segment, after a test
	// completes (and, on updateSegment only, after that segment's
	// self-update/config-sync/report) before actually rebooting into the
	// next segment -- e.g. "2m". Deliberately applies everywhere (not
	// just updateSegment, as originally designed) so there's a real
	// window to SSH/console into the box and inspect it before it cycles
	// away, on whatever segment turns out to need debugging -- a slow or
	// hung boot isn't necessarily on updateSegment. The cost: this is now
	// paid once PER SEGMENT, not once per full cycle -- a full cycle's
	// wall-clock time grows by roughly (segment count x RebootDelay), so
	// pick a value that's fine to pay that many times, not just once.
	// Empty/zero means immediate reboot everywhere (the old default). A
	// Go duration string (time.ParseDuration).
	RebootDelay string `yaml:"rebootDelay,omitempty"`
	// CheckAttempts is how many times to run the WHOLE batch of checks
	// (dhcp, dhcp6, every dns pass, reverse, geo, routing, ...) before
	// giving up -- defaults to 3 if zero/unset. If ANY check in a batch
	// fails, the entire batch is re-run from scratch after CheckRetryDelay
	// (not just the failing check -- a single bad attempt is retried as a
	// whole, since check results are more meaningful read together than
	// individually re-run at different points in time). Stops as soon as
	// one whole batch passes every check; if every attempt still has a
	// failure, the LAST attempt's results are what's recorded.
	CheckAttempts int `yaml:"checkAttempts,omitempty"`
	// CheckRetryDelay is how long to wait before re-running the whole
	// batch of checks after any one of them failed -- defaults to "10s"
	// if empty. A Go duration string. Has nothing to do with RebootDelay
	// (that's once per segment, before rebooting into the next one; this
	// is per checks batch, before even recording a result).
	CheckRetryDelay string `yaml:"checkRetryDelay,omitempty"`
	// Timezone is OPTIONAL -- an IANA zone name (e.g. "Europe/Berlin",
	// "America/New_York") applied via `timedatectl set-timezone` at the
	// start of every run (see cmd/mseg-tester's applyTimezone) --
	// idempotent, so a no-op once already set. Lives here rather than in
	// bootstrap.yaml/cloud-init deliberately: it's content, not a
	// VM-level fact, so it can be changed the same way as any other
	// config.yaml value (edit and re-provision on the easy path, or a
	// commit to configRepo on the synced path) without recreating the
	// VM. Empty (the default) leaves whatever timezone the base Ubuntu
	// Server image already has (normally UTC) untouched. Applying this
	// is best-effort: an invalid zone name is logged, not fatal --
	// wrong/missing timezone shouldn't block the actual network checks.
	Timezone string    `yaml:"timezone,omitempty"`
	Report   *Report   `yaml:"report,omitempty"`
	Segments []Segment `yaml:"segments"`
}

// RebootDelayDuration parses RebootDelay, defaulting to 0 (immediate) for
// an empty string or a value that fails to parse -- a malformed delay
// shouldn't block the whole cycle from ever completing.
func (c Config) RebootDelayDuration() time.Duration {
	if c.RebootDelay == "" {
		return 0
	}
	d, err := time.ParseDuration(c.RebootDelay)
	if err != nil {
		return 0
	}
	return d
}

// CheckAttemptsOrDefault returns CheckAttempts, defaulting to 3 for
// zero/negative (unset, or a malformed/nonsensical config.yaml value).
func (c Config) CheckAttemptsOrDefault() int {
	if c.CheckAttempts <= 0 {
		return 3
	}
	return c.CheckAttempts
}

// CheckRetryDelayOrDefault parses CheckRetryDelay, defaulting to 10s for
// empty, malformed, or non-positive values.
func (c Config) CheckRetryDelayOrDefault() time.Duration {
	if c.CheckRetryDelay == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(c.CheckRetryDelay)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

// BySegmentName finds a Segment by its Name/VLAN ID, or ok=false if this
// config doesn't declare one -- e.g. active.yaml pointing at a segment
// that was since removed from config.yaml.
func (c Config) BySegmentName(name string) (Segment, bool) {
	for _, s := range c.Segments {
		if s.Name == name {
			return s, true
		}
	}
	return Segment{}, false
}

// CycleNames returns every declared segment's Name, in the order given in
// config.yaml -- used to seed active.yaml on first boot. Deliberately
// includes "wifi" segments alongside "native"/"vlan" ones: the reboot
// cycle itself doesn't care what carries a segment's traffic, only
// internal/netplan.Render (branching on Type) and cmd/verify-mseg-tester's
// Proxmox trunk derivation (VLANSegmentNames below) do.
func (c Config) CycleNames() []string {
	names := make([]string, len(c.Segments))
	for i, s := range c.Segments {
		names[i] = s.Name
	}
	return names
}

// VLANSegmentNames returns every "native"/"vlan" segment's Name, in
// declaration order, EXCLUDING "wifi" segments -- used by
// cmd/verify-mseg-tester to derive the Proxmox trunk NIC's VLAN list
// (Params.TrunkVLANs). A "wifi" segment's Name isn't a VLAN ID at all (it
// never rides the trunk NIC -- see config.Segment.Type's doc comment), so
// including it in "trunks=" would be meaningless at best and a Proxmox
// error at worst. Use CycleNames instead wherever every segment
// (including wifi) genuinely belongs, e.g. seeding active.yaml's cycle.
func (c Config) VLANSegmentNames() []string {
	var names []string
	for _, s := range c.Segments {
		if s.Type == "wifi" {
			continue
		}
		names = append(names, s.Name)
	}
	return names
}

// Load reads and parses path, expanding any "${VAR}" references against
// envVars first (see envfile.Expand -- a nil/empty map still falls back
// to the real process environment for each reference, so this works even
// when no .env file exists at all). Pass bootstrap.Bootstrap.EnvFile's
// contents (envfile.Load) as envVars; pass nil if there's no env file to
// load at all (e.g. cmd/verify-mseg-tester deriving VLANs from a local
// -config-file before any VM, let alone its .env, exists).
func Load(path string, envVars map[string]string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("config: reading %s: %w", path, err)
	}
	expanded := envfile.Expand(string(b), envVars)
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return c, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if len(c.Segments) == 0 {
		return c, fmt.Errorf("config: %s declares no segments", path)
	}
	natives := 0
	for _, s := range c.Segments {
		switch s.Type {
		case "native":
			natives++
		case "vlan":
			// fine
		case "wifi":
			// ifname/mac/pciVendor+pciDevice are all optional -- see
			// Segment.IfName's doc comment for the priority order and
			// what leaving all four unset ("auto") means
			// (internal/ifdiscover.Find). The one thing actually invalid
			// here is a PARTIAL pciVendor/pciDevice pair -- that's not
			// "fall back to auto", it's almost certainly a typo/omission
			// worth failing on now rather than silently auto-discovering
			// the wrong (or a different) radio.
			if (s.PCIVendor == "") != (s.PCIDevice == "") {
				return c, fmt.Errorf("config: %s: segment %q: pciVendor and pciDevice must both be set, or neither (got pciVendor=%q pciDevice=%q)", path, s.Name, s.PCIVendor, s.PCIDevice)
			}
			if s.SSID == "" {
				return c, fmt.Errorf("config: %s: segment %q: type \"wifi\" requires ssid", path, s.Name)
			}
			if s.PSK == "" {
				return c, fmt.Errorf("config: %s: segment %q: type \"wifi\" requires psk", path, s.Name)
			}
		default:
			return c, fmt.Errorf("config: %s: segment %q has invalid type %q (must be \"native\", \"vlan\", or \"wifi\")", path, s.Name, s.Type)
		}
		if err := validateDNSCheck(path, s.Name, s.DNSCheck); err != nil {
			return c, err
		}
	}
	if natives > 1 {
		return c, fmt.Errorf("config: %s: %d segments declared type \"native\" -- a trunk has at most one native/untagged VLAN", path, natives)
	}
	return c, nil
}

// validServerEntry reports whether entry is a valid DNSCheck.Servers (or
// DNSTest.Servers) entry -- either the literal "system", or a syntactically
// valid IP address (v4 or v6) to dial directly.
func validServerEntry(entry string) bool {
	if entry == "system" {
		return true
	}
	return net.ParseIP(entry) != nil
}

// validateDNSCheck fails fast on a malformed DNSCheck -- an invalid
// server entry or DNSTest.Type, or a test missing the field its Type
// requires -- rather than letting it surface later as a confusing
// runtime check failure that looks like a network problem.
func validateDNSCheck(path, segName string, dc *DNSCheck) error {
	if dc == nil {
		return nil
	}
	for _, entry := range dc.Servers {
		if !validServerEntry(entry) {
			return fmt.Errorf("config: %s: segment %q: dnsCheck.servers: invalid entry %q (must be \"system\" or a literal IP address)", path, segName, entry)
		}
	}
	for _, t := range dc.Tests {
		for _, entry := range t.Servers {
			if !validServerEntry(entry) {
				return fmt.Errorf("config: %s: segment %q: dnsCheck test %q: invalid server %q (must be \"system\" or a literal IP address)", path, segName, t.Type, entry)
			}
		}
		if len(t.Servers) == 0 && len(dc.Servers) == 0 {
			return fmt.Errorf("config: %s: segment %q: dnsCheck test %q: no servers -- set dnsCheck.servers or this test's own servers", path, segName, t.Type)
		}
		switch t.Type {
		case "A", "AAAA", "A-PTR", "AAAA-PTR":
			if t.Host == "" {
				return fmt.Errorf("config: %s: segment %q: dnsCheck test type %q requires host", path, segName, t.Type)
			}
		case "Hostname4", "Hostname6":
			if t.Domain == "" {
				return fmt.Errorf("config: %s: segment %q: dnsCheck test type %q requires domain", path, segName, t.Type)
			}
		default:
			return fmt.Errorf("config: %s: segment %q: dnsCheck test has invalid type %q (must be \"A\", \"AAAA\", \"A-PTR\", \"AAAA-PTR\", \"Hostname4\", or \"Hostname6\")", path, segName, t.Type)
		}
	}
	return nil
}

// NativeSegmentName returns the one segment whose Type is "native", or
// ""/false if every segment is a normal tagged VLAN. Load already
// guarantees at most one exists.
func (c Config) NativeSegmentName() (string, bool) {
	for _, s := range c.Segments {
		if s.Type == "native" {
			return s.Name, true
		}
	}
	return "", false
}

// WifiPCIDevices returns the deduplicated set of "vendor:device" PCI ID
// pairs declared by this config's "wifi" segments' PCIVendor/PCIDevice,
// in first-seen order -- used by cmd/verify-mseg-tester to derive which
// PCI devices a disposable VM's `create` should passthrough
// (Params.HostPCIDevices), so the same config.yaml that identifies a
// wifi segment's radio INSIDE the guest also drives which physical card
// gets attached in the first place, rather than needing that PCI ID
// looked up and typed a second time by hand.
//
// Segments identifying their radio by IfName/MAC instead, or by neither
// (auto-discovery), contribute nothing here: there's no PCI ID to look
// up a host address for -- IfName/MAC-based discovery (and auto)
// presume the device is already passed through by the time the guest
// runs, they don't say which physical device that should be. Dedup
// matters because this project's own segments (wifi-128/130/131) all
// currently share ONE physical radio -- each would otherwise ask for the
// same device to be passed through three times.
func (c Config) WifiPCIDevices() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Segments {
		if s.Type != "wifi" || s.PCIVendor == "" || s.PCIDevice == "" {
			continue
		}
		key := s.PCIVendor + ":" + s.PCIDevice
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}
