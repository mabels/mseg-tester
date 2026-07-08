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
	"strings"
	"time"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/netplan"
	"github.com/mabels/mseg-tester/internal/state"
)

const timeout = 8 * time.Second

// Run performs every check for one segment and returns them in a fixed
// order (dhcp, dhcp6, dns, dns6 if configured, geo if configured, routing,
// routing6 if configured) -- fixed order so <segment>.result.yaml reads
// the same shape every time, easier to diff across runs by hand.
//
// nativeSegment is bootstrap.Bootstrap.NativeSegment, passed through
// unchanged -- see netplan.IfaceName for what it means. Empty is fine:
// every segment is then assumed to be a normal tagged VLAN.
func Run(seg config.Segment, trunkInterface, nativeSegment string) []state.CheckResult {
	results := []state.CheckResult{
		dhcpCheck(seg, trunkInterface, nativeSegment),
		dhcp6Check(seg, trunkInterface, nativeSegment),
		dnsCheck(seg),
	}
	if seg.DNSServer6 != "" {
		results = append(results, dns6Check(seg))
	}
	if seg.GeoCheck != nil {
		results = append(results, geoCheck(seg))
	}
	results = append(results, routingCheck(seg))
	if seg.RoutingCheck6 != "" {
		results = append(results, routing6Check(seg))
	}
	return results
}

// dhcpCheck doesn't send its own DHCPDISCOVER -- netplan/systemd-networkd
// already did that before this binary was invoked (see the systemd unit's
// After=network-online.target). What's actually being confirmed here is
// the OUTCOME: does this segment's interface (a tagged VLAN sub-interface,
// or the trunk interface itself if this IS the native segment -- see
// netplan.IfaceName) have a real, non-link-local IPv4 address bound to
// it. That's the observable proof DHCP succeeded, without this tool
// needing to re-implement a DHCP client itself just to double-check one
// already run.
func dhcpCheck(seg config.Segment, trunkInterface, nativeSegment string) state.CheckResult {
	name := "dhcp"
	ifaceName := netplan.IfaceName(trunkInterface, seg.Name, nativeSegment)
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
		return state.CheckResult{Name: name, Pass: true, Detail: ipNet.IP.String()}
	}
	return state.CheckResult{Name: name, Pass: false, Detail: fmt.Sprintf("no non-link-local IPv4 address on %s", ifaceName)}
}

// dhcp6Check is dhcpCheck's IPv6 counterpart: confirms the same VLAN
// sub-interface also picked up a global (non-link-local) IPv6 address,
// via SLAAC. Unconditional and needs no per-segment config, exactly like
// dhcpCheck -- just interface state. See the package doc for why this is
// also, incidentally, a live regression check for OVN's solicited-RA-only
// behavior on this network.
func dhcp6Check(seg config.Segment, trunkInterface, nativeSegment string) state.CheckResult {
	name := "dhcp6"
	ifaceName := netplan.IfaceName(trunkInterface, seg.Name, nativeSegment)
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
		if !ok || ipNet.IP.To4() != nil || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		return state.CheckResult{Name: name, Pass: true, Detail: ipNet.IP.String()}
	}
	return state.CheckResult{
		Name: name, Pass: false,
		Detail: fmt.Sprintf("no global IPv6 address on %s (SLAAC via solicited RA may not have completed)", ifaceName),
	}
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
