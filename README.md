# mseg-tester

A small self-contained Go binary that cycles a single VM through every
network segment (128/129/130/131), one per reboot, confirming on each one
that DHCP/SLAAC handed out both an IPv4 and IPv6 address, DNS answers over
both families (and, for VPN-exit segments, answers as if physically in
the right region), and the segment's egress uplink actually carries
traffic. IPv6 gets equal billing deliberately — see `internal/checks`'
package doc for why a segment's SLAAC address is also, incidentally, an
ongoing regression check for a specific OVN behavior on this network
(only *solicited* router advertisements are ever delivered).

## Why a VM that reboots into each segment, instead of a container per segment

The earlier attempt at this needed a controller with Proxmox API access to
spin up/tear down test VMs per segment — a privileged surface nobody
wanted to grant. This version needs **zero** ongoing Proxmox access: the
VM's one virtual NIC is set up **once, by hand**, as an 802.1Q trunk
carrying every segment's VLAN tag. From that point on, switching which
segment is under test is just this binary rewriting its own netplan
config and calling `reboot` — a fresh, genuine cold-boot DHCP/DNS/routing
test every cycle, entirely self-contained inside the guest.

## Two config files, split by how often each changes

Both live under `/mseg-tester` — nothing this tool touches lives under
`/etc` at all — but stay conceptually distinct:

- **`/mseg-tester/bootstrap.yaml`** — local, rarely changes, written
  once by cloud-init: which NIC is the trunk, which segment can reach the
  internet (`updateSegment`), where the public software repo is, and
  (optionally) where a private config repo is.
- **`/mseg-tester/config.yaml`** — the content: segment list, test
  targets, reboot timing, where to report results. This is the one file
  here that genuinely changes on its own schedule. Two ways to get it
  onto the VM, pick either:
  - **easy (the default)**: cloud-init just writes it directly — no
    repo, no token. Changing it means editing `cloud-init/user-data.yaml`
    and re-provisioning.
  - set `bootstrap.yaml`'s `configRepo` to a repo URL holding this file
    and it's fetched/refreshed at runtime instead (`internal/configsync`)
    — changing it is then a commit, not a re-provision.

  See `examples/config.yaml` for the shape either way.

## How a cycle works

Every check below runs as one batch. If ANY check in the batch fails, the
WHOLE batch — not just the failing check — is re-run from scratch after
`config.yaml`'s `checkRetryDelay` (default `10s`), up to `checkAttempts`
times (default 3) total; the batch stops retrying as soon as one full
attempt passes every check, and the LAST attempt's results are what get
recorded if every attempt still had a failure. Pass `mseg-tester run
-verbose` to log each step and each check's pass/fail/detail as it
happens, not just the final summary.

One straight-line sequence, no branching state machine and nothing that
throttles independently of it:

1. Read `/mseg-tester/active.yaml` — which segment is active right now.
   If it doesn't exist (a genuinely fresh VM, or a state dir that's lost
   it), DETECT the live segment instead of assuming any particular one:
   compare each `config.yaml` segment's expected interface
   (`internal/netplan.IfaceName`, resolving a `"wifi"` segment's real
   interface first) against a live `ip a` snapshot
   (`internal/ifaces.List`), and take whichever one is UP with a real
   (non-link-local) address (`detectActiveSegment`/`matchActiveSegment`
   in `cmd/mseg-tester`). This matters for more than just true first
   boot: if `active.yaml` is lost partway through an already-cycling
   box's lifetime (e.g. its state dir wiped while parked on a later
   segment), assuming `updateSegment` would be wrong — this looks at
   what's actually live instead. Falls back to `bootstrap.yaml`'s
   `updateSegment`, logged, only if detection itself fails outright.
