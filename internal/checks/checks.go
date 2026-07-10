// Package checks runs the things a segment needs to prove to pass: it
// handed out real IPv4 and IPv6 addresses (DHCP/SLAAC already having run
// via netplan by the time this binary starts -- checked here, not
// performed here), its configured DNS tests resolve/round-trip
// correctly against whichever resolver(s) each is configured to use
// (see config.DNSCheck/DNSTest), and its egress uplink actually carries
// traffic over both families.
//
// IPv6 gets equal billing with IPv4 here deliberately: this network's
// segments are dual-stack (see network-topology.md), and OVN on this
// setup can only ever deliver a SOLICITED router advertisement -- its own
// periodic/unsolicited RA is unconditionally dropped by a compiled
// ovn-northd flow (see the ovn-fabric project's notes on this). A cold
// reboot into a segment always sends a fresh RS on link-up, so this
// tool's own cold-reboot-per-segment cycle happens to be a good, ongoing
// regression check for exactly that failure class -- not just an
// IPv4/IPv6 parity nicety.
package checks

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/ifaces"
	"github.com/mabels/mseg-tester/internal/ifdiscover"
	"github.com/mabels/mseg-tester/internal/state"
)

const timeout = 8 * time.Second

// Run performs every check for one segment, retrying the WHOLE batch (not
// individual checks) up to attempts times if any single check in it
// failed, waiting retryDelay in between -- see config.Config's
// CheckAttemptsOrDefault/CheckRetryDelayOrDefault doc comments for why a
// batch, not a per-check, retry. Stops as soon as one whole batch passes
// every check; the LAST attempt's results are what's returned otherwise.
//
// verbose, if true, logs every check's pass/fail/detail as it completes,
// and each batch retry -- handy when watching a live console rather than
// only reading the final <segment>.result.yaml afterward.
func Run(seg config.Segment, attempts int, retryDelay time.Duration, verbose bool) []state.CheckResult {
	if attempts <= 0 {
		attempts = 1
	}
	var results []state.CheckResult
	for attempt := 1; attempt <= attempts; attempt++ {
		if verbose && attempt > 1 {
			log.Printf("checks: segment %s: attempt %d/%d", seg.Name, attempt, attempts)
		}
		results = runOnceFn(seg, verbose)
		if allPass(results) {
			return results
		}
		if attempt < attempts {
			if verbose {
				log.Printf("checks: segment %s: attempt %d/%d had a failure, waiting %s before re-running every check",
					seg.Name, attempt, attempts, retryDelay)
			}
			time.Sleep(retryDelay)
		}
	}
	return results
}

// allPass reports whether every check in results passed.
func allPass(results []state.CheckResult) bool {
	for _, r := range results {
		if !r.Pass {
			return false
		}
	}
	return true
}

// runOnceFn is a var, not a plain function call, so tests can fake a
// whole attempt's outcome (pass/fail) without needing a real interface,
// DNS server, or network reachability -- same seam pattern as
// internal/selfupdate's goInstall var.
var runOnceFn = runOnce

// runOnce runs every check for seg exactly once, in a fixed order (so
// <segment>.result.yaml reads the same shape every time, easier to diff
// across runs by hand): dhcp, dhcp6, every configured DNSCheck test (see
// dnsChecks), geo if configured, routing, routing6 if configured.
func runOnce(seg config.Segment, verbose bool) []state.CheckResult {
	logged := func(r state.CheckResult) state.CheckResult {
		if verbose {
			status := "FAIL"
			if r.Pass {
				status = "PASS"
			}
			log.Printf("checks: %-16s %s  %s", r.Name, status, r.Detail)
		}
		return r
	}

	iface, ifaceErr := discoverIface(seg)

	results := []state.CheckResult{
		logged(dhcpCheck(iface, ifaceErr)),
		logged(dhcp6Check(iface, ifaceErr)),
	}
	for _, r := range dnsChecks(seg) {
		results = append(results, logged(r))
	}
	if seg.GeoCheck != nil {
		results = append(results, logged(geoCheck(seg)))
	}
	results = append(results, logged(routingCheck(seg)))
	if seg.RoutingCheck6 != "" {
		results = append(results, logged(routing6Check(seg)))
	}
	return results
}

// resolveIfNameFn is a var, not a direct call, so tests can fake
// hardware discovery without touching the real filesystem -- same seam
// pattern as runOnceFn above and internal/selfupdate's goInstall var.
var resolveIfNameFn = ifdiscover.ResolveIfName

