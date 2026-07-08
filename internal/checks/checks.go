// Package checks runs the things a segment needs to prove to pass: it
// handed out real IPv4 and IPv6 addresses (DHCP/SLAAC already having run
// via netplan by the time this binary starts -- checked here, not
// performed here), its configured DNS record(s) resolve -- both a local
// group (this segment's own resolver) and a remote group (the system
// default resolver, or another server entirely), each with its own list
// of records to test (see config.DNSCheck) -- and its egress uplink
// actually carries traffic over both families.
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
	"os/exec"
	"strings"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/ifaces"
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
// across runs by hand): dhcp, dhcp6, every configured DNSCheck
// local/remote group (see dnsChecks), reverse/reverse6 if configured, geo
// if configured, routing, routing6 if configured.
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
	if seg.ReverseCheck != nil {
		results = append(results, logged(reverseCheck(seg)))
	}
	if seg.ReverseCheck6 != nil && seg.DNSServer6 != "" {
		results = append(results, logged(reverse6Check(seg)))
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

// discoverIface finds seg's real, currently-running interface via
// internal/ifaces -- a fresh `ip a` snapshot taken once per attempt and
// shared by dhcpCheck/dhcp6Check, rather than assuming a name from
// config. A failure here (couldn't even list interfaces) is carried into
// both dhcp/dhcp6 as their own failing result -- everything else below
// doesn't depend on it.
func discoverIface(seg config.Segment) (ifaces.Iface, error) {
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
// resolver, so a lookup through it proves THIS specific server answered,
// not whatever else the OS might fall back to. Used whenever a
// config.DNSCheckGroup names a specific Server (or, for the local group,
// whenever Segment.DNSServer/DNSServer6 supply the default) -- the
// remote group's default instead uses net.DefaultResolver directly (see
// dnsChecks).
func resolverVia(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		},
	}
}

// dnsLookup resolves fqdn via resolver and turns the outcome into a
// state.CheckResult named name -- the one building block shared by every
// dns-* check below.
func dnsLookup(name, fqdn, via string, resolver *net.Resolver) state.CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, fqdn)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: %v", fqdn, via, err)}
	}
	if len(addrs) == 0 {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: no answers", fqdn, via)}
	}
	return state.CheckResult{Name: name, Pass: true, Detail: fmt.Sprintf("%s -> %s", fqdn, strings.Join(addrs, ","))}
}

// dnsChecks runs seg.DNSCheck's Local and/or Remote groups (either or
// both may be nil -- see config.DNSCheck) and returns one state.CheckResult
// per (group, family, test) combination.
func dnsChecks(seg config.Segment) []state.CheckResult {
	if seg.DNSCheck == nil {
		return nil
	}
	var results []state.CheckResult
	if seg.DNSCheck.Local != nil {
		results = append(results, localDNSGroupChecks(seg, seg.DNSCheck.Local)...)
	}
	if seg.DNSCheck.Remote != nil {
		results = append(results, remoteDNSGroupChecks(seg.DNSCheck.Remote)...)
	}
	return results
}

// localDNSGroupChecks runs group.Tests against group.Server if set, or
// otherwise against BOTH of seg's own resolver addresses (DNSServer over
// IPv4, and DNSServer6 over IPv6 too if that's configured) -- see
// config.DNSCheckGroup's doc comment.
func localDNSGroupChecks(seg config.Segment, group *config.DNSCheckGroup) []state.CheckResult {
	if group.Server != "" {
		resolver := resolverVia(group.Server)
		var results []state.CheckResult
		for _, fqdn := range group.Tests {
			results = append(results, dnsLookup(fmt.Sprintf("dns-local:%s", fqdn), fqdn, group.Server, resolver))
		}
		return results
	}
	var results []state.CheckResult
	v4 := resolverVia(seg.DNSServer)
	for _, fqdn := range group.Tests {
		results = append(results, dnsLookup(fmt.Sprintf("dns-local-ipv4:%s", fqdn), fqdn, seg.DNSServer, v4))
	}
	if seg.DNSServer6 != "" {
		v6 := resolverVia(seg.DNSServer6)
		for _, fqdn := range group.Tests {
			results = append(results, dnsLookup(fmt.Sprintf("dns-local-ipv6:%s", fqdn), fqdn, seg.DNSServer6, v6))
		}
	}
	return results
}

// remoteDNSGroupChecks runs group.Tests against group.Server if set, or
// otherwise the system's default resolver (whatever /etc/resolv.conf
// points at) -- see config.DNSCheckGroup's doc comment.
func remoteDNSGroupChecks(group *config.DNSCheckGroup) []state.CheckResult {
	resolver, via := net.DefaultResolver, "the system default resolver"
	if group.Server != "" {
		resolver, via = resolverVia(group.Server), group.Server
	}
	var results []state.CheckResult
	for _, fqdn := range group.Tests {
		results = append(results, dnsLookup(fmt.Sprintf("dns-remote:%s", fqdn), fqdn, via, resolver))
	}
	return results
}

// reverseCheck confirms ReverseCheck.IP reverse-resolves (PTR) to
// ReverseCheck.Expect against this segment's own resolver (DNSServer,
// same "bypass /etc/resolv.conf, ask THIS segment's server specifically"
// reasoning as the dns-local-* checks). A resolver can answer forward
// queries fine while its PTR zone is stale or wrong, so this is a
// genuinely distinct failure mode, not just a formality.
func reverseCheck(seg config.Segment) state.CheckResult {
	name := "reverse"
	resolver := resolverVia(seg.DNSServer)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, seg.ReverseCheck.IP)
	if err != nil {
		return state.CheckResult{
			Name: name, Pass: false,
			Detail: fmt.Sprintf("reverse-resolving %s via %s: %v", seg.ReverseCheck.IP, seg.DNSServer, err),
		}
	}
	for _, n := range names {
		if strings.EqualFold(n, seg.ReverseCheck.Expect) {
			return state.CheckResult{Name: name, Pass: true, Detail: n}
		}
	}
	return state.CheckResult{
		Name: name, Pass: false,
		Detail: fmt.Sprintf("reverse-resolving %s via %s: expected %q, got %v", seg.ReverseCheck.IP, seg.DNSServer, seg.ReverseCheck.Expect, names),
	}
}

// reverse6Check mirrors reverseCheck but reaches this segment's resolver
// over IPv6 (DNSServer6) -- only run when both DNSServer6 and
// ReverseCheck6 are configured. IP itself may be either family (PTR
// lookups work the same regardless of which transport carries the DNS
// query), but the "typical" case is IP being that segment's own IPv6
// gateway/host address.
func reverse6Check(seg config.Segment) state.CheckResult {
	name := "reverse6"
	resolver := resolverVia(seg.DNSServer6)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, seg.ReverseCheck6.IP)
	if err != nil {
		return state.CheckResult{
			Name: name, Pass: false,
			Detail: fmt.Sprintf("reverse-resolving %s via %s: %v", seg.ReverseCheck6.IP, seg.DNSServer6, err),
		}
	}
	for _, n := range names {
		if strings.EqualFold(n, seg.ReverseCheck6.Expect) {
			return state.CheckResult{Name: name, Pass: true, Detail: n}
		}
	}
	return state.CheckResult{
		Name: name, Pass: false,
		Detail: fmt.Sprintf("reverse-resolving %s via %s: expected %q, got %v", seg.ReverseCheck6.IP, seg.DNSServer6, seg.ReverseCheck6.Expect, names),
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
