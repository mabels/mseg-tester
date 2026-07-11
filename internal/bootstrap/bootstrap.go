// Package bootstrap reads /mseg-tester/bootstrap.yaml -- the small,
// rarely-changing set of facts that have to be known BEFORE any config
// can be fetched from anywhere: which local NIC is the VLAN trunk, which
// segment can actually reach the internet, and where the two GitHub repos
// this tool depends on are.
//
// This is deliberately separate from internal/config.Config, which is
// fetched FROM the private repo named here and can change often
// (segment list, test targets, timing, reporting) without ever touching
// this file. bootstrap.yaml is written once by cloud-init and is the one
// thing that can't itself live in a repo this tool fetches -- it's what
// tells the tool where to look.
//
// Lives under /mseg-tester (not /etc/mseg-tester, where it used to live)
// -- one directory holds everything this tool ever touches on a VM
// (bootstrap.yaml, .env, config.yaml, active.yaml, results), nothing
// under /etc at all, deliberately: two directories to remember (and back
// up, and inspect) for what's genuinely one small set of local state was
// never worth it, since nothing here actually depended on FHS's
// static-vs-variable distinction in practice.
package bootstrap

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Bootstrap is the whole of /mseg-tester/bootstrap.yaml.
type Bootstrap struct {
	// TrunkInterface is the guest NIC carrying every segment's VLAN tag.
	TrunkInterface string `yaml:"trunkInterface"`
	// UpdateSegment is the only segment with a route to the internet --
	// self-update and config-sync are only ever attempted from here. May
	// or may not also be config.yaml's native segment (config.Segment.Type
	// == "native"); both are handled -- see internal/netplan.IfaceName.
	UpdateSegment string `yaml:"updateSegment"`
	// SoftwareRepo is "owner/repo" (assumed to be on github.com) for this
	// tool's own PUBLIC source -- internal/selfupdate turns it into a git
	// clone URL ("https://github.com/" + SoftwareRepo + ".git") for a
	// local checkout it builds from directly (`go build`, not `go
	// install` -- see internal/selfupdate's package doc). No GitHub
	// release or build pipeline is involved: any commit pushed to this
	// repo is buildable.
	SoftwareRepo string `yaml:"softwareRepo"`
	// SoftwareRef is the git branch/tag/commit the local checkout at
	// internal/selfupdate.DefaultSrcDir tracks -- both for the very
	// first checkout (see the cloud-init bootstrap script, which reads
	// this file directly) and every subsequent self-update
	// (internal/selfupdate: `git fetch`+`git reset --hard` to this ref's
	// current tip, every time the update segment comes around). Defaults
	// to "main" if left empty. Point this at your own branch or a commit
	// SHA to run unreleased code with no build/release step at all --
	// see cmd/verify-mseg-tester's -module-ref.
	SoftwareRef string `yaml:"softwareRef,omitempty"`
	// ConfigRepo is OPTIONAL -- the URL of a PRIVATE repo holding the
	// real, content-level config.yaml (segments, timing, reporting),
	// e.g. "https://github.com/owner/repo" (a bare "owner/repo" and the
	// "git@github.com:owner/repo.git" SSH form are also accepted -- see
	// internal/configsync.ownerRepoFromURL).
	//
	// Leave this empty (the default, and the "make it easy first" path)
	// to skip config-sync entirely and rely purely on a plain
	// config.yaml provisioned directly by cloud-init at ConfigLocalPath
	// -- no repo, no token, nothing to fetch at runtime. Set it once
	// you actually want config.yaml to live in git and refresh on its
	// own schedule instead of requiring a VM re-provision to change.
	ConfigRepo string `yaml:"configRepo"`
	// ConfigPath is the path of config.yaml WITHIN ConfigRepo. Ignored
	// when ConfigRepo is empty.
	ConfigPath string `yaml:"configPath"`
	// ConfigRef is the branch/tag/commit to fetch ConfigPath at. Ignored
	// when ConfigRepo is empty.
	ConfigRef string `yaml:"configRef"`
	// ConfigToken is a fine-grained GitHub PAT, read-only, scoped to
	// ONLY ConfigRepo -- provisioned by cloud-init the same deliberate
	// way ovn-fabric's WireGuard PrivateKey lives in a git-tracked file
	// (see that project's types.ts): a real credential accepted as
	// living here on purpose, not overlooked. Treat this file with the
	// same care as any other credential-bearing file. Leave empty if
	// ConfigRepo is empty, or if ConfigRepo is itself public.
	ConfigToken string `yaml:"configToken"`
	// StateDir -- see internal/state. Defaults to /mseg-tester.
	StateDir string `yaml:"stateDir"`
	// ConfigLocalPath is where the fetched config.yaml is written --
	// defaults to /mseg-tester/config.yaml, alongside StateDir's
	// active.yaml/*.result.yaml rather than under /etc -- this is the one
	// file on the box that changes every time a segment cycles back
	// around (self-update/config-sync/report all touch state here on
	// UpdateSegment) -- this and bootstrap.yaml now share one directory
	// (/mseg-tester) on disk, but remain conceptually distinct: this
	// changes constantly, bootstrap.yaml essentially never does.
	ConfigLocalPath string `yaml:"configLocalPath"`
	// EnvFile is OPTIONAL -- path to a simple "KEY=VALUE" .env file
	// (internal/envfile) used to expand "${VAR}" references anywhere in
	// config.yaml's text (e.g. report.influx.token) before it's parsed.
	// Defaults to "/mseg-tester/.env" if empty. Written once by
	// cloud-init, 0600, and -- like bootstrap.yaml itself -- NEVER synced
	// via ConfigRepo: this is the one place actual secrets live on disk,
	// kept separate from config.yaml, which may be shared or even public.
	// Missing entirely is fine too (an env file is optional); "${VAR}"
	// references then fall back to the real process environment, or are
	// left untouched if that's unset as well (see envfile.Expand).
	EnvFile string `yaml:"envFile,omitempty"`
}

const defaultPath = "/mseg-tester/bootstrap.yaml"

func Load(path string) (Bootstrap, error) {
	if path == "" {
		path = defaultPath
	}
	var b Bootstrap
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("bootstrap: reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("bootstrap: parsing %s: %w", path, err)
	}
	if b.StateDir == "" {
		b.StateDir = "/mseg-tester"
	}
	if b.ConfigLocalPath == "" {
		b.ConfigLocalPath = "/mseg-tester/config.yaml"
	}
	if b.SoftwareRef == "" {
		b.SoftwareRef = "main"
	}
	if b.EnvFile == "" {
		b.EnvFile = "/mseg-tester/.env"
	}
	// ConfigRepo is deliberately NOT required here -- see its doc comment
	// above: empty means "use the plain config.yaml cloud-init already
	// wrote, no fetching."
	if b.TrunkInterface == "" || b.UpdateSegment == "" {
		return b, fmt.Errorf("bootstrap: %s missing trunkInterface/updateSegment", path)
	}
	return b, nil
}
