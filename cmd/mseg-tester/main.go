// Command mseg-tester is a single self-contained binary meant to run once
// per boot on a small, otherwise-unremarkable Ubuntu Server VM whose one
// NIC is trunked with every segment's VLAN tag. Two subcommands:
//
//	mseg-tester deploy   -- one-time (and idempotent-to-repeat) install:
//	                        copy this executable to /usr/local/bin, write
//	                        and enable the systemd unit. See internal/deploy.
//	                        This is what cloud-init runs, once, after
//	                        downloading the binary -- everything about how
//	                        this tool wires itself into systemd lives here,
//	                        not duplicated into a separate cloud-init template.
//	mseg-tester run      -- what the systemd unit actually executes, once
//	                        per boot:
//	                          1. reads which segment is currently active,
//	                          2. if this IS the update segment (the only
//	                             one with a route anywhere outside the
//	                             segment under test), refreshes config.yaml
//	                             from the private repo (internal/configsync),
//	                          3. checks the active segment (DHCP address
//	                             present, DNS answers, optionally a geo
//	                             check, plain routing reachability -- see
//	                             internal/checks),
//	                          4. records the result,
//	                          5. on the update segment: rebuilds itself via
//	                             `go install` straight from source and
//	                             replaces its own executable if the result
//	                             differs (internal/selfupdate), then POSTs
//	                             every accumulated result to
//	                             config.Report.URL if set (internal/report),
//	                          6. writes netplan for the NEXT segment in the
//	                             cycle, waits config.RebootDelay, reboots.
//
// Two configuration files, split by how often each changes -- see the
// package docs on internal/bootstrap and internal/config for why:
//
//	/etc/mseg-tester/bootstrap.yaml -- local, rare, written once by
//	                                   cloud-init (trunk NIC, update
//	                                   segment, where the two repos are).
//	/mseg-tester/config.yaml        -- content, frequent, fetched from
//	                                   the private repo named in
//	                                   bootstrap.yaml (segment list, test
//	                                   targets, timing, reporting).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/mabels/mseg-tester/internal/bootstrap"
	"github.com/mabels/mseg-tester/internal/checks"
	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/configsync"
	"github.com/mabels/mseg-tester/internal/deploy"
	"github.com/mabels/mseg-tester/internal/netplan"
	"github.com/mabels/mseg-tester/internal/report"
	"github.com/mabels/mseg-tester/internal/selfupdate"
	"github.com/mabels/mseg-tester/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("mseg-tester: expected a subcommand: \"deploy\" or \"run\"")
	}
	subcommand := os.Args[1]
	args := os.Args[2:]

	switch subcommand {
	case "deploy":
		if err := deploy.Run(); err != nil {
			log.Fatalf("mseg-tester: %v", err)
		}
	case "run":
		runCmd(args)
	default:
		log.Fatalf("mseg-tester: unknown subcommand %q -- expected \"deploy\" or \"run\"", subcommand)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	bootstrapPath := fs.String("bootstrap", "/etc/mseg-tester/bootstrap.yaml", "path to bootstrap.yaml")
	noReboot := fs.Bool("no-reboot", false, "run one pass and print the result instead of rebooting (manual testing)")
	_ = fs.Parse(args)

	if err := run(*bootstrapPath, *noReboot); err != nil {
		log.Fatalf("mseg-tester: %v", err)
	}
}