2. **Only if** the active segment IS `updateSegment` (the "report
   segment" — the only one with a route anywhere outside the segment
   under test) AND `bootstrap.yaml`'s `configRepo` is set: fetch the
   latest `config.yaml` from that repo (best-effort — a failed fetch
   just leaves the last successfully-synced, or cloud-init-provisioned,
   `config.yaml` in place). On a genuinely fresh VM (`active.yaml`
   didn't exist at all), this is attempted unconditionally instead,
   before the segment is even known — see step 1's fallback rationale.
   Skipped entirely when `configRepo` is empty.
3. Apply `config.yaml`'s `timezone` if set (`timedatectl set-timezone`) —
   idempotent and best-effort, never fatal.
4. Discover which real interface is carrying this segment's traffic by
   running `ip a` and parsing its output (`internal/ifaces`) — not by
   assuming a name from config: mseg-tester's own netplan never brings up
   more than "lo", the trunk NIC, and (only for a tagged VLAN segment)
   one VLAN sub-interface at a time, so "whichever non-loopback interface
   is actually the right one" is discoverable without trusting any naming
   convention. Then confirm that interface actually has a non-link-local
   IPv4 address, **and** a global (non-link-local) IPv6 address
   (`dhcp`/`dhcp6` — DHCP/SLAAC already ran via netplan before this binary
   starts, see the systemd unit's `After=network-online.target`; this
   step confirms the *outcome*, it doesn't run its own DHCP client or
   send its own router solicitation). Both checks also record,
   best-effort, the interface's default gateway (`ip route show
   default`, IPv4 and IPv6 separately) and, for `dhcp6`, every
   SLAAC-assigned global IPv6 address seen (not just the first) — all
   appended to the check's `Detail`, purely informational, never fails
   the check on their own.
5. Run every test in `dnsCheck.tests`, whichever are set — a single flat
   list, each test naming a `type` and running once per server in that
   test's own `servers` (or `dnsCheck.servers` if the test doesn't
   override it):
   - `system` — the OS's default resolver (whatever `/etc/resolv.conf`
     points at, normally this segment's own resolver too via DHCP) —
     proves plain, unconfigured resolution works.
   - a literal IP address (v4 or v6, e.g. `192.168.130.5`,
     `fd00:192:168:130::5`, or a public resolver like `1.1.1.1`) — dialed
     directly on port 53, bypassing `/etc/resolv.conf` — proves THIS
     specific server answers, regardless of what the OS ended up with via
     DHCP. Any address reachable from the segment works, not just its own
     resolver.

   Each test's `type` decides both what's checked and which address
   family it forces, regardless of which server answers it:
   - `A` / `AAAA` — `host`: resolves as that record type, passes as soon
     as any answer comes back. No round trip, no expected value — use
     for plain reachability (e.g. `google.com.`, or this segment's own
     zone, catching "answers its own zone but doesn't forward upstream"
     vs. the reverse).
   - `A-PTR` / `AAAA-PTR` — `host`: forward-resolves host (A or AAAA),
     reverse-resolves the first answer, and expects the PTR name to
     equal `host` — a full forward-confirmed reverse DNS (FCrDNS) round
     trip against a fixed, known name (e.g. a segment's own gateway). A
     resolver's forward zone can be fine while its PTR zone is stale or
     wrong, so this is a genuinely separate failure mode from a plain `A`
     test.
   - `Hostname4` / `Hostname6` — `domain`: the same FCrDNS round trip as
     `A-PTR`/`AAAA-PTR`, but against THIS running VM's own dynamically-
     registered hostname (`os.Hostname()` + `domain`) instead of a fixed
     name — proves dynamic DNS registration (both directions) actually
     works for this specific roaming host, not just that the resolver
     can answer one known-good static record.

   For segments expected to exit through a specific region (a WireGuard
   tunnel), `geoCheck` fetches a geo-IP echo URL and confirms the
   response mentions the right country (`geo`).
6. Confirm plain TCP reachability to a known external address over IPv4
   (`routing`), and over IPv6 too if `routingCheck6` is set (`routing6`)
   — proves the segment's actual egress uplink carries traffic, not just
   that DNS resolved.
7. Write the result to `/mseg-tester/<segment>.result.yaml` (overwritten
   each time this segment comes back around — the cycle itself is the
   time dimension, this file is always "most recent pass"). Its
   `version` field is `selfupdate.BuildInfo()`'s version string (a Go
   module pseudo-version whose last 12 hex characters are the commit
   this binary was built from, for anything short of a real tagged
   release) — the same string `mseg-tester version` prints, so any
   result (including whatever's pushed to `report.url`/`report.influx`)
   can always be traced back to an exact commit.
8. **Only if** the segment just tested IS `updateSegment` (the report
   segment, same condition as step 2) — every single visit,
   unconditionally, with nothing throttling this separately from step
   9's delay below:
   - update the local git checkout of `softwareRepo` at
     `internal/selfupdate.DefaultSrcDir` (`/mseg-tester/src`) — clone it
     first if it doesn't exist yet, then always `git fetch`+`git reset
     --hard` to `softwareRef`'s current tip (conflict-free by
     construction: local state is always discarded, never merged) — and
     only if that HEAD commit differs from the one THIS binary was
     itself built from (`selfupdate.BuildInfo().Revision`) rebuild via
     `go build`, replace itself, and re-exec its own `mseg-tester deploy`
     (`internal/selfupdate` — no GitHub release, no build pipeline, no
     asset to keep in sync, no Go module proxy involved at all). The
     redeploy step matters as much as the binary swap: `deploy` is what
     (re)writes `/etc/systemd/system/mseg-tester.service` from that
     build's own embedded unit file, so anything the new commit changed
     about how the service runs (e.g. `PATH` gaining `/snap/bin`) takes
     effect immediately instead of leaving an already-deployed box on a
     stale unit until it's rebuilt from scratch;
   - if `config.yaml` sets `report.url`, POST every accumulated
     `<segment>.result.yaml` there as JSON (`internal/report.Push`); if it
     sets `report.influx`, write them straight into an InfluxDB v2 bucket
     as line protocol instead/as well (`internal/report.PushInflux`).
     Either, both, or neither may be set.

   Every other segment skips all of this — there's nothing to reach
   GitHub or the report target(s) through.
9. **Unless `active.yaml`'s `stopOn` equals the segment just tested**:
   write netplan for the *next* segment in the cycle, advance
   `active.yaml`, sleep, and reboot — on **every** segment, not just
   `updateSegment`, so there's always a window to log in and inspect the
   box before it cycles away. Paid once *per segment*, so a full cycle's
   wall-clock time grows by roughly (segment count × delay).

   The sleep itself (`effectiveDelay` in `cmd/mseg-tester`) is
   `config.yaml`'s `rebootDelay` everywhere EXCEPT the report segment,
   which instead sleeps `report.wait.waitDelay` if `report.wait.on`
   names it (`internal/config.Wait`) — since nothing else throttles how
   often step 8's config-sync/self-update/report actually runs anymore,
   THIS delay is now the only thing keeping those infrequent: a short
   `rebootDelay` on the report segment would mean that whole block comes
   back around and re-runs every couple of minutes. `report.wait` is
   entirely optional; a report segment with no `report.wait` (or no
   `report` section at all) just uses the same plain `rebootDelay` as
   every other segment, running step 8 on every visit at that pace — an
   earlier design tried a SEPARATE elapsed-time throttle on top of this
   delay and found the two fought each other (see git history around
   `state.LastWait`/`updateSegmentThrottled` if curious); this delay
   alone is simpler and was chosen instead.

   `stopOn` is optional and not something `config.yaml`/cloud-init ever
   sets — hand-edit it into `/mseg-tester/active.yaml` (e.g. over
   SSH/console on whatever segment is currently reachable) when one
   specific segment needs sustained, live debugging rather than just the
   brief `rebootDelay` window before it cycles away again. Once set, the
   box parks on that segment on every subsequent boot (a manual reboot,
   a crash, etc.) until `stopOn` is edited back out or changed.

   If the next segment's interface can't be resolved right now (e.g. a
   `"wifi"` segment whose passthrough radio failed to probe this boot),
   that segment is **skipped**, not fatal — a failed
   `<segment>.result.yaml` (a single `"interface"` check) is recorded for
   it and the cycle advances to the segment after it instead. Bounded to
   one full lap of the cycle: if literally every segment fails to
   resolve, that's surfaced as a real error instead of rebooting forever.

   Netplan itself is written to `/etc/netplan/90-mseg-tester.yaml` (the
   one file this tool owns there), but `internal/netplan.Write` also
   keeps a per-segment copy alongside it —
   `/etc/netplan/90-mseg-tester.yaml.<segment>`, always overwritten with
   that segment's latest render — and `90-mseg-tester.yaml` itself is a
   hard link to whichever one is currently active. A debug aid: you can
   inspect exactly what was last written for any segment (e.g. "what did
   we generate for 130 three reboots ago") without waiting for it to
   come back around, or needing the box to still be reachable on that
   segment. These per-segment files deliberately don't end in `.yaml`
   themselves, so netplan's own `/etc/netplan/*.yaml` glob never picks
   them up as live config.

## Setup

**One-time, in Proxmox** (not automated — see "Why" above): create a small
VM, one NIC, set as a trunk port carrying VLANs 128/129/130/131. Attach
`cloud-init/user-data.yaml` as the VM's cloud-init user-data.

**Optional, also one-time, in Proxmox**: for any `type: wifi` segment, PCI
(or USB) passthrough a dedicated Wi-Fi radio to the VM (`hostpci0:
0000:xx:xx.x` in the VM's config) — needs IOMMU/VFIO already enabled on
the host, and the radio isolated in its own IOMMU group; check `lspci
-nnk` and `/sys/kernel/iommu_groups/` on the host first. Attaching the
device itself is NOT automated for a hand-built VM the way the rest of
this section describes (the trunk NIC isn't either — see "Why" above);
`cmd/verify-mseg-tester create` DOES automate it, though, deriving which
device(s) to passthrough straight from `-config-file`'s wifi segments'
`pciVendor`/`pciDevice` — see that section below. Once passed through, the
guest sees it as a normal wireless NIC — but PCI passthrough gives no
guarantee the guest names it the same thing on every boot, so pick one of
three ways to identify it in `config.yaml` rather than hardcoding a name:
a literal `segments[].ifname` (only safe if you've confirmed it's
stable), `segments[].mac`, or `segments[].pciVendor`+`segments[].pciDevice`
(read straight off the guest's `lspci -nn`, survives interface renames).
Leave all three unset and it auto-picks the first wireless interface it
finds — fine when there's only one radio passed through. Run `mseg-tester
find-iface` on the guest to check what any of these would resolve to
before committing it to `config.yaml` (see the Subcommands table below).

Before using it, edit `cloud-init/user-data.yaml`'s `bootstrap.yaml`
block:

- Replace every `TRUNK_IFACE` with this VM's actual guest NIC name
  (commonly `ens18` on Proxmox's default virtio NIC model — confirm with
  `ip link` if unsure).
- **Easy path (default)**: fill in the plain `config.yaml` write_files
  entry with real `dnsCheck`/`routingCheck`/`geoCheck` targets per
  segment (see `examples/config.yaml`), leave `bootstrap.yaml`'s
  `configRepo` empty. Nothing else to set up.
- **Or**, to manage `config.yaml` from git instead: set `configRepo` to
  that repo's URL and `configToken` to a fine-grained GitHub PAT,
  read-only, scoped to *only* that repo (leave `configToken` empty if
  the repo is public). Treat `configToken` with the same care as any
  other credential-bearing file.
- If any segment is `type: wifi`, also fill in its real `ssid` and the
  matching `WIFI_<segment name>_SSID`/`_PSK` pair in the `/mseg-tester/.env`
  write_files entry (cloud-init/user-data.yaml ships this pre-wired but
  with `REPLACE_ME` placeholders — they will **not** resolve on their own).

First boot brings up segment 129 directly (the only one cloud-init itself
can reach the internet through), installs the Go toolchain via `snap
install go --classic` (not an apt package or a hand-extracted tarball —
one less thing the bootstrap script has to manage itself; assumes snapd
is already present, which it is on stock Ubuntu Server cloud images),
clones `softwareRepo` into `/mseg-tester/src`, resets it to `softwareRef`,
builds the binary with a plain `go build`, runs `mseg-tester deploy`
(installs the systemd unit), and reboots into the first real cycle. If
`configRepo` is set, the unit's own first `run` also syncs `config.yaml`
from it.

## Releasing an update

There's no build pipeline, no release, no binary asset, and no Go module
proxy involved at all: pushing a commit is the whole release process —

```sh
git push origin main
```

— and the next time the cycle reaches `updateSegment`, every VM's local
checkout at `/mseg-tester/src` gets `git fetch`+`git reset --hard` to
`softwareRef`'s new tip (`internal/selfupdate`), compares that HEAD
commit against the one baked into the binary currently running
(`selfupdate.BuildInfo().Revision` — reliably populated because every
build, first boot and every self-update since, is a plain `go build`
inside a real git checkout, no `-ldflags` needed), and only rebuilds+
replaces itself if they differ. `softwareRef` (in `bootstrap.yaml`)
defaults to `"main"`.

This also sidesteps a real problem the old GitHub-Releases design would
eventually hit: checking `api.github.com` for a new release on every
VM's own cycle is subject to GitHub's unauthenticated REST API rate
limit (60 requests/hour per source IP). Plain `git fetch` against
GitHub's own git servers isn't rate-limited the same way.

Point `softwareRef` at your own branch or a commit SHA (`-module-ref` in
`cmd/verify-mseg-tester`) to run unreleased code with zero release step
at all.

## Subcommands

| Command | When it runs | What it does |
|---|---|---|
| `mseg-tester deploy` | Once, from the cloud-init bootstrap script, after the binary is first downloaded (and again after every self-update) | Copies itself to `/usr/local/bin`, writes and enables the systemd unit (`internal/deploy`) |
| `mseg-tester run [--bootstrap path] [--no-reboot] [--verbose]` | Every boot, via the systemd unit | The full cycle described above. `--bootstrap` defaults to `/mseg-tester/bootstrap.yaml`; `--no-reboot` prints the outcome instead of rebooting; `--verbose` logs every step and every check's pass/fail/detail as it happens |
| `mseg-tester render-netplan -config path -segment name [-trunk-iface name]` | Whenever you want to eyeball a segment's netplan | Prints exactly what `internal/netplan.Write` would install for that segment, from a local `config.yaml` — no VM, no network, no shell-on-the-box needed. Handy when a box is stuck at boot (e.g. `systemd-networkd-wait-online`) and unreachable: run this locally against the same `config.yaml` instead |
| `mseg-tester find-iface [-mac addr] [-pci-vendor id -pci-device id]` | On the guest, whenever you want to check what a `"wifi"` segment's `mac`/`pciVendor`+`pciDevice` (or auto-discovery, if none of the flags are given) would resolve to right now | Runs the same `internal/ifdiscover` resolution a `"wifi"` segment goes through at boot, and prints the resulting interface name (or the error you'd otherwise only see in a failed cycle's logs) — a fast way to sanity-check a MAC or PCI vendor/device pair against the guest's actual hardware before putting it in `config.yaml` |
| `mseg-tester version` (`-version`/`--version` also work) | Whenever you SSH/console into a box and want to know which commit it's actually running | Prints `selfupdate.BuildInfo()` — the same commit-identifying version string recorded into every `state.Result` (see `Version` below). Every build (first boot and every self-update since) happens as a plain `go build` inside a real local git checkout, so this is normally the full 40-character commit SHA (`Revision`) alongside a Go module pseudo-version (`Version`) whose last 12 hex characters are that same commit — no `-ldflags` needed, the Go toolchain stamps this automatically |

## `bootstrap.yaml` reference (`/mseg-tester/bootstrap.yaml`)

| Field | Meaning |
|---|---|
| `trunkInterface` | The guest NIC carrying every segment's VLAN tag |
| `updateSegment` | The one segment config-sync/self-update/report are attempted from |
| `softwareRepo` | `owner/repo`, assumed to be on `github.com` — turned into a git clone URL (`https://github.com/<softwareRepo>.git`) for the local checkout at `internal/selfupdate.DefaultSrcDir` (`/mseg-tester/src`), built directly with `go build`, no release step |
| `softwareRef` | Git branch/tag/commit the local checkout is `git fetch`+`git reset --hard` to — for both the first checkout and every later self-update. Defaults to `"main"` if empty |
| `configRepo` | Optional. URL of a repo to fetch/refresh the real `config.yaml` from, e.g. `https://github.com/owner/repo` (a bare `owner/repo` or the `git@github.com:owner/repo.git` SSH form also work). Empty (the default) means "just use the plain `config.yaml` cloud-init already wrote" — no repo, no token, nothing fetched |
| `configPath` | Path of `config.yaml` within `configRepo`. Ignored when `configRepo` is empty |
| `configRef` | Branch/tag/commit to fetch it at. Ignored when `configRepo` is empty |
| `configToken` | Fine-grained GitHub PAT, read-only, scoped to only `configRepo`. Leave empty if `configRepo` is empty or public |
| `stateDir` | Defaults to `/mseg-tester` |
| `configLocalPath` | Where `config.yaml` lives — either written directly by cloud-init, or fetched into, depending on `configRepo`. Defaults to `/mseg-tester/config.yaml` |
| `envFile` | Optional. Path to a simple `KEY=VALUE` `.env` file (`internal/envfile`) used to expand `"${VAR}"` references anywhere in `config.yaml`'s text before it's parsed — see below. Defaults to `/mseg-tester/.env`. Written once by cloud-init, 0600, and — like this file — never synced via `configRepo` |

Which segment (if any) is this trunk's native/untagged VLAN is declared in
`config.yaml` now, not here — see `segments[].type` below.

## `config.yaml` reference (see `examples/config.yaml`; either written directly by cloud-init or fetched from `configRepo` — same shape either way)

Any string value in this file may reference `"${VAR}"` — expanded at load
time against `bootstrap.yaml`'s `envFile` (or, missing that, the real
process environment; an unresolved reference is left untouched rather
than silently becoming empty). This is how `report.influx.token` below
can be a real secret without `config.yaml` itself ever holding one —
handy since `config.yaml` may be fetched from a shared or even public
`configRepo`, while the small `.env` file never leaves the VM.

| Field | Meaning |
|---|---|
| `rebootDelay` | Optional Go duration (e.g. `"2m"`) to wait, on every segment, before rebooting into the next one — gives a window to log in and inspect the box (e.g. a slow/hung boot) before it cycles away. Paid once per segment, so a full cycle's wall-clock time grows by roughly (segment count × `rebootDelay`). Omit for immediate everywhere. The report segment (`bootstrap.yaml`'s `updateSegment`) uses `report.wait.waitDelay` instead of this, if `report.wait.on` names it — see below |
| `checkAttempts` | Optional. How many times to run the WHOLE batch of checks before giving up — if ANY check fails, the whole batch (not just that check) is re-run. Stops as soon as one full attempt passes every check. Defaults to `3` if omitted |
| `checkRetryDelay` | Optional Go duration (e.g. `"10s"`) to wait before re-running the whole batch after a failure. Defaults to `"10s"` if omitted |
| `timezone` | Optional IANA zone name (e.g. `"Europe/Berlin"`), applied via `timedatectl set-timezone` at the start of every run — idempotent, best-effort (an invalid zone is logged, never fatal). Lives in `config.yaml` rather than `bootstrap.yaml` since it's content that can change without re-provisioning the VM. Omit to leave the base image's timezone (normally UTC) untouched |
| `report.url` | Optional. If set, every accumulated `<segment>.result.yaml` is POSTed here as JSON, only from `updateSegment` |
| `report.influx` | Optional `{url, org, bucket, token}` — writes the same accumulated results straight into an InfluxDB v2 bucket as line protocol instead (or as well). `token` must be a write-only token scoped to just `bucket`, typically written as `"${INFLUX_TOKEN}"` (see the `"${VAR}"` expansion note above) rather than a literal value — see `internal/report.PushInflux` and `examples/config.yaml` |
| `report.wait.on` | Optional — the one segment (normally `bootstrap.yaml`'s `updateSegment`) whose reboot-delay this replaces with `report.wait.waitDelay`. Lives under `report`, not its own top-level section, since config-sync/self-update/report are all the same updateSegment-only block and `wait` is fundamentally about that segment's timing. If omitted (no `report` section, or `report` with no `wait` subsection), that segment just uses the same plain `rebootDelay` as every other segment |
| `report.wait.waitDelay` | Optional Go duration (e.g. `"10m"`), only meaningful alongside `report.wait.on`. How long `report.wait.on`'s segment sleeps before advancing/rebooting into the next segment, in place of `rebootDelay` — since config-sync/self-update/report run unconditionally on every visit to that segment (step 8 above), THIS delay is what keeps those infrequent, by making visits themselves infrequent. Segment checks themselves are unaffected either way — they still run on every visit, whatever the delay. Defaults to `"10m"` if `report.wait.on` is set but this is omitted. See `internal/config.Wait` and `effectiveDelay` in `cmd/mseg-tester` |
| `segments[].name` | Both the cycle identifier and the VLAN ID |
| `segments[].type` | `"native"` (this trunk's untagged VLAN — arrives directly on `trunkInterface`, no 802.1Q tag), `"vlan"` (a normal tagged sub-interface), or `"wifi"` (a dedicated Wi-Fi radio passed through to this VM, associating to `ssid`/`psk` instead of riding any VLAN at all). Required on every segment; at most one may be `"native"`. Drives interface naming (`internal/netplan.IfaceName`) and, for `cmd/verify-mseg-tester`, whether `net0` gets `trunks=` at all — the single source of truth for "which segment is native", and (via `Config.VLANSegmentNames`) which segments are VLANs at all, since a `"wifi"` segment's `name` isn't a VLAN ID and must never end up in `trunks=`. If any segment is `"native"`, `net0` is created with **no** `tag=`/`trunks=` whatsoever (see `internal/verifyvm.Params.net0`'s doc comment: on an OVS bridge, Proxmox's `tag=` for a genuinely-untagged VLAN misroutes it — confirmed live) — every VLAN reaches the guest untouched and `internal/netplan.Write` does the demuxing on the guest side. A `"wifi"` segment cycles through `active.yaml`/`rebootDelay`/reporting exactly like any other segment — reassociating over Wi-Fi doesn't need a reboot the way switching VLANs does, but testing it via the same cold-boot cycle as everything else is deliberate, not a limitation. Whichever interface (trunk NIC or Wi-Fi radio) ISN'T this segment's own gets an explicit `activation-mode: off` in the written netplan, not just left out of the file — a passed-through radio's kernel network device exists as soon as its driver binds, independent of netplan entirely, so merely not mentioning it doesn't actually keep it off (confirmed live: this broke `internal/ifaces.Find`'s interface-counting heuristic once passthrough Wi-Fi started working). See `internal/netplan.Render`'s `disableWifiIfaces` doc comment |
| `segments[].ifname` | For `"native"`/`"vlan"`: optional, overrides the interface name `internal/netplan.IfaceName` would otherwise derive (`trunkInterface` for the native segment, `trunkInterface.<name>` for a tagged one). For `"wifi"`: optional — if set, used literally and takes priority over `mac`/`pciVendor`+`pciDevice`/auto-discovery below. There's no trunk-derived default for a standalone radio, so leave it unset unless you've confirmed the guest names it the same thing on every boot |
| `segments[].mac` | `"wifi"` only, optional — resolves to whichever interface currently has this MAC address (`internal/ifdiscover`), checked at every boot rather than assumed stable. Ignored if `ifname` is also set |
| `segments[].pciVendor` | `"wifi"` only, optional, must be paired with `pciDevice` (both set or both empty) — resolves to whichever interface is bound to the PCI device with this vendor ID (hex, e.g. `"14c3"`), read from the guest's `/sys/bus/pci/devices/*/vendor` (same values `lspci -nn` shows). Ignored if `ifname` is also set |
| `segments[].pciDevice` | `"wifi"` only, optional, must be paired with `pciVendor` — the PCI device ID (hex, e.g. `"0616"`) |
| `segments[].ssid` | Required for `"wifi"`, ignored otherwise — the network name to associate to |
| `segments[].psk` | Required for `"wifi"`, ignored otherwise — the network's password. Written as `"${VAR}"` and expanded the same way `report.influx.token` is (see the `"${VAR}"` expansion note above); this project's own convention is `WIFI_<segment name>_SSID`/`WIFI_<segment name>_PSK` in `.env`, e.g. `WIFI_128_SSID`/`WIFI_128_PSK` for a segment named `"wifi-128"` mirroring segment `"128"`'s checks — not enforced by code, just a naming convention |

A `"wifi"` segment's interface is resolved (`internal/ifdiscover`) in this
priority order, first match wins: literal `ifname`, then `mac`, then
`pciVendor`+`pciDevice`, then — if none of those are set — auto-discovery
of the first wireless interface found on the guest. Resolution happens
fresh on every attempt (both when writing netplan and when running
checks), not just once at boot, so a MAC/PCI-identified radio keeps
working even if the guest renames it between boots.
| `segments[].dnsCheck.servers` | List of servers every test in `tests` runs against by default (a test's own `servers` overrides this for just that test). Each entry is either `system` (the OS's default resolver) or a literal IP address, v4 or v6 (e.g. `192.168.130.5`, `fd00:192:168:130::5`, or a public resolver like `1.1.1.1`), dialed directly on port 53. At least one of `dnsCheck.servers` or every test's own `servers` is required |
| `segments[].dnsCheck.tests[].type` | `A` / `AAAA` — `host` resolves as that record, passes on any answer, no round trip. `A-PTR` / `AAAA-PTR` — `host` forward-resolves (A/AAAA), reverse-resolves the first answer, and expects the PTR name to equal `host` — a full FCrDNS round trip against a fixed name (e.g. a segment's gateway); replaces the old `reverseCheck`/`reverseCheck6`. `Hostname4` / `Hostname6` — `domain` (no `host`): the same FCrDNS round trip, but against THIS VM's own dynamically-registered name (`os.Hostname()` + `domain`) instead of a fixed one, proving dynamic DNS registration works for this roaming host; replaces the old `selfDnsDomain`. Only `Hostname4` is used anywhere yet — this network has no IPv6 DHCP to drive dynamic AAAA/PTR6 registration, so `Hostname6` would just spuriously fail. The address family tested (`A`/`AAAA` vs `A-PTR`/`AAAA-PTR` vs `Hostname4`/`Hostname6`) is always fixed by `type`, never by which server answers it |
| `segments[].dnsCheck.tests[].host` | Required for `A`/`AAAA`/`A-PTR`/`AAAA-PTR` — the name to resolve/round-trip |
| `segments[].dnsCheck.tests[].domain` | Required for `Hostname4`/`Hostname6` — just the domain (e.g. `"mam-hh-dmz.adviser.com."`), no hostname; `os.Hostname()` supplies that part at check time |
| `segments[].dnsCheck.tests[].servers` | Optional — overrides `dnsCheck.servers` for just this one test |
| `segments[].geoCheck` | Optional `{url, expect}` — omit to skip |
| `segments[].routingCheck` | `host:port` expected to be TCP-reachable (IPv4) |
| `segments[].routingCheck6` | Optional. IPv6 equivalent, e.g. `[2606:4700:4700::1111]:443` (brackets required) — omit to skip `routing6` |

## Grafana dashboard (`grafana/mseg-tester-dashboard.json`)

If `report.influx` is configured, a ready-to-import Grafana dashboard is
at `grafana/mseg-tester-dashboard.json`. It queries the two measurements
`internal/report.PushInflux` writes — `mseg_tester_result` (one line per
segment per cycle: `pass`, `updated`, `version`) and `mseg_tester_check`
(one line per individual check within that cycle: `pass`, `detail`) —
via Flux against an InfluxDB v2 datasource.

Import: Grafana → Dashboards → New → Import → upload the file → pick
your InfluxDB datasource in the `datasource` variable prompt (the
`bucket` variable defaults to `"mseg-tester"`, matching
`examples/config.yaml` — change it if yours differs). Panels: a
per-segment PASS/FAIL stat row, a table of each segment's latest run
(version/updated/age), a state-timeline of pass/fail history per
segment, a table of every check's latest result and failure detail, a
per-check state-timeline history, and an hourly pass-rate line (useful
for spotting a check that's flaky rather than fully broken).

## Manual testing

```sh
go run ./cmd/mseg-tester run --bootstrap /path/to/bootstrap.yaml --no-reboot
```

Runs one pass, prints the outcome, skips the reboot — useful for checking
a config change without actually cycling the machine. Point
`bootstrap.yaml`'s `configLocalPath` at a throwaway file and `stateDir` at
a scratch directory to avoid touching the real machine state.

## Verifying end-to-end on real hardware (`cmd/verify-mseg-tester`)

A separate, small Go tool that creates (and later destroys) a disposable
VM on any SSH-reachable Proxmox host, to exercise the whole cloud-init →
fetch → deploy → cycle pipeline for real rather than by inspection. Every
setting is a flag — no host, VMID, storage, bridge, or credential is
hardcoded, since this is meant to be published alongside mseg-tester and
run against any Proxmox host.

`create` and `destroy` are **dry-run by default**: they print exactly the
`qm`/`pvesm` commands they would run over SSH and make no connection at
all. Nothing touches the Proxmox host until you pass `-yes`.

```sh
go run ./cmd/verify-mseg-tester create \
  -host root@proxmox.example.com \
  -vmid 199 \
  -storage local-lvm \
  -image /var/lib/vz/template/iso/ubuntu-24.04-server-cloudimg-amd64.img \
  -bridge vmbr0 \
  -update-segment 129 \
  -software-repo mabels/mseg-tester \
  -config-file ./examples/config.yaml \
  -env-file ./.env \
  -ssh-key-file ~/.ssh/id_ed25519.pub
# prints the plan; re-run with -yes to actually create the VM

go run ./cmd/verify-mseg-tester destroy -host root@proxmox.example.com -vmid 199
# prints the plan; re-run with -yes to actually stop+purge it

go run ./cmd/verify-mseg-tester status -host root@proxmox.example.com -vmid 199
# read-only, runs immediately
```

Notes:

- `-bridge` must already be VLAN-aware on the Proxmox host (Linux bridge:
  `bridge-vlan-aware yes`; OVS bridge: works implicitly) — that, like
  mseg-tester's own trunk-NIC requirement, stays a one-time manual
  prerequisite, never automated.
- There are no `-trunk-vlans`/`-native-segment` flags — the Proxmox NIC's
  trunked VLAN list and, if any, its native/untagged VLAN are both derived
  automatically from `-config-file`'s `segments[].name`/`.type`
  (`internal/config.Config.VLANSegmentNames`/`.NativeSegmentName`), the
  same `type: native`/`type: vlan` fields that drive the guest-side
  interface naming (`internal/netplan.IfaceName`) — one source of truth
  instead of two lists to keep in sync by hand. `VLANSegmentNames`
  deliberately excludes any `type: wifi` segments (their `name` isn't a
  VLAN ID and never rides the trunk NIC at all — see
  `segments[].type` above); the full segment list including `wifi` ones
  still seeds the guest's reboot cycle via `Config.CycleNames`, just not
  the Proxmox trunk. If you use `-config-repo` instead (config.yaml
  fetched at runtime, not given as a local file), the trunk is left fully
  untagged (every VLAN passes) since there's no local segment list to
  derive from at create-time.
- Likewise, there's no `-hostpci` flag — any `type: wifi` segment in
  `-config-file` that identifies its radio by `pciVendor`+`pciDevice` is
  passed straight through to the new VM automatically
  (`internal/config.Config.WifiPCIDevices`, deduplicated — this project's
  own wifi-128/130/131 all share one physical card, so that's one
  `hostpci0`, not three). Unlike every other setting `create` derives,
  this genuinely can't be resolved in a dry run: the vendor:device ID
  pair is stable hardware identity, but which PCI *bus address* it's
  actually sitting at right now is a live fact about the Proxmox host, so
  those steps only run (one `lspci -n -d` lookup + `qm set --hostpciN`
  per distinct device, over ssh) when `-yes` is given — a dry-run plan
  just shows that the step exists, not the address it'll resolve to. Wifi
  segments identified by `ifname`/`mac`/auto-discovery instead contribute
  nothing here (there's no PCI ID to look an address up for) — passthrough
  for those still has to be set up by hand, same as the manual-VM path in
  Setup above. Whenever `-config-file` has at least one such device,
  `create` also adds `--machine q35` to the VM itself (in addition to
  `-bios ovmf`'s own unconditional q35) — Proxmox refuses hostpciN's
  `pcie=1` on the default i440fx machine type ("q35 machine model is not
  enabled"), confirmed live the hard way (`qm start` failing after an
  otherwise-successful `create`). `-bios seabios` (the default) plus q35
  is a normal combination and needs no `--efidisk0`; only `-bios ovmf`
  adds that.
- `-vga` defaults to `"std"` — a normal graphical console, viewable in
  Proxmox's regular noVNC "Console" tab. Pass `-vga serial0` instead to
  put boot output/login on the serial console only (`qm terminal`, or
  Proxmox's noVNC "Serial Console" tab) — or any other `qm`/QEMU vga
  type: `cirrus`, `qxl`, `virtio`, ... `--serial0 socket` is added
  unconditionally either way, so `qm terminal` keeps working regardless
  of which one is picked; `-vga` only chooses which is the PRIMARY
  display during boot (GRUB, kernel messages, getty).
- If any segment is `type: native`, `net0` is generated with **no**
  `tag=`/`trunks=` at all, not `tag=<native>,trunks=<rest>` — on an OVS
  bridge, giving the untagged segment an explicit `tag=` routes it
  through OVS's native-untagged VLAN handling instead of leaving it
  alone, which silently breaks delivery for that one segment (confirmed
  live; see `internal/verifyvm.Params.net0`'s doc comment for the full
  story). A plain, no-VLAN-params NIC passes every VLAN through
  untouched, matching how the physical uplink port itself is normally
  configured on that bridge.
- `config.yaml` needs to come from either `-config-file` (a plain local
  file — no repo or token needed, the easy path shown above) or
  `-config-repo` (fetched/refreshed at runtime instead;
  `-config-token`/`-config-token-file` only matter if that repo is
  private). At least one of the two is required; both may be given.
  `-config-file`'s content is **not** deployed byte-for-byte: any
  `"${VAR}"` reference in it (e.g. `report.influx.token`) is substituted
  against `-env-file` right now, at create time, before being embedded
  into cloud-init — the file that lands at `/mseg-tester/config.yaml` on
  the VM already has real values in it, it does not depend on
  `/mseg-tester/.env` existing and being read correctly on first
  boot before it resolves. Runtime substitution (`internal/envfile`,
  driven by the *deployed* `.env`) still also happens on every
  `mseg-tester run` — this matters for `-config-repo`, where there's no
  local file to substitute at create time, and as a second pass in case a
  later config-sync reintroduces a placeholder this create-time pass
  never saw.
- `-module-ref` sets `bootstrap.yaml`'s `softwareRef` — the git
  branch/tag/commit the bootstrap script's local checkout (and every
  later self-update) tracks `-software-repo` at. Defaults to `"main"`.
  Point it at your own branch or a commit SHA to exercise unreleased
  code end-to-end — no GitHub release, no build pipeline, no
  binary-hosting side channel needed at all.
- The rendered cloud-init is the same bootstrap
  `cloud-init/user-data.yaml` uses, plus an `ubuntu` user with
  NOPASSWD sudo and (if `-ssh-key-file` is given) your public key — the
  one deliberate difference from the production cloud-init, which has no
  user/SSH access at all.
- `-console-password` (or `-console-password-file`, preferred) sets a
  plaintext password for the `ubuntu` user, for logging in on Proxmox's
  serial/VNC console independent of SSH — useful before SSH is even up,
  or if you skipped `-ssh-key-file`. If neither is given, a `CONSOLE_PASSWORD`
  entry in `-env-file` is used instead, if present — so the one local
  `.env` file can hold this alongside e.g. `INFLUX_TOKEN` without a
  separate `-console-password-file` to pass. Leave all three unset to
  keep the account password-locked. This does **not** enable SSH password
  auth; SSH still requires `-ssh-key-file`'s key either way.
- `-config-token-file` (not `-config-token`) is the safe way to pass a
  private repo's PAT — the direct flag form ends up in shell history and
  `ps` output. The same reasoning applies to `-console-password-file`
  over `-console-password`.
- `-env-file` deploys a local `.env` file (`KEY=VALUE`, see
  `internal/envfile`) to `/mseg-tester/.env` on the guest, `0600`.
  This is what actually resolves `config.yaml`'s `"${VAR}"` references
  (e.g. `report.influx.token`) at runtime and feeds the `CONSOLE_PASSWORD`
  fallback above — without it, those references are only ever resolved
  from the shell's own environment when `mseg-tester run` happens to have
  one (normally it won't, running under systemd), so the placeholder
  stays literal. **Defaults to `".env"` in the current directory** — read
  automatically if present, silently skipped if not (the common case: no
  `.env` next to where you run `create`). Pass `-env-file ""` to disable
  entirely, or point it at another path explicitly — unlike the default,
  an explicitly-named file that doesn't exist is a hard error, so a typo
  is never silently ignored. Never fetched from `-config-repo` or synced
  anywhere — like `bootstrap.yaml` itself, it's local-only and
  provisioned once, by hand, per VM.

## Write-ups

- [Simple Wishes Are Sometimes Very Complex](https://mabels.github.io/mseg-tester/blog/simple-wishes-are-sometimes-very-complex.html) —
  the motivation (why autodiscovery testing across VLANs and Wi-Fi never had a
  clean solution), the reboot-cycling sequencer design, and where building it
  actually got hard. Source: [docs/blog/simple-wishes-are-sometimes-very-complex.md](docs/blog/simple-wishes-are-sometimes-very-complex.md).

## License

Apache License 2.0 — see `LICENSE`.
