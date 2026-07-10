// Command mseg-tester is a single self-contained binary meant to run once
// per boot on a small, otherwise-unremarkable Ubuntu Server VM whose one
// NIC is trunked with every segment's VLAN tag. Subcommands:
//
//	mseg-tester render-netplan -- offline debug helper: prints exactly what
//	                        internal/netplan.Write would install for one
//	                        segment, from a local config.yaml, no VM or
//	                        network access needed at all. See renderNetplanCmd.
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
//	                             internal/checks), retrying the WHOLE batch
//	                             up to config.CheckAttempts times if any
//	                             single check in it failed,
//	                          4. records the result,
//	                          5. on the update segment: rebuilds itself via
//	                             `go install` straight from source and
//	                             replaces its own executable if the result
//	                             differs (internal/selfupdate), then POSTs
//	                             every accumulated result to
//	                             config.Report.URL if set (internal/report),
//	                          6. unless active.yaml's StopOn equals the
//	                             segment just tested (see
//	                             state.Active.StopOn): writes netplan for
//	                             the NEXT segment in the cycle, waits
//	                             config.RebootDelay (every segment, not
//	                             just the update one -- gives a login
//	                             window before it cycles away), reboots.
//	                             StopOn is hand-edited into active.yaml to
//	                             park the cycle on one segment for
//	                             sustained live debugging instead.
//
//	-verbose logs each of the above steps, and every individual check's
//	pass/fail/detail, as they happen -- handy when watching a live serial
//	console rather than only reading the final <segment>.result.yaml
//	afterward.
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
	"github.com/mabels/mseg-tester/internal/envfile"
	"github.com/mabels/mseg-tester/internal/netplan"
	"github.com/mabels/mseg-tester/internal/report"
	"github.com/mabels/mseg-tester/internal/selfupdate"
	"github.com/mabels/mseg-tester/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("mseg-tester: expected a subcommand: \"deploy\", \"run\", or \"render-netplan\"")
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
	case "render-netplan":
		renderNetplanCmd(args)
	default:
		log.Fatalf("mseg-tester: unknown subcommand %q -- expected \"deploy\", \"run\", or \"render-netplan\"", subcommand)
	}
}

// renderNetplanCmd prints exactly what internal/netplan.Write would install
// for one segment, without touching disk or needing to be run on a real
// VM at all -- lets a stuck/hung box's netplan be inspected offline from a
// local config.yaml, e.g. while the VM itself is unreachable (no lease yet,
// stuck at systemd-networkd-wait-online).
func renderNetplanCmd(args []string) {
	fs := flag.NewFlagSet("render-netplan", flag.ExitOnError)
	configFile := fs.String("config", "", "path to a local config.yaml (required)")
	segmentName := fs.String("segment", "", "the segments[].name to render (required)")
	trunkIface := fs.String("trunk-iface", "ens18", "trunk NIC name, matching bootstrap.yaml's trunkInterface")
	_ = fs.Parse(args)

	if *configFile == "" || *segmentName == "" {
		log.Fatalf("mseg-tester: render-netplan requires -config and -segment")
	}
	cfg, err := config.Load(*configFile, nil)
	if err != nil {
		log.Fatalf("mseg-tester: %v", err)
	}
	seg, ok := cfg.BySegmentName(*segmentName)
	if !ok {
		log.Fatalf("mseg-tester: segment %q not declared in %s", *segmentName, *configFile)
	}
	fmt.Print(netplan.Render(*trunkIface, seg))
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	bootstrapPath := fs.String("bootstrap", "/etc/mseg-tester/bootstrap.yaml", "path to bootstrap.yaml")
	noReboot := fs.Bool("no-reboot", false, "run one pass and print the result instead of rebooting (manual testing)")
	verbose := fs.Bool("verbose", false, "log every step and every check's pass/fail/detail as it happens, not just the final summary -- handy when watching a live console")
	_ = fs.Parse(args)

	if err := run(*bootstrapPath, *noReboot, *verbose); err != nil {
		log.Fatalf("mseg-tester: %v", err)
	}
}