// discoverIface finds seg's real, currently-running interface via
// internal/ifaces -- a fresh `ip a` snapshot taken once per attempt and
// shared by dhcpCheck/dhcp6Check, rather than assuming a name from
// config.
//
// For a "wifi" segment, seg.IfName may be empty (see config.Segment.IfName's
// doc comment -- "auto", or identified by MAC/PCI instead of a literal
// name) -- resolved fresh via internal/ifdiscover (resolveIfNameFn) on
// EVERY call, not just once. That's deliberate, not wasted work: Run
// calls discoverIface once per attempt already, via runOnce, so a
// passthrough radio whose driver is still binding at the moment of the
// first attempt gets picked up on a later one for free, riding Run's
// existing retryDelay -- no separate retry loop needed here.
//
// A failure here (couldn't resolve the interface name, or couldn't even
// list interfaces) is carried into both dhcp/dhcp6 as their own failing
// result -- everything else below doesn't depend on it.
func discoverIface(seg config.Segment) (ifaces.Iface, error) {
	if seg.Type == "wifi" {
		resolved, err := resolveIfNameFn(seg.IfName, seg.MAC, seg.PCIVendor, seg.PCIDevice)
		if err != nil {
			return ifaces.Iface{}, fmt.Errorf("resolving wifi interface: %w", err)
		}
		seg.IfName = resolved
	}
	list, err := ifaces.List()
	if err != nil {
		return ifaces.Iface{}, err
	}
	return ifaces.Find(list, seg)
}

// dhcpCheck doesn't send its own DHCPDISCOVER -- netplan/systemd-networkd
// already did that before this binary was invoked (see the systemd unit's
// After=network-online.target). What's actually being confirmed here is
// the OUTCOME: does seg's real interface (discovered by internal/ifaces,
// not assumed from a naming convention) have a real, non-link-local IPv4
// address bound to it. Detail also reports the IPv4 default gateway
// learned via that interface, if any (defaultGateway) -- best-effort,
// never fails the check by itself.
func dhcpCheck(iface ifaces.Iface, ifaceErr error) state.CheckResult {
	name := "dhcp"
	if ifaceErr != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: ifaceErr.Error()}
	}
	for _, a := range iface.Addrs {
		if a.IP == nil || a.IP.To4() == nil || a.IP.IsLinkLocalUnicast() {
			continue
		}
		detail := a.IP.String()
		if gw := defaultGateway(iface.Name, false); gw != "" {
			detail = fmt.Sprintf("%s (gw %s)", detail, gw)
		}
		return state.CheckResult{Name: name, Pass: true, Detail: detail}
	}
	return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("no non-link-local IPv4 address on %s", iface.Name)}
}

// dhcp6Check is dhcpCheck's IPv6 counterpart: confirms seg's real
// interface also picked up at least one global (non-link-local) IPv6
// address, via SLAAC. See the package doc for why this is also,
// incidentally, a live regression check for OVN's solicited-RA-only
// behavior on this network. Detail lists EVERY global address found
// (SLAAC can hand out more than one, e.g. a stable address alongside an
// RFC 4941 privacy/temporary one) plus the IPv6 default gateway learned
// via that interface, if any (defaultGateway).
func dhcp6Check(iface ifaces.Iface, ifaceErr error) state.CheckResult {
	name := "dhcp6"
	if ifaceErr != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: ifaceErr.Error()}
	}
	var globals []string
	for _, a := range iface.Addrs {
		if a.IP == nil || a.IP.To4() != nil || a.IP.IsLinkLocalUnicast() {
			continue
		}
		globals = append(globals, a.IP.String())
	}
	if len(globals) == 0 {
		return state.CheckResult{
			Name: name, Pass: false,
			Detail: fmt.Sprintf("no global IPv6 address on %s (SLAAC via solicited RA may not have completed)", iface.Name),
		}
	}
	detail := strings.Join(globals, ",")
	if gw := defaultGateway(iface.Name, true); gw != "" {
		detail = fmt.Sprintf("%s (gw %s)", detail, gw)
	}
	return state.CheckResult{Name: name, Pass: true, Detail: detail}
}

// defaultGateway shells out to `ip [-6] route show default dev iface`
// and returns the "via" address, or "" if there's no default route
// through iface (expected for every segment except updateSegment) or the
// command fails for any reason -- best-effort only, never itself a
// reason to fail dhcpCheck/dhcp6Check. iproute2 (the `ip` binary) is
// always present on Ubuntu Server, so this needs no extra dependency.
func defaultGateway(iface string, ipv6 bool) string {
	args := []string{"route", "show", "default", "dev", iface}
	if ipv6 {
		args = append([]string{"-6"}, args...)
	}
	out, err := exec.Command("ip", args...).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// resolverVia builds a *net.Resolver that dials server (port 53)
// directly -- deliberately bypassing /etc/resolv.conf and the system
// resolver, so a lookup through it proves THIS specific server
// answered, not whatever else the OS might fall back to. Used for the
// "ipv4"/"ipv6" server kinds (see serverFor) -- "system" instead uses
// net.DefaultResolver directly.
func resolverVia(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		},
	}
}

