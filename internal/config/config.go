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
package config

import (
	"fmt"
	"os"
	"time"

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

// ReverseCheck optionally confirms this segment's resolver also answers
// PTR queries correctly (rDNS) -- IP is expected to reverse-resolve to
// Expect (a FQDN; trailing dot recommended to match how
// net.Resolver.LookupAddr returns names). This is a genuinely separate
// failure mode from forward resolution (DNSCheck): a resolver can answer
// forward queries fine while its PTR zone is stale, unconfigured, or
// simply wrong, and rDNS is what a surprising number of other systems
// (mail, some VPN/geo services, logging) quietly depend on.
type ReverseCheck struct {
	IP     string `yaml:"ip"`
	Expect string `yaml:"expect"`
}

// DNSCheckGroup is one named group of forward-DNS lookups to run
// together -- see DNSCheck and internal/checks.
type DNSCheckGroup struct {
	// Server is OPTIONAL and means something different in each group:
	//   - Local group: leave empty to query BOTH of this segment's own
	//     resolver addresses (Segment.DNSServer over IPv4, and
	//     Segment.DNSServer6 over IPv6 too if that's set) -- every test
	//     below is then run against each family in turn. Set Server to
	//     query only that one specific address instead.
	//   - Remote group: leave empty to use the SYSTEM's default resolver
	//     (whatever /etc/resolv.conf points at -- normally this segment's
	//     own resolver too, via DHCP) -- proving plain, unconfigured
	//     resolution works. Set Server to dial a specific address instead
	//     (e.g. a public resolver like "1.1.1.1").
	Server string `yaml:"server,omitempty"`
	// Tests is every FQDN to resolve in this group, e.g. a segment's own
	// record alongside a public internet one, to prove the resolver also
	// forwards upstream and not just answers its own local zone.
	Tests []string `yaml:"tests"`
}

// DNSCheck is a segment's forward-DNS test configuration -- Local and
// Remote are each optional (nil skips that group entirely), so a segment
// can run either, both, or neither. See DNSCheckGroup for what "local"
// and "remote" mean for Server's default.
type DNSCheck struct {
	Local  *DNSCheckGroup `yaml:"local,omitempty"`
	Remote *DNSCheckGroup `yaml:"remote,omitempty"`
}

// Segment is one entry in the cycle -- everything needed to configure
// netplan for it and to check it once it's up.
type Segment struct {
	// Name is both this segment's cycle identifier AND the VLAN ID,
	// e.g. "130" -- kept as one field since ovn-fabric's own segments
	// already use the VLAN ID as the name (see topology.ts), and there's
	// no reason for this tool to invent a second identifier scheme.
	Name string `yaml:"name"`
	// Type is either "native" (this trunk's untagged/native VLAN -- its
	// traffic arrives directly on the trunk interface, no 802.1Q tag at
	// all) or "vlan" (a normal 802.1Q-tagged sub-interface, the common
	// case). Required on every segment: at most one may be "native" (a
	// trunk has at most one native VLAN), enforced by Load. Drives
	// interface naming (internal/netplan.IfaceName) AND, for
	// cmd/verify-mseg-tester, which VLAN becomes the Proxmox NIC's
	// "tag=" vs. "trunks=" -- this field is the single source of truth
	// for "which segment is native," replacing what used to be a
	// separate, hand-kept bootstrap.yaml field of the same meaning.
	Type string `yaml:"type"`
	// IfName is OPTIONAL -- overrides the interface name
	// internal/netplan.IfaceName would otherwise derive (normally
	// "<trunkInterface>" for the native segment, "<trunkInterface>.<Name>"
	// for a tagged one). Only needed if your naming convention differs.
	IfName string `yaml:"ifname,omitempty"`
	// DNSServer is the segment-local resolver to query directly (its
	// own Technitium instance, typically <subnet>.5) -- also DNSCheck's
	// Local group's default server over IPv4 when that group doesn't
	// override Server itself.
	DNSServer string `yaml:"dnsServer"`
	// DNSServer6 is OPTIONAL -- the same resolver's IPv6 address (e.g.
	// "fd00:192:168:129::5", the convention this project's other
	// cloud-init files already use for their own IPv6 nameserver entries
	// -- plain address, no brackets, same style as DNSServer). Also
	// DNSCheck's Local group's default server over IPv6 -- skipped
	// (like ReverseCheck6) when this is empty.
	DNSServer6 string `yaml:"dnsServer6,omitempty"`
	// DNSCheck configures this segment's forward-DNS tests -- see
	// DNSCheck/DNSCheckGroup above and internal/checks. Optional: nil
	// runs no forward-DNS checks at all for this segment.
	DNSCheck *DNSCheck `yaml:"dnsCheck,omitempty"`
	// ReverseCheck is OPTIONAL -- see ReverseCheck above. Queried against
	// DNSServer (IPv4 transport). Nil skips the "reverse" check.
	ReverseCheck *ReverseCheck `yaml:"reverseCheck,omitempty"`
	// ReverseCheck6 is OPTIONAL -- ReverseCheck's IPv6 counterpart,
	// queried against DNSServer6. Skipped when DNSServer6 is empty, even
	// if this is set.
	ReverseCheck6 *ReverseCheck `yaml:"reverseCheck6,omitempty"`
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

// Report is where (and how) to send accumulated results -- only ever
// attempted while the active segment is bootstrap.Bootstrap.UpdateSegment
// (the one segment that can reach anywhere outside the segment being
// tested), same reachability constraint as self-update and config-sync.
type Report struct {
	// URL results are POSTed to, as a JSON array of state.Result -- e.g.
	// a Prometheus Pushgateway, or any small webhook/collector willing
	// to accept that shape. Empty disables reporting entirely.
	URL string `yaml:"url"`
}

// Config is the content-level configuration fetched from the private repo
// (see package doc above) -- everything in here is safe to change on its
// own schedule, independent of any bootstrap.yaml/VM-level fact.
type Config struct {
	// RebootDelay is how long to wait, ONLY on updateSegment, after a
	// test completes (and after that segment's self-update/config-sync/
	// report) before actually rebooting into the next segment -- e.g.
	// "6m". Every other segment has nothing to wait for (none of that
	// happens there) and always reboots immediately regardless of this
	// value, so the delay is paid once per full cycle, not once per
	// segment. Empty/zero means updateSegment reboots immediately too. A
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
	// (that's once per cycle, on updateSegment only; this is per checks
	// batch, on every segment).
	CheckRetryDelay string    `yaml:"checkRetryDelay,omitempty"`
	Report          *Report   `yaml:"report,omitempty"`
	Segments        []Segment `yaml:"segments"`
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
// config.yaml -- used to seed active.yaml on first boot.
func (c Config) CycleNames() []string {
	names := make([]string, len(c.Segments))
	for i, s := range c.Segments {
		names[i] = s.Name
	}
	return names
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("config: reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
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
		default:
			return c, fmt.Errorf("config: %s: segment %q has invalid type %q (must be \"native\" or \"vlan\")", path, s.Name, s.Type)
		}
	}
	if natives > 1 {
		return c, fmt.Errorf("config: %s: %d segments declared type \"native\" -- a trunk has at most one native/untagged VLAN", path, natives)
	}
	return c, nil
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
