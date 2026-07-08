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

- **`/etc/mseg-tester/bootstrap.yaml`** — local, rarely changes, written
  once by cloud-init: which NIC is the trunk, which segment can reach the
  internet (`updateSegment`), where the public software repo is, and
  (optionally) where a private config repo is.
- **`/mseg-tester/config.yaml`** — the content: segment list, test
  targets, reboot timing, where to report results. Lives alongside
  `active.yaml`/`*.result.yaml` rather than under `/etc`, since it's the
  one file here that genuinely changes on its own schedule. Two ways to
  get it onto the VM, pick either:
  - **easy (the default)**: cloud-init just writes it directly — no
    repo, no token. Changing it means editing `cloud-init/user-data.yaml`
    and re-provisioning.
  - set `bootstrap.yaml`'s `configRepo` to a repo URL holding this file
    and it's fetched/refreshed at runtime instead (`internal/configsync`)
    — changing it is then a commit, not a re-provision.

  See `examples/config.yaml` for the shape either way.

## How a cycle works

1. Read `/mseg-tester/active.yaml` — which segment is active right now.
2. **Only if** the active segment is `updateSegment` AND `bootstrap.yaml`'s
   `configRepo` is set: fetch the latest `config.yaml` from that repo
   (best-effort — a failed fetch just leaves the last successfully-synced,
   or cloud-init-provisioned, `config.yaml` in place). Skipped entirely
   when `configRepo` is empty.
