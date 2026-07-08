// Package checks runs the things a segment needs to prove to pass: it
// handed out real IPv4 and IPv6 addresses (DHCP/SLAAC already having run
// via netplan by the time this binary starts -- checked here, not
// performed here), its own DNS server answers over both address families
// (and, optionally, looks like it's answering from the right region), and
// its egress uplink actually carries traffic over both families.
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
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/netplan"
	"github.com/mabels/mseg-tester/internal/state"
)

const timeout = 8 * time.Second

// Run performs every check for one segment and returns them in a fixed
// order (dhcp, dhcp6, dns, dns6 if configured, reverse if configured,
// reverse6 if configured, geo if configured, routing, routing6 if
// configured) -- fixed order so <segment>.result.yaml reads the same
// shape every time, easier to diff across runs by hand.
//
// Which interface belongs to seg -- and whether it's the trunk's
// native/untagged VLAN or a tagged sub-interface -- comes entirely from
// seg.Type/seg.IfName (config.yaml is the single source of truth here;
// see netplan.IfaceName), not from any separate bootstrap-level field.
//
// attempts/retryDelay come from config.Config's CheckAttemptsOrDefault/
// CheckRetryDelayOrDefault -- EVERY check below is retried independently
// up to attempts times (stopping as soon as one passes), waiting
// retryDelay between tries. Applies uniformly, including dhcp/dhcp6:
// those just read already-settled interface state (network-online.target
// already waited for it), but a retry costs nothing and guards against
// any lingering race, not just the network-dependent checks.
func Run(seg config.Segment, trunkInterface string, attempts int, retryDelay time.Duration) []state.CheckResult {
	if attempts <= 0 {
		attempts = 1
	}
	results := []state.CheckResult{
		withRetry(attempts, retryDelay, func() state.CheckResult { return dhcpCheck(seg, trunkInterface) }),
		withRetry(attempts, retryDelay, func() state.CheckResult { return dhcp6Check(seg, trunkInterface) }),
		withRetry(attempts, retryDelay, func() state.CheckResult { return dnsCheck(seg) }),
	}
	if seg.DNSServer6 != "" {
		results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return dns6Check(seg) }))
	}
	if seg.ReverseCheck != nil {
		results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return reverseCheck(seg) }))
	}
	if seg.ReverseCheck6 != nil && seg.DNSServer6 != "" {
		results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return reverse6Check(seg) }))
	}
	if seg.GeoCheck != nil {
		results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return geoCheck(seg) }))
	}
	results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return routingCheck(seg) }))
	if seg.RoutingCheck6 != "" {
		results = append(results, withRetry(attempts, retryDelay, func() state.CheckResult { return routing6Check(seg) }))
	}
	return results
}

// withRetry calls fn up to attempts times, stopping as soon as one
// attempt passes. On a check that never passes, the last failing
// result's Detail gets the attempt count appended, so
// <segment>.result.yaml distinguishes "failed once" from "failed after
// every retry" without needing a separate field.
func withRetry(attempts int, delay time.Duration, fn func() state.CheckResult) state.CheckResult {
	var last state.CheckResult
	for i := 1; i <= attempts; i++ {
		last = fn()
		if last.Pass {
			return last
		}
		if i < attempts {
			time.Sleep(delay)
		}
	}
	if attempts > 1 {
		last.Detail = fmt.Sprintf("%s (failed all %d attempts)", last.Detail, attempts)
	}
	return last
}