func run(bootstrapPath string, noReboot, verbose bool) error {
	boot, err := bootstrap.Load(bootstrapPath)
	if err != nil {
		return err
	}
	if verbose {
		log.Printf("run: bootstrap loaded from %s: trunkInterface=%s updateSegment=%s softwareRepo=%s",
			bootstrapPath, boot.TrunkInterface, boot.UpdateSegment, boot.SoftwareRepo)
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
	if verbose {
		log.Printf("run: active segment=%s firstBoot=%v", active.Segment, firstBoot)
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
		if verbose {
			log.Printf("run: syncing config.yaml from %s", boot.ConfigRepo)
		}
		if err := configsync.Fetch(boot); err != nil {
			log.Printf("mseg-tester: config sync: %v", err)
		}
	}

	envVars, err := envfile.Load(boot.EnvFile)
	if err != nil {
		return fmt.Errorf("loading env file: %w", err)
	}
	if verbose && len(envVars) > 0 {
		log.Printf("run: loaded %d var(s) from %s for \"${VAR}\" expansion in %s", len(envVars), boot.EnvFile, boot.ConfigLocalPath)
	}

	cfg, err := config.Load(boot.ConfigLocalPath, envVars)
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

	attempts, retryDelay := cfg.CheckAttemptsOrDefault(), cfg.CheckRetryDelayOrDefault()
	if verbose {
		log.Printf("run: checking segment %s (checkAttempts=%d checkRetryDelay=%s)", seg.Name, attempts, retryDelay)
	}
	result := state.Result{
		Segment:   seg.Name,
		Timestamp: time.Now().UTC(),
		Checks:    checks.Run(seg, attempts, retryDelay, verbose),
		Version:   currentVersion,
	}

	if active.Segment == boot.UpdateSegment {
		applyUpdateCheck(boot, &result, verbose)
	}

	if err := state.SaveResult(boot.StateDir, result); err != nil {
		return fmt.Errorf("saving result: %w", err)
	}

	if active.Segment == boot.UpdateSegment && cfg.Report != nil {
		if cfg.Report.URL != "" {
			if verbose {
				log.Printf("run: pushing accumulated results to %s", cfg.Report.URL)
			}
			if err := report.Push(cfg.Report.URL, boot.StateDir); err != nil {
				log.Printf("mseg-tester: report push: %v", err)
			}
		}
		if cfg.Report.Influx != nil {
			if verbose {
				log.Printf("run: pushing accumulated results to influxdb %s (org=%s bucket=%s)",
					cfg.Report.Influx.URL, cfg.Report.Influx.Org, cfg.Report.Influx.Bucket)
			}
			if err := report.PushInflux(*cfg.Report.Influx, boot.StateDir); err != nil {
				log.Printf("mseg-tester: influx report push: %v", err)
			}
		}
	}

	// StopOn (state.Active.StopOn) parks the cycle here instead of
	// advancing/rebooting -- this run's own checks/result/self-update/
	// report above already happened as normal; only the step that would
	// move on to the NEXT segment is skipped. See state.Active.StopOn's
	// doc comment for how this gets set (hand-edited into active.yaml
	// while the cycle is already running) and why (sustained live
	// debugging on one specific segment, e.g. over SSH/console, instead
	// of only the brief RebootDelay window before it cycles away again).
	if active.StopOn != "" && active.StopOn == active.Segment {
		if verbose {
			log.Printf("run: stopOn %q reached -- staying on segment %s, not advancing or rebooting", active.StopOn, active.Segment)
		}
		return nil
	}

	next := active.Next()
	nextSeg, ok := cfg.BySegmentName(next)
	if !ok {
		return fmt.Errorf("next segment %q not declared in %s", next, boot.ConfigLocalPath)
	}
	if verbose {
		log.Printf("run: writing netplan for next segment %s", next)
	}
	if err := netplan.Write(boot.TrunkInterface, nextSeg); err != nil {
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

	// RebootDelay now applies on EVERY segment, not just updateSegment --
	// deliberately changed from the original "updateSegment only" design
	// (see config.Config.RebootDelay's doc comment) so there's a real
	// window to SSH/console into the box and inspect it before it cycles
	// away, on WHATEVER segment turns out to need debugging (e.g. a slow
	// or hung boot on a non-update segment -- the whole reason this was
	// changed). The cost: total per-cycle wall-clock time is now
	// (segment count x RebootDelay), not paid just once -- pick a value
	// that's fine to pay per segment, not just once per cycle.
	if delay := cfg.RebootDelayDuration(); delay > 0 {
		if verbose {
			log.Printf("run: sleeping %s before reboot (rebootDelay, every segment)", delay)
		}
		time.Sleep(delay)
	}

	if verbose {
		log.Printf("run: rebooting into segment %s", next)
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
func applyUpdateCheck(boot bootstrap.Bootstrap, result *state.Result, verbose bool) {
	modulePath := fmt.Sprintf("github.com/%s/cmd/mseg-tester", boot.SoftwareRepo)
	if verbose {
		log.Printf("run: checking for update: go install %s@%s", modulePath, boot.SoftwareRef)
	}
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