3. Confirm the VLAN sub-interface for that segment actually has a
   non-link-local IPv4 address, **and** a global (non-link-local) IPv6
   address (`dhcp`/`dhcp6` — DHCP/SLAAC already ran via netplan before
   this binary starts, see the systemd unit's
   `After=network-online.target`; this step confirms the *outcome*, it
   doesn't run its own DHCP client or send its own router solicitation).
4. Resolve a configured record directly against that segment's own DNS
   server over IPv4 (`dns`), and over IPv6 too if `dnsServer6` is set
   (`dns6`) — then, if `reverseCheck`/`reverseCheck6` are set, confirm
   that same server also answers PTR queries correctly (`reverse`,
   `reverse6`; a resolver's forward zone can be fine while its PTR zone
   is stale or wrong, so this is a genuinely separate failure mode) —
   and, for segments expected to exit through a specific region (a
   WireGuard tunnel), fetch a geo-IP echo URL and confirm the response
   mentions the right country (`geo`).
5. Confirm plain TCP reachability to a known external address over IPv4
   (`routing`), and over IPv6 too if `routingCheck6` is set (`routing6`)
   — proves the segment's actual egress uplink carries traffic, not just
   that DNS resolved.
6. Write the result to `/mseg-tester/<segment>.result.yaml` (overwritten
   each time this segment comes back around — the cycle itself is the
   time dimension, this file is always "most recent pass").
7. **Only if** the segment just tested is `updateSegment`:
   - rebuild itself via `go install` straight from `softwareRepo`'s
     source and replace itself if the result differs (`internal/selfupdate`
     — no GitHub release, no build pipeline, no asset to keep in sync);
   - if `config.yaml` sets `report.url`, POST every accumulated
     `<segment>.result.yaml` there (`internal/report`).

   Every other segment skips both — there's nothing to reach the module
   proxy/GitHub or the report target through.
8. Write netplan for the *next* segment in the cycle, advance
   `active.yaml`, and reboot — **only** on `updateSegment`, first sleep
   `config.yaml`'s `rebootDelay` (if set); every other segment reboots
   immediately, so the delay is paid once per full cycle, not once per
   segment.

## Setup

**One-time, in Proxmox** (not automated — see "Why" above): create a small
VM, one NIC, set as a trunk port carrying VLANs 128/129/130/131. Attach
`cloud-init/user-data.yaml` as the VM's cloud-init user-data.

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

First boot brings up segment 129 directly (the only one cloud-init itself
can reach the internet through), builds the binary via `go install`
(cloud-init installs the `golang-go` package for this — see `packages:`),
runs `mseg-tester deploy` (installs the systemd unit), and reboots into
the first real cycle. If `configRepo` is set, the unit's own first `run`
also syncs `config.yaml` from it.

## Releasing an update

There's no build pipeline, no release, no binary asset: `go install
github.com/<softwareRepo>/cmd/mseg-tester@<softwareRef>` builds straight
from source, both for the first install (the cloud-init bootstrap
script) and every later self-update (`internal/selfupdate`). Pushing a
commit is the whole release process —

```sh
git push origin main
```

— and the next time the cycle reaches `updateSegment`, every VM rebuilds
itself and picks it up. `softwareRef` (in `bootstrap.yaml`) defaults to
`"latest"` (the newest semver tag); tag a commit `vX.Y.Z` and push the
tag if you want that discipline, or leave it on `"latest"` tracking a
branch's tip — either works, `go install` resolves both.

This also sidesteps a real problem the old GitHub-Releases design would
eventually hit: checking `api.github.com` for a new release on every
VM's own cycle is subject to GitHub's unauthenticated REST API rate
limit (60 requests/hour per source IP). `go install` talks to the Go
module proxy (or git directly) instead, which isn't rate-limited the
same way.

Point `softwareRef` at your own branch or a commit SHA (`-module-ref` in
`cmd/verify-mseg-tester`) to run unreleased code with zero release step
at all.

## Subcommands

| Command | When it runs | What it does |
|---|---|---|
| `mseg-tester deploy` | Once, from the cloud-init bootstrap script, after the binary is first downloaded | Copies itself to `/usr/local/bin`, writes and enables the systemd unit (`internal/deploy`) |
| `mseg-tester run [--bootstrap path] [--no-reboot]` | Every boot, via the systemd unit | The full cycle described above. `--bootstrap` defaults to `/etc/mseg-tester/bootstrap.yaml`; `--no-reboot` prints the outcome instead of rebooting |

## `bootstrap.yaml` reference (`/etc/mseg-tester/bootstrap.yaml`)

| Field | Meaning |
|---|---|
| `trunkInterface` | The guest NIC carrying every segment's VLAN tag |
| `nativeSegment` | Optional. The one segment (if any) that's this trunk's native/UNTAGGED VLAN — it arrives directly on `trunkInterface`, not on a `trunkInterface.<segment>` sub-interface. Leave empty if every segment is a normal tagged VLAN |
| `updateSegment` | The one segment config-sync/self-update/report are attempted from |
| `softwareRepo` | `owner/repo`, assumed to be on `github.com` — built into a Go module path (`github.com/<softwareRepo>/cmd/mseg-tester`) that `go install` builds directly, no release step |
| `softwareRef` | Git branch/tag/commit `go install` builds — for both the first install and every self-update. Defaults to `"latest"` (the newest semver tag) if empty |
| `configRepo` | Optional. URL of a repo to fetch/refresh the real `config.yaml` from, e.g. `https://github.com/owner/repo` (a bare `owner/repo` or the `git@github.com:owner/repo.git` SSH form also work). Empty (the default) means "just use the plain `config.yaml` cloud-init already wrote" — no repo, no token, nothing fetched |
| `configPath` | Path of `config.yaml` within `configRepo`. Ignored when `configRepo` is empty |
| `configRef` | Branch/tag/commit to fetch it at. Ignored when `configRepo` is empty |
| `configToken` | Fine-grained GitHub PAT, read-only, scoped to only `configRepo`. Leave empty if `configRepo` is empty or public |
| `stateDir` | Defaults to `/mseg-tester` |
| `configLocalPath` | Where `config.yaml` lives — either written directly by cloud-init, or fetched into, depending on `configRepo`. Defaults to `/mseg-tester/config.yaml` |

## `config.yaml` reference (see `examples/config.yaml`; either written directly by cloud-init or fetched from `configRepo` — same shape either way)

| Field | Meaning |
|---|---|
| `rebootDelay` | Optional Go duration (e.g. `"6m"`) to wait, only on `updateSegment`, before rebooting into the next segment. Every other segment always reboots immediately. Omit for immediate everywhere |
| `report.url` | Optional. If set, every accumulated `<segment>.result.yaml` is POSTed here as JSON, only from `updateSegment` |
| `segments[].name` | Both the cycle identifier and the VLAN ID |
| `segments[].dnsServer` / `.dnsCheck` | Resolver to query (IPv4), record expected to resolve |
| `segments[].dnsServer6` | Optional. Same resolver's IPv6 address, e.g. `fd00:192:168:129::5` — runs the same `dnsCheck` record over IPv6 (`dns6`); omit to skip |
| `segments[].reverseCheck` | Optional `{ip, expect}` — confirms `dnsServer` also answers a PTR query for `ip` with `expect` (`reverse`); omit to skip |
| `segments[].reverseCheck6` | Optional `{ip, expect}` — `reverseCheck`'s IPv6 counterpart, queried against `dnsServer6` (`reverse6`); skipped if `dnsServer6` is empty even when this is set |
| `segments[].geoCheck` | Optional `{url, expect}` — omit to skip |
| `segments[].routingCheck` | `host:port` expected to be TCP-reachable (IPv4) |
| `segments[].routingCheck6` | Optional. IPv6 equivalent, e.g. `[2606:4700:4700::1111]:443` (brackets required) — omit to skip `routing6` |

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
  -trunk-vlans 128,129,130,131 \
  -native-segment 128 \
  -update-segment 129 \
  -software-repo mabels/mseg-tester \
  -config-file ./examples/config.yaml \
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
- `-trunk-vlans` restricts which VLANs the NIC trunks; omit it for a
  fully-untagged NIC that passes every VLAN.
- `-native-segment` marks one of `-trunk-vlans` as the port's
  native/untagged VLAN (Proxmox `net0`'s `tag=`) instead of a tagged one
  — matches `bootstrap.yaml`'s `nativeSegment` on the guest side
  (`internal/netplan.IfaceName`). Must also appear in `-trunk-vlans` if
  that's non-empty. Omit if every segment is a normal tagged VLAN.
- `config.yaml` needs to come from either `-config-file` (a plain local
  file deployed as-is — no repo or token needed, the easy path shown
  above) or `-config-repo` (fetched/refreshed at runtime instead;
  `-config-token`/`-config-token-file` only matter if that repo is
  private). At least one of the two is required; both may be given.
- `-module-ref` sets `bootstrap.yaml`'s `softwareRef` — the git
  branch/tag/commit the bootstrap script's `go install` (and every later
  self-update) builds `-software-repo` from. Defaults to `"latest"`.
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
  or if you skipped `-ssh-key-file`. Leave both unset to keep the account
  password-locked. This does **not** enable SSH password auth; SSH still
  requires `-ssh-key-file`'s key either way.
- `-config-token-file` (not `-config-token`) is the safe way to pass a
  private repo's PAT — the direct flag form ends up in shell history and
  `ps` output. The same reasoning applies to `-console-password-file`
  over `-console-password`.

## License

Apache License 2.0 — see `LICENSE`.