// dhcpCheck doesn't send its own DHCPDISCOVER -- netplan/systemd-networkd
// already did that before this binary was invoked (see the systemd unit's
// After=network-online.target). What's actually being confirmed here is
// the OUTCOME: does this segment's interface (a tagged VLAN sub-interface,
// or the trunk interface itself if this IS the native segment -- see
// netplan.IfaceName) have a real, non-link-local IPv4 address bound to
// it. That's the observable proof DHCP succeeded, without this tool
// needing to re-implement a DHCP client itself just to double-check one
// already run. Detail also reports the IPv4 default gateway learned via
// that interface, if any (defaultGateway) -- best-effort, never fails
// the check by itself.
func dhcpCheck(seg config.Segment, trunkInterface string) state.CheckResult {
	name := "dhcp"
	ifaceName := netplan.IfaceName(trunkInterface, seg)
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("interface %s: %v", ifaceName, err)}
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("reading addrs on %s: %v", ifaceName, err)}
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.To4() == nil || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		detail := ipNet.IP.String()
		if gw := defaultGateway(ifaceName, false); gw != "" {
			detail = fmt.Sprintf("%s (gw %s)", detail, gw)
		}
		return state.CheckResult{Name: name, Pass: true, Detail: detail}
	}
	return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("no non-link-local IPv4 address on %s", ifaceName)}
}

// dhcp6Check is dhcpCheck's IPv6 counterpart: confirms the same VLAN
// sub-interface also picked up at least one global (non-link-local) IPv6
// address, via SLAAC. Unconditional and needs no per-segment config,
// exactly like dhcpCheck -- just interface state. See the package doc for
// why this is also, incidentally, a live regression check for OVN's
// solicited-RA-only behavior on this network. Detail lists EVERY global
// address found (SLAAC can hand out more than one, e.g. a stable address
// alongside an RFC 4941 privacy/temporary one) plus the IPv6 default
// gateway learned via that interface, if any (defaultGateway).
func dhcp6Check(seg config.Segment, trunkInterface string) state.CheckResult {
	name := "dhcp6"
	ifaceName := netplan.IfaceName(trunkInterface, seg)
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("interface %s: %v", ifaceName, err)}
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("reading addrs on %s: %v", ifaceName, err)}
	}
	var globals []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.To4() != nil || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		globals = append(globals, ipNet.IP.String())
	}
	if len(globals) == 0 {
		return state.CheckResult{
			Name: name, Pass: false,
			Detail: fmt.Sprintf("no global IPv6 address on %s (SLAAC via solicited RA may not have completed)", ifaceName),
		}
	}
	detail := strings.Join(globals, ",")
	if gw := defaultGateway(ifaceName, true); gw != "" {
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

// dnsCheck resolves DNSCheck directly against this segment's own resolver
// (not the system resolver -- deliberately bypassing /etc/resolv.conf so
// this proves THIS segment's DNS server specifically answered, regardless
// of what else the OS might fall back to).
func dnsCheck(seg config.Segment) state.CheckResult {
	name := "dns"
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(seg.DNSServer, "53"))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, seg.DNSCheck)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: %v", seg.DNSCheck, seg.DNSServer, err)}
	}
	if len(addrs) == 0 {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: no answers", seg.DNSCheck, seg.DNSServer)}
	}
	return state.CheckResult{Name: name, Pass: true, Detail: strings.Join(addrs, ",")}
}

// dns6Check mirrors dnsCheck but reaches this segment's resolver over
// IPv6 (net.JoinHostPort brackets the address automatically, same as any
// IPv6 literal) -- only run when DNSServer6 is configured.
func dns6Check(seg config.Segment) state.CheckResult {
	name := "dns6"
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(seg.DNSServer6, "53"))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, seg.DNSCheck)
	if err != nil {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: %v", seg.DNSCheck, seg.DNSServer6, err)}
	}
	if len(addrs) == 0 {
		return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("resolving %s via %s: no answers", seg.DNSCheck, seg.DNSServer6)}
	}
	return state.CheckResult{Name: name, Pass: true, Detail: strings.Join(addrs, ",")}
}

// reverseCheck confirms ReverseCheck.IP reverse-resolves (PTR) to
// ReverseCheck.Expect against this segment's own resolver (DNSServer,
// same "bypass /etc/resolv.conf, ask THIS segment's server specifically"
// reasoning as dnsCheck). A resolver can answer forward queries fine
// while its PTR zone is stale or wrong, so this is a genuinely distinct
// failure mode from dnsCheck, not just a formality.
func reverseCheck(seg config.Segment) state.CheckResult {
	name := "reverse"
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(seg.DNSServer, "53"))
		},
	}
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
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, net.JoinHostPort(seg.DNSServer6, "53"))
		},
	}
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
