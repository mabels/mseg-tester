// Command verify-mseg-tester creates or destroys a disposable VM on any
// SSH-reachable Proxmox host, to exercise mseg-tester's cloud-init +
// binary end-to-end on real hardware rather than by inspection.
//
// Every setting -- the Proxmox host, VMID, storage, bridge, VLANs,
// software/config repos, credentials -- is a command-line flag; nothing
// here is specific to any one environment, since this tool is meant to
// be published alongside mseg-tester itself and run against any Proxmox
// host.
//
// `create` and `destroy` are dry-run by default: they print exactly the
// remote commands they would run and make no connection to the Proxmox
// host at all. Pass -yes to actually execute the plan.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/envfile"
	"github.com/mabels/mseg-tester/internal/sshrun"
	"github.com/mabels/mseg-tester/internal/verifyvm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "create":
		runCreate(os.Args[2:])
	case "destroy":
		runDestroy(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "verify-mseg-tester: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `verify-mseg-tester -- create or destroy a disposable VM on a Proxmox
host to verify mseg-tester's cloud-init + binary end-to-end.

Usage:
  verify-mseg-tester create  -host <user@proxmox> -vmid <id> [flags...] [-yes]
  verify-mseg-tester destroy -host <user@proxmox> -vmid <id> [-yes]
  verify-mseg-tester status  -host <user@proxmox> -vmid <id>

Without -yes, create/destroy only print the remote commands they would
run and never connect to the Proxmox host. Run "verify-mseg-tester create
-h" or "... destroy -h" for the full flag list.
`)
}

func commonFlags(fs *flag.FlagSet, p *verifyvm.Params) {
	fs.StringVar(&p.Host, "host", "", "SSH target for the Proxmox host, e.g. root@proxmox.example.com (required)")
	fs.Func("ssh-opt", "extra ssh option, repeatable, e.g. -ssh-opt -p -ssh-opt 2222", func(v string) error {
		p.SSHOpts = append(p.SSHOpts, v)
		return nil
	})
	fs.IntVar(&p.VMID, "vmid", 0, "Proxmox VMID to use (required)")
	fs.StringVar(&p.Name, "name", "verify-mseg-tester", "VM name/hostname")
}

func runCreate(args []string) {
	var p verifyvm.Params
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	commonFlags(fs, &p)

	fs.StringVar(&p.Storage, "storage", "", "Proxmox storage ID for the VM disk (required)")
	fs.StringVar(&p.Image, "image", "", "path to a cloud-init-ready disk image, ON the Proxmox host (required)")
	fs.StringVar(&p.Bridge, "bridge", "", "Proxmox bridge name (required)")
	fs.IntVar(&p.Cores, "cores", 2, "vCPUs")
	fs.IntVar(&p.MemoryMB, "memory", 1024, "memory in MB")
	fs.StringVar(&p.DiskSize, "disk-size", "8G", "disk size after import, passed to `qm resize`")
	fs.StringVar(&p.BIOS, "bios", "seabios", `"seabios" or "ovmf"`)
	fs.BoolVar(&p.Onboot, "onboot", false, "start this VM automatically when the Proxmox host itself boots")
	fs.BoolVar(&p.Start, "start", true, "start the VM once created")
	fs.StringVar(&p.SnippetsStorage, "snippets-storage", "local", "storage ID that holds cloud-init snippets")
	fs.StringVar(&p.SnippetsPath, "snippets-path", "/var/lib/vz/snippets", "filesystem path to that storage's snippets dir, ON the Proxmox host")

	fs.StringVar(&p.TrunkIface, "trunk-iface", "ens18", "NIC name inside the guest")
	fs.StringVar(&p.UpdateSegment, "update-segment", "", "the one VLAN/segment with internet access (required)")
	fs.StringVar(&p.SoftwareRepo, "software-repo", "", "owner/repo (on github.com) `go install` builds mseg-tester from -- no release needed (required)")
	configFile := fs.String("config-file", "", "path to a plain local config.yaml to deploy as-is -- no private repo or token needed (the easy path; see examples/config.yaml). Also read to derive the trunk's VLAN list and native segment (config.Segment.Type/IfName -- see internal/config), so -trunk-vlans/-native-segment no longer exist as separate flags. Required unless -config-repo is set")
	fs.StringVar(&p.ConfigRepo, "config-repo", "", "URL of a private (or public) repo to fetch/refresh config.yaml from at runtime instead, e.g. https://github.com/owner/repo. Required unless -config-file is set")
	fs.StringVar(&p.ConfigPath, "config-path", "config.yaml", "path of config.yaml within config-repo")
	fs.StringVar(&p.ConfigRef, "config-ref", "main", "branch/tag/commit to fetch config.yaml at")
	configToken := fs.String("config-token", "", "config-repo's PAT, given directly, if it's private (prefer -config-token-file: this appears in argv/shell history/process list)")
	configTokenFile := fs.String("config-token-file", "", "path to a local file containing the fine-grained PAT for config-repo, if it's private")
	envFile := fs.String("env-file", ".env", "path to a local .env file (KEY=VALUE, see internal/envfile) to deploy to /etc/mseg-tester/.env on the guest, 0600 -- this is what lets -config-file's \"${VAR}\" references (e.g. report.influx.token) and a CONSOLE_PASSWORD entry actually resolve/apply, instead of needing a manual copy onto the VM. Defaults to \".env\" in the current directory: read automatically if it exists there, silently skipped if not. Set to \"\" to disable entirely, or point at another path explicitly -- an explicitly-named file that doesn't exist is a hard error (unlike the default), so a typo here is never silently ignored. Never fetched from -config-repo")
	sshKeyFile := fs.String("ssh-key-file", "", "path to a local SSH public key file to authorize for the 'ubuntu' user (recommended, for inspecting the VM)")
	fs.StringVar(&p.SoftwareRef, "module-ref", "", "git branch/tag/commit the bootstrap script's `go install` (and every later self-update) builds -software-repo from. Defaults to \"latest\" (the newest semver tag). Point at your own branch or a commit SHA to exercise unreleased code -- no GitHub release or build pipeline needed (\"test without gh\")")
	consolePassword := fs.String("console-password", "", "plaintext password for the 'ubuntu' user, for logging in on Proxmox's serial/VNC console independent of SSH (prefer -console-password-file, or a CONSOLE_PASSWORD entry in -env-file: this flag appears in argv/shell history/process list). Leave everything unset to leave the account password-locked -- SSH via -ssh-key-file is unaffected either way")
	consolePasswordFile := fs.String("console-password-file", "", "path to a local file containing the plaintext console password. If neither this nor -console-password is set, falls back to a CONSOLE_PASSWORD entry in -env-file")

	yes := fs.Bool("yes", false, "actually connect to the Proxmox host and run these commands (default: print the plan only)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	p.ConfigToken = *configToken
	if *configTokenFile != "" {
		b, err := os.ReadFile(*configTokenFile)
		if err != nil {
			log.Fatalf("verify-mseg-tester: reading -config-token-file: %v", err)
		}
		p.ConfigToken = strings.TrimSpace(string(b))
	}
	// -env-file defaults to ".env" -- track whether the user actually typed
	// -env-file themselves (vs just getting the default) so a missing
	// default file is a silent no-op (the common case: no .env in this
	// directory, nothing to deploy) while a missing EXPLICITLY-named file
	// is still a hard error (a typo shouldn't silently deploy nothing).
	envFileExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "env-file" {
			envFileExplicit = true
		}
	})
	var envVars map[string]string
	if *envFile != "" {
		b, err := os.ReadFile(*envFile)
		if err != nil {
			if os.IsNotExist(err) && !envFileExplicit {
				b = nil // default ".env" isn't present -- nothing to deploy, not an error
			} else {
				log.Fatalf("verify-mseg-tester: reading -env-file: %v", err)
			}
		}
		if b != nil {
			p.EnvFile = string(b)

			envVars, err = envfile.Load(*envFile)
			if err != nil {
				log.Fatalf("verify-mseg-tester: parsing -env-file: %v", err)
			}
		}
	}
	if *configFile != "" {
		b, err := os.ReadFile(*configFile)
		if err != nil {
			log.Fatalf("verify-mseg-tester: reading -config-file: %v", err)
		}
		// Substitute "${VAR}" references (e.g. report.influx.token) against
		// envVars (from -env-file) RIGHT NOW, at create time, before this
		// content is embedded into cloud-init -- not just left for the
		// deployed VM's own runtime `mseg-tester run` to resolve later.
		// Both still happen (belt and suspenders: this also covers a
		// -config-repo-fetched config.yaml later re-syncing placeholders
		// runtime substitution alone wouldn't have caught yet), but the
		// version actually written to /mseg-tester/config.yaml on the VM
		// now has real values baked in immediately, rather than depending
		// on /etc/mseg-tester/.env existing and being read correctly on
		// first boot before anything (e.g. the very first report push)
		// needs it resolved.
		p.ConfigYAML = envfile.Expand(string(b), envVars)

		// Derive the trunk's VLAN list and native segment from config.yaml
		// itself (config.Segment.Type/IfName) rather than separate
		// -trunk-vlans/-native-segment flags -- one source of truth,
		// nothing to keep in sync by hand. See internal/config's package
		// doc and internal/netplan.IfaceName. envVars (from -env-file, if
		// given) is passed here too so this validation parse resolves
		// "${VAR}" references (e.g. report.influx.token) exactly the same
		// way the deployed VM's own mseg-tester run eventually will --
		// still falls back to this shell's own environment when envVars is
		// nil (no -env-file given), same as config.Load's doc comment
		// describes.
		parsedCfg, err := config.Load(*configFile, envVars)
		if err != nil {
			log.Fatalf("verify-mseg-tester: parsing -config-file: %v", err)
		}
		p.TrunkVLANs = parsedCfg.VLANSegmentNames()
		if native, ok := parsedCfg.NativeSegmentName(); ok {
			p.NativeSegment = native
		}
	}
	if *sshKeyFile != "" {
		b, err := os.ReadFile(*sshKeyFile)
		if err != nil {
			log.Fatalf("verify-mseg-tester: reading -ssh-key-file: %v", err)
		}
		p.SSHAuthorizedKey = strings.TrimSpace(string(b))
	}
	p.ConsolePassword = *consolePassword
	if *consolePasswordFile != "" {
		b, err := os.ReadFile(*consolePasswordFile)
		if err != nil {
			log.Fatalf("verify-mseg-tester: reading -console-password-file: %v", err)
		}
		p.ConsolePassword = strings.TrimSpace(string(b))
	} else if p.ConsolePassword == "" {
		// Neither -console-password nor -console-password-file given --
		// fall back to CONSOLE_PASSWORD in -env-file, if present, so the
		// one local .env file can hold this secret too instead of needing
		// yet another -console-password-file passed on the command line.
		if v, ok := envVars["CONSOLE_PASSWORD"]; ok {
			p.ConsolePassword = v
		}
	}
	if err := p.ValidateCreate(); err != nil {
		log.Fatalf("verify-mseg-tester: %v", err)
	}

	steps, err := p.BuildCreatePlan()
	if err != nil {
		log.Fatalf("verify-mseg-tester: %v", err)
	}
	runPlan(p, steps, *yes)
}

func runDestroy(args []string) {
	var p verifyvm.Params
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	commonFlags(fs, &p)
	fs.StringVar(&p.SnippetsPath, "snippets-path", "/var/lib/vz/snippets", "filesystem path to the snippets dir, ON the Proxmox host (must match what create used)")
	fs.BoolVar(&p.KeepSnippets, "keep-snippets", false, "don't remove the cloud-init snippet files create uploaded")
	yes := fs.Bool("yes", false, "actually connect to the Proxmox host and run these commands (default: print the plan only)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if err := p.ValidateCommon(); err != nil {
		log.Fatalf("verify-mseg-tester: %v", err)
	}
	runPlan(p, p.BuildDestroyPlan(), *yes)
}

func runStatus(args []string) {
	var p verifyvm.Params
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	commonFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if err := p.ValidateCommon(); err != nil {
		log.Fatalf("verify-mseg-tester: %v", err)
	}
	// Read-only -- runs unconditionally, no -yes gate needed.
	runner := sshrun.New(p.Host, p.SSHOpts)
	for _, step := range p.BuildStatusPlan() {
		out, err := runner.Run(step.Command, step.Stdin)
		fmt.Printf("== %s ==\n%s\n", step.Description, out)
		if err != nil {
			log.Fatalf("verify-mseg-tester: %v", err)
		}
	}
}

func runPlan(p verifyvm.Params, steps []verifyvm.Step, yes bool) {
	fmt.Printf("Plan for VM %d (%s) on %s:\n\n", p.VMID, p.Name, p.Host)
	for i, step := range steps {
		fmt.Printf("%2d. %s\n", i+1, step.Description)
		if step.Command != "" {
			fmt.Printf("    ssh %s %s\n", p.Host, step.Command)
		}
	}
	fmt.Println()

	if !yes {
		fmt.Println("Dry run only -- no connection made. Re-run with -yes to apply.")
		return
	}

	runner := sshrun.New(p.Host, p.SSHOpts)
	for i, step := range steps {
		fmt.Printf("--> [%d/%d] %s\n", i+1, len(steps), step.Description)
		var (
			out string
			err error
		)
		if step.Action != nil {
			err = step.Action(runner)
		} else {
			out, err = runner.Run(step.Command, step.Stdin)
		}
		if out != "" {
			fmt.Println(out)
		}
		if err != nil {
			log.Fatalf("verify-mseg-tester: step %d (%s) failed: %v", i+1, step.Description, err)
		}
	}
	fmt.Println("Done.")
}