// dnsChecks runs every test in seg.DNSCheck.Tests (nil DNSCheck skips
// all DNS checks entirely) once per server entry it's configured for --
// each test's own Servers if set, otherwise seg.DNSCheck.Servers -- and
// returns one state.CheckResult per (test, server) combination.
func dnsChecks(seg config.Segment) []state.CheckResult {
	if seg.DNSCheck == nil {
		return nil
	}
	var results []state.CheckResult
	for _, t := range seg.DNSCheck.Tests {
		servers := t.Servers
		if len(servers) == 0 {
			servers = seg.DNSCheck.Servers
		}
		for _, entry := range servers {
			results = append(results, dnsTestCheck(t, entry))
		}
	}
	return results
}

// serverFor resolves a DNSTest.Servers entry into the *net.Resolver and
// "via" label to use, or ok=false if entry isn't usable -- a value
// config.Load should already have rejected (defensive only; ok=false
// here isn't itself treated as a failure, see dnsTestCheck). entry is
// either the literal "system" (net.DefaultResolver) or a literal IP
// address (v4 or v6) dialed directly on port 53, bypassing
// /etc/resolv.conf -- see config.DNSCheck's doc comment.
func serverFor(entry string) (resolver *net.Resolver, via string, ok bool) {
	if entry == "system" {
		return net.DefaultResolver, "the system default resolver", true
	}
	if ip := net.ParseIP(entry); ip != nil {
		return resolverVia(entry), entry, true
	}
	return nil, "", false
}

// dnsTestCheck runs one config.DNSTest against one server entry -- see
// config.DNSTest's doc comment for what each Type does. The address
// family tested (ip4/ip6) is fixed by t.Type itself, never by which
// server answers the query.
func dnsTestCheck(t config.DNSTest, entry string) state.CheckResult {
	target := t.Host
	if target == "" {
		target = t.Domain
	}
	name := fmt.Sprintf("dns-%s-%s:%s", strings.ToLower(t.Type), entry, target)

	resolver, via, ok := serverFor(entry)
	if !ok {
		// a malformed server entry that config.Load should already have
		// rejected -- reported as a passing "not applicable" result
		// (visible in <segment>.result.yaml) rather than a failure or a
		// silently vanished check.
		return state.CheckResult{Name: name, Pass: true, Detail: fmt.Sprintf("skipped: %q is not \"system\" or a valid IP address", entry)}
	}

	switch t.Type {
	case "A":
		return forwardOnlyCheck(name, t.Host, "ip4", via, resolver)
	case "AAAA":
		return forwardOnlyCheck(name, t.Host, "ip6", via, resolver)
	case "A-PTR":
		return roundTripCheck(name, t.Host, "ip4", via, resolver)
	case "AAAA-PTR":
		return roundTripCheck(name, t.Host, "ip6", via, resolver)
	case "Hostname4":
		return roundTripCheck(name, selfFQDN(t.Domain), "ip4", via, resolver)
	case "Hostname6":
		return roundTripCheck(name, selfFQDN(t.Domain), "ip6", via, resolver)
	default:
		// config.Load already rejects this -- unreachable via normal
		// use, kept only so a Config built some other way (e.g. directly
		// in a test) fails loudly instead of panicking.
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("unknown dnsCheck test type %q", t.Type)}
	}
}

// forwardOnlyCheck resolves host as an A ("ip4") or AAAA ("ip6") record
// via resolver and passes as soon as at least one answer comes back --
// no round trip, no expected value, just "does this name resolve" (see
// config.DNSTest's "A"/"AAAA" doc comment).
func forwardOnlyCheck(name, host, network, via string, resolver *net.Resolver) state.CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ips, err := resolver.LookupIP(ctx, network, host)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: %v", host, via, err)}
	}
	if len(ips) == 0 {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: no answers", host, via)}
	}
	strs := make([]string, len(ips))
	for i, ip := range ips {
		strs[i] = ip.String()
	}
	return state.CheckResult{Name: name, Pass: true, Detail: fmt.Sprintf("%s -> %s", host, strings.Join(strs, ","))}
}

// selfFQDN builds the FQDN a "Hostname" test resolves: this host's own
// hostname (os.Hostname() -- whatever DHCP told this segment's resolver
// to dynamically register, e.g. "verify-mseg-tester" or production's
// "mseg-tester") plus domain, normalized to exactly one "." between them
// and exactly one trailing "." (domain's own trailing dot, if any, is
// stripped first so this never produces a doubled "..").
func selfFQDN(domain string) string {
	host, _ := os.Hostname() // best-effort: an error here is exceedingly unlikely (just a syscall), and simply produces an FQDN that won't resolve -- the check itself then fails with a clear detail, no special-casing needed here.
	return fmt.Sprintf("%s.%s.", host, strings.TrimSuffix(domain, "."))
}

