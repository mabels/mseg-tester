// Package bootstrap reads /etc/mseg-tester/bootstrap.yaml -- the small,
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
package bootstrap

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Bootstrap is the whole of /etc/mseg-tester/bootstrap.yaml.
type Bootstrap struct {
	// TrunkInterface is the guest NIC carrying every segment's VLAN tag.
	TrunkInterface string `yaml:"trunkInterface"`
	// NativeSegment is OPTIONAL -- the one segment (if any) that arrives
	// on TrunkInterface UNTAGGED, i.e. Proxmox's net0 "tag=" VLAN rather
	// than one of its "trunks=" list. A trunk port carries some number of
	// tagged VLANs PLUS, usually, one native/untagged VLAN -- on this
	// project's real network that's segment "128" (the home network):
	// its traffic is NOT on a TrunkInterface.128 sub-interface at all,
	// it's directly on TrunkInterface itself. Every OTHER segment is a
	// normal 802.1Q-tagged VLAN sub-interface (TrunkInterface.<segment>).
	//
	// Leave empty (the default) if every segment is a tagged VLAN and
	// there's no native/untagged one at all.
	NativeSegment string `yaml:"nativeSegment,omitempty"`
	// UpdateSegment is the only segment with a route to the internet --
	// self-update and config-sync are only ever attempted from here.
	// May or may not be the same as NativeSegment; both are handled.
	UpdateSegment string `yaml:"updateSegment"`
	// SoftwareRepo is "owner/repo" (assumed to be on github.com) for this
	// tool's own PUBLIC source -- internal/selfupdate builds it into a Go
	// module path ("github.com/" + SoftwareRepo + "/cmd/mseg-tester") and
	// runs `go install` against it directly. No GitHub release or build
	// pipeline is involved: any commit pushed to this repo is installable.
	SoftwareRepo string `yaml:"softwareRepo"`
	// SoftwareRef is the git branch/tag/commit `go install` fetches --
	// both for the very first install (see the cloud-init bootstrap
	// script, which reads this file directly) and every subsequent
	// self-update (internal/selfupdate). Defaults to "latest" (the
	// newest semver tag) if left empty. Point this at a branch or commit
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
	// defaults to /etc/mseg-tester/config.yaml.
	ConfigLocalPath string `yaml:"configLocalPath"`
}

const defaultPath = "/etc/mseg-tester/bootstrap.yaml"

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
		b.ConfigLocalPath = "/etc/mseg-tester/config.yaml"
	}
	if b.SoftwareRef == "" {
		b.SoftwareRef = "latest"
	}
	// ConfigRepo is deliberately NOT required here -- see its doc comment
	// above: empty means "use the plain config.yaml cloud-init already
	// wrote, no fetching."
	if b.TrunkInterface == "" || b.UpdateSegment == "" {
		return b, fmt.Errorf("bootstrap: %s missing trunkInterface/updateSegment", path)
	}
	return b, nil
}
