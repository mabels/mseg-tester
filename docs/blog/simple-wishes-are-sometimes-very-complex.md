---
layout: page
title: Simple Wishes Are Sometimes Very Complex
---

# Simple Wishes Are Sometimes Very Complex

Code: [github.com/mabels/mseg-tester](https://github.com/mabels/mseg-tester)

## The itch

For close to fifteen years I've wanted a specific, boring-sounding thing: a way to
know, automatically and continuously, whether a network actually works — not just
"can I ping 8.8.8.8" but the whole chain a real client depends on. DHCP handing out
an address. DNS answering, in both directions. IPv6 SLAAC and router advertisements
doing their auto-configuration dance correctly. And if the network includes Wi-Fi,
the client actually associating, authenticating, and getting through that same
chain over the air.

There is no shortage of tools that check one link of that chain in isolation. `dig`
tells you DNS works. `ping` tells you routing works. But DHCP and SLAAC are
stateful and transient — they happen once, at boot, and then the evidence is a
lease or an address that's already there by the time you'd think to check it. And
once you add multiple access points, multiple SSIDs, or multiple VLAN segments to
the mix, the thing you actually care about — "does autodiscovery work end-to-end,
on *every* one of these networks, the way a real laptop would experience it" —
turns into a combinatorial mess that I never once saw solved cleanly. Not by a
product, not by a script I found online, not by anything. I looked, more than
once, over the years. Nothing.

This isn't the first piece I've written about this same home network, either.
The segmented VLAN/Wi-Fi fabric mseg-tester actually cycles through is the one
[ovn-fabric](https://mabels.github.io/ovn-fabric/blog/ovn-fabric-writeup.html)
bridges into OVN/OVS in the first place, and the DNS/DHCP side of that same
network — the part mseg-tester can only ever *test*, not fix — is its own long
story, told in
["It's always DNS"](https://mabels.github.io/unified-dns-dhcp-chart/writeups/blog/its-always-dns.html).
This post picks up where those two leave off: the network exists and DNS/DHCP
are wired up — now how do you actually *know*, continuously, that they keep
working.

## Enter Claude

So this time I actually sat down and started talking through the architecture with
Claude instead of shelving the idea again. The design conversation alone wasn't
trivial. The first real conclusion: this needs a dedicated piece of hardware — an
RPi, or a VM with real Wi-Fi access — because you can't test a network truthfully
from inside a container or a script running on a machine that's already on some
other network.

The second, more important conclusion: the quality of the test is directly
proportional to how closely the test client resembles a *real* device. The
platonic ideal is literally a user's laptop being handed Wi-Fi credentials and
joining like any other guest. The closer you get to that, the more the test is
actually worth something. But that raises two problems immediately: how do you
switch a single test device between segments and SSIDs without the switching
itself becoming a source of false failures, and — just as important — how do you
get the results back out, given that not every segment you're testing has any
reporting or alerting infrastructure of its own. Some of these networks are
deliberately isolated. That's the point of testing them.

## The sequencer

What we landed on is almost embarrassingly simple to describe: a VM (or RPi) whose
one network interface gets reconfigured for a single segment, reboots into that
configuration, runs its checks, stores the results locally, and then moves on to
the next segment in the cycle. When the cycle reaches the one segment that
actually has a route out — the "report" segment — it flushes everything it's
accumulated to an external collector, then keeps going. Forever. One box,
cycling through every VLAN and every Wi-Fi network it's been told about, one
reboot at a time.

The same report segment is also how the box updates itself, and deliberately
not through anything resembling a release pipeline: no GitHub Releases, no
binary artifacts to keep in sync, no package registry involved at all. It just
`git fetch`es the source repo it was told to track, compares the commit it's
sitting on against the one it was actually built from, and — only if they
differ — rebuilds with a plain `go build` and replaces itself. Shipping a
change to every box in the field is nothing more than `git push origin main`.
There's a real practical reason for this beyond simplicity, too: checking
`api.github.com` for a new release on every single cycle would run straight
into GitHub's unauthenticated rate limit once you have more than a couple of
boxes doing it; a plain `git fetch` against GitHub's git servers doesn't share
that limit.

The reboot is the trick, not a compromise. It means the network configuration on
the box at any given moment is as simple as it could possibly be — one interface,
one segment, no leftover state from whatever was configured five minutes ago, no
SLAAC addresses lingering from a network it isn't even connected to anymore. You
don't need to write careful teardown code to reset DHCP leases or drop stale
routes or forget an old Wi-Fi association, because a cold boot does all of that
for you, for free, and does it the same way a real client's boot would. The
simplicity isn't just aesthetic — it's what makes the test trustworthy in the
first place.

## Where it actually got hard

All of that is a clean idea to describe in a paragraph. Building it was a
different story, and the title of this post is about exactly where the
difficulty showed up.

The software itself — DHCP/DHCP6 checks, a flat list of DNS test types (forward,
reverse, dynamic-registration round trips), routing reachability, an optional
geo-IP check for VPN-exit segments — wasn't the hard part. The hard part was
state. This tool's entire existence is defined by surviving its own repeated,
deliberate death: every single "session" is one boot, and everything that matters
— which segment is active, what the cycle order is, what's been tested so far,
whether it's safe to advance to the next segment or reboot at all — has to live
in a file that survives past the process exiting and the machine restarting. That
is a fundamentally different shape of program than almost anything else, and
getting an LLM to consistently reason about "what does the NEXT boot need to
already know, and where does it read it from" — rather than reasoning about state
the way you'd reason about a normal long-running process — took Claude to the
edge of what it could reliably keep straight. We hit real, working-but-wrong
designs more than once: a throttle that quietly broke itself the moment I asked
for a related fix, a "just sleep longer" patch that looked correct and wasn't,
because the actual bug was a segment coming back around and rebooting away before
the fix even had a chance to matter. Each of those took a live test on the actual
VM to catch — reading the code convinced both of us it was right, and it still
wasn't.

That last point is worth dwelling on, because it's the flip side of the whole
design: the exact same reboot that makes each segment's test trustworthy also
makes the *system itself* almost impossible to analyze while it's running. You
can't attach a debugger to a process that's about to reboot out from under you.
You can't tail a log across the reboot — it's gone, and the next boot starts a
fresh one. You can't even assume the box is still reachable at the address you
just SSHed to; by the time a command finishes, it may have already moved on to a
different segment with a different address entirely, or be mid-boot and
reachable nowhere at all. Every real bug in this project got run down by racing
a live SSH session or a live ping test against the reboot cycle itself, catching
it in the narrow window before the evidence rebooted away — not by anything
resembling normal debugging.

It took about a week of four-to-five-hour sessions, and somewhere around $100 in
tokens, to get from "let's design this" to "this actually runs correctly on real
hardware." That's a lot more time and money than I expected going in for
something whose end description fits in two paragraphs.

## Where it stands

It works now. Point it at a Proxmox host, give it a VLAN-trunked NIC and
(optionally) a passed-through Wi-Fi radio, and it will cycle through every
segment and every Wi-Fi network you've configured, checking DHCP/DHCP6, DNS
(forward, reverse, and dynamic-registration), routing, and geo-location where it
applies — and write everything into an InfluxDB bucket, ready for a Grafana
dashboard to sit on top of. Alerting on top of that isn't wired up yet — that's
still a TODO — but the data everything else would need is already flowing.

Fifteen years is a long time to want something boring. I'm glad it exists now. If
you've built something like this before, I'd genuinely like to hear about it —
I looked, and I didn't find it.
