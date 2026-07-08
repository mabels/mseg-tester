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

// Segment is one entry in the cycle -- everything needed to configure
// netplan for it and to check it once it's up.
type Segment struct {
	// Name is both this segment's cycle identifier AND the VLAN ID,
	// e.g. "130" -- kept as one field since ovn-fabric's own segments
	// already use the VLAN ID as the name (see topology.ts), and there's
	// no reason for this tool to invent a second identifier scheme.
	Name string `yaml:"name"`
	// DNSServer is the segment-local resolver to query directly (its
	// own Technitium instance, typically <subnet>.5).
	DNSServer string `yaml:"dnsServer"`
	// DNSCheck is a record expected to resolve against DNSServer --
	// confirms the resolver itself is answering, independent of
	// upstream/internet reachability.
	DNSCheck string `yaml:"dnsCheck"`
	// DNSServer6 is OPTIONAL -- the same resolver's IPv6 address (e.g.
	// "fd00:192:168:129::5", the convention this project's other
	// cloud-init files already use for their own IPv6 nameserver entries
	// -- plain address, no brackets, same style as DNSServer). The
	// "dns6" check (the same DNSCheck record, reached over IPv6) is
	// skipped when this is empty.
	DNSServer6 string `yaml:"dnsServer6,omitempty"`
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
	// RebootDelay is how long to wait after a test completes (and after
	// any self-update/config-sync/report on the update segment) before
	// actually rebooting into the next segment -- e.g. "10s". Empty/zero
	// means reboot immediately. A Go duration string (time.ParseDuration).
	RebootDelay string    `yaml:"rebootDelay,omitempty"`
	Report      *Report   `yaml:"report,omitempty"`
	Segments    []Segment `yaml:"segments"`
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
	return c, nil
}