func run(bootstrapPath string, noReboot bool) error {
	boot, err := bootstrap.Load(bootstrapPath)
	if err != nil {
		return err
	}

	active, err := state.LoadActive(boot.StateDir)
	firstBoot := false
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("loading active state: %w", err)
		}
		// First boot: cloud-init brings the VM up on boot.UpdateSegment
		// directly (see cloud-init/user-data.yaml) -- treat that as this
		// run's segment until config.yaml is loaded below and the real
		// cycle can be seeded.
		firstBoot = true
		active = state.Active{Segment: boot.UpdateSegment}
	}

	// Config-sync (like self-update and reporting below) is only ever
	// attempted from boot.UpdateSegment -- every other segment has no
	// route to api.github.com. It's also entirely OPTIONAL: an empty
	// boot.ConfigRepo means "just use the plain config.yaml cloud-init
	// already wrote" (see bootstrap.Bootstrap.ConfigRepo's doc comment)
	// -- the "make it easy first" path with no private repo or token at
	// all. A failed sync (when ConfigRepo IS set) is not fatal: whatever
	// config.yaml is already on disk (from a previous successful sync,
	// or the one cloud-init provisioned) is left in place.
	if active.Segment == boot.UpdateSegment && boot.ConfigRepo != "" {
		if err := configsync.Fetch(boot); err != nil {
			log.Printf("mseg-tester: config sync: %v", err)
		}
	}

	cfg, err := config.Load(boot.ConfigLocalPath)
	if err != nil {
		return err
	}

	if firstBoot {
		names := cfg.CycleNames()
		active = state.Active{Segment: names[0], Cycle: names}
		if err := state.SaveActive(boot.StateDir, active); err != nil {
			return fmt.Errorf("seeding active state: %w", err)
		}
	}

	seg, ok := cfg.BySegmentName(active.Segment)
	if !ok {
		return fmt.Errorf("active segment %q not declared in %s", active.Segment, boot.ConfigLocalPath)
	}

	// Best-effort: a content hash of the running binary, meaningful even
	// though there's no semver tag baked in via -ldflags anymore (see
	// internal/selfupdate). A failure here (very unlikely -- just reads
	// and hashes this same executable) is worth recording, not fatal.
	currentVersion, verErr := selfupdate.CurrentVersion()
	if verErr != nil {
		log.Printf("mseg-tester: reading current version: %v", verErr)
	}

	result := state.Result{
		Segment:   seg.Name,
		Timestamp: time.Now().UTC(),
		Checks:    checks.Run(seg, boot.TrunkInterface, boot.NativeSegment),
		Version:   currentVersion,
	}

	if active.Segment == boot.UpdateSegment {
		applyUpdateCheck(boot, &result)
	}

	if err := state.SaveResult(boot.StateDir, result); err != nil {
		return fmt.Errorf("saving result: %w", err)
	}

	if active.Segment == boot.UpdateSegment && cfg.Report != nil && cfg.Report.URL != "" {
		if err := report.Push(cfg.Report.URL, boot.StateDir); err != nil {
			log.Printf("mseg-tester: report push: %v", err)
		}
	}

	next := active.Next()
	if err := netplan.Write(boot.TrunkInterface, next, boot.NativeSegment); err != nil {
		return fmt.Errorf("writing netplan for next segment %s: %w", next, err)
	}
	if err := state.SaveActive(boot.StateDir, state.Active{Segment: next, Cycle: active.Cycle}); err != nil {
		return fmt.Errorf("advancing active state to %s: %w", next, err)
	}

	if noReboot {
		fmt.Printf("dry run: segment %s checked (pass=%v), next would be %s, not rebooting\n",
			seg.Name, result.Pass(), next)
		return nil
	}

	// RebootDelay only ever applies on updateSegment -- every other
	// segment has nothing to wait for (no self-update/config-sync/report
	// happens there, see applyUpdateCheck above) and should cycle straight
	// through to the next reboot as fast as possible. Pausing there too
	// would multiply the whole cycle's wall-clock time by the segment
	// count for no reason.
	if active.Segment == boot.UpdateSegment {
		if delay := cfg.RebootDelayDuration(); delay > 0 {
			time.Sleep(delay)
		}
	}

	// A self-update above may have just replaced this executable on
	// disk -- re-exec'ing here would pick that up sooner, but reboot is
	// what actually proves the NEXT segment's netplan/DHCP config works
	// from a genuinely cold boot, which is this whole tool's point.
	// Always reboot, updated or not.
	return exec.Command("systemctl", "reboot").Run()
}

// applyUpdateCheck is only ever called when the active segment IS
// boot.UpdateSegment -- see run() above. A failed build/replace is
// recorded as its own failed check, not fatal: this segment's own
// DHCP/DNS/routing results are still valid and still worth keeping, and
// missing one update just means trying again next time this segment
// comes back around in the cycle.
func applyUpdateCheck(boot bootstrap.Bootstrap, result *state.Result) {
	modulePath := fmt.Sprintf("github.com/%s/cmd/mseg-tester", boot.SoftwareRepo)
	up, err := selfupdate.CheckAndApply(modulePath, boot.SoftwareRef)
	if err != nil {
		result.Checks = append(result.Checks, state.CheckResult{
			Name: "selfupdate", Pass: false, Detail: err.Error(),
		})
		return
	}
	detail := fmt.Sprintf("already at %s@%s", modulePath, boot.SoftwareRef)
	if up.Applied {
		result.Updated = true
		detail = fmt.Sprintf("installed %s@%s", modulePath, boot.SoftwareRef)
	}
	result.Checks = append(result.Checks, state.CheckResult{Name: "selfupdate", Pass: true, Detail: detail})
}