// roundTripCheck forward-resolves host -- as an A/AAAA record via
// resolver.LookupIP, network always "ip4" or "ip6" (never "" here: the
// address family is fixed by the caller's DNSTest.Type, not by which
// server kind is answering) -- takes the first answer, reverse-resolves
// (PTR) THAT address, and expects the PTR name to match host: a full
// forward-confirmed reverse DNS (FCrDNS) round trip. Shared
// implementation behind config.DNSTest's "A-PTR"/"AAAA-PTR" and
// "Hostname4"/"Hostname6" types, which differ only in what host is: a
// fixed name (e.g. a segment's gateway) vs. this running VM's own
// dynamically-registered one. The two lookups each get their own full
// timeout budget rather than sharing one context, so a slow forward
// lookup can't starve the reverse lookup that follows it.
func roundTripCheck(name, host, network, via string, resolver *net.Resolver) state.CheckResult {
	fctx, fcancel := context.WithTimeout(context.Background(), timeout)
	var firstAnswer string
	ips, err := resolver.LookupIP(fctx, network, host)
	if err == nil && len(ips) > 0 {
		firstAnswer = ips[0].String()
	}
	fcancel()
	if err != nil || firstAnswer == "" {
		detail := fmt.Sprintf("resolving %s via %s: no answers", host, via)
		if err != nil {
			detail = fmt.Sprintf("resolving %s via %s: %v", host, via, err)
		}
		return state.CheckResult{Name: name, Pass: false, Detail: detail}
	}

	rctx, rcancel := context.WithTimeout(context.Background(), timeout)
	names, err := resolver.LookupAddr(rctx, firstAnswer)
	rcancel()
	if err != nil {
		return state.CheckResult{
			Name: name, Pass: false,
			Detail: fmt.Sprintf("%s -> %s, but reverse-resolving %s via %s: %v", host, firstAnswer, firstAnswer, via, err),
		}
	}
	for _, n := range names {
		if strings.EqualFold(n, host) {
			return state.CheckResult{Name: name, Pass: true, Detail: fmt.Sprintf("%s -> %s -> %s", host, firstAnswer, n)}
		}
	}
	return state.CheckResult{
		Name: name, Pass: false,
		Detail: fmt.Sprintf("%s -> %s, but reverse-resolving %s via %s: expected %q, got %v", host, firstAnswer, firstAnswer, via, host, names),
	}
}

// geoCheck fetches GeoCheck.URL and, if Expect is set, confirms the
// response body contains it (case-insensitive) -- e.g. an IP-geolocation
// echo service returning a country name, proving THIS segment's traffic
// is actually egressing through the uplink it's supposed to (a WireGuard
// exit in a specific region), not just that outbound HTTP works at all.
// Deliberately protocol-agnostic (whichever family the OS/network
// prefers) -- geo-exit behavior isn't expected to differ by family here.
func geoCheck(seg config.Segment) state.CheckResult {
	name := "geo"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, seg.GeoCheck.URL, nil)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("reading response: %v", err)}
	}
	text := strings.TrimSpace(string(body))
	if seg.GeoCheck.Expect == "" {
		return state.CheckResult{Name: name, Pass: true, Detail: text}
	}
	if strings.Contains(strings.ToLower(text), strings.ToLower(seg.GeoCheck.Expect)) {
		return state.CheckResult{Name: name, Pass: true, Detail: text}
	}
	return state.CheckResult{
		Name: name, Pass: false,
		Detail: fmt.Sprintf("expected %q, got %q", seg.GeoCheck.Expect, text),
	}
}

// routingCheck confirms plain reachability to something outside this
// segment entirely -- a TCP dial rather than ICMP, since ICMP needs raw
// sockets/CAP_NET_RAW this binary shouldn't need to run with, and TCP
// connect-refused vs. timeout already distinguishes "reachable, nothing
// listening" from "not reachable at all" well enough for this purpose.
func routingCheck(seg config.Segment) state.CheckResult {
	name := "routing"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp4", seg.RoutingCheck)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: err.Error()}
	}
	defer conn.Close()
	return state.CheckResult{Name: name, Pass: true, Detail: seg.RoutingCheck}
}

// routing6Check mirrors routingCheck over IPv6 -- RoutingCheck6 must
// already be a valid "[ipv6]:port" literal (brackets included, same as
// any Go "host:port" with an IPv6 host). "tcp6" forces the IPv6 family
// explicitly rather than letting a hostname resolve either way -- only
// run when RoutingCheck6 is configured.
func routing6Check(seg config.Segment) state.CheckResult {
	name := "routing6"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp6", seg.RoutingCheck6)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: err.Error()}
	}
	defer conn.Close()
	return state.CheckResult{Name: name, Pass: true, Detail: seg.RoutingCheck6}
}
