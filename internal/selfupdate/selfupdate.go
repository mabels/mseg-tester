// Package selfupdate keeps a local git checkout of this tool's own
// source up to date and, when its HEAD commit differs from the one the
// currently-running executable was built from, rebuilds and replaces
// itself, then re-runs its own "deploy" subcommand so
// /etc/systemd/system/mseg-tester.service is rewritten from the NEW
// build too -- a self-update that only swapped the binary but left a
// stale unit file behind (e.g. still pointing PATH at a Go install
// method the new build no longer expects) would silently strand
// already-deployed VMs until someone fixed the unit by hand. Only ever
// called when the active segment is bootstrap.Bootstrap.UpdateSegment --
// see cmd/mseg-tester.
//
// Deliberately git + `go build`, not `go install <module>@<ref>` (an
// earlier design this replaced) -- git gives a cheap, local way to
// answer "is there anything new" (compare two commit SHAs) BEFORE
// spending a `go build`, rather than always building speculatively and
// only finding out afterward via a content hash. It also means no
// dependency on the Go module proxy's own resolution/caching behavior
// for a "@branch" ref, and the same git checkout directory is what the
// very first boot's bootstrap script (cloud-init) creates too -- one
// mechanism, not two: see cloud-init/user-data.yaml's
// mseg-tester-bootstrap.sh.
package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// DefaultSrcDir is where the local git checkout of this tool's own
// source lives -- alongside bootstrap.yaml/config.yaml/active.yaml/etc.,
// nothing under /etc (see internal/bootstrap's package doc).
const DefaultSrcDir = "/mseg-tester/src"

// Result reports what happened, for the caller to fold into
// state.Result.Updated -- never an error just because no update was
// needed; that's the common case, not a failure.
type Result struct {
	Applied bool
}

// gitClone, gitFetchReset, gitRevParseHEAD, and goBuild are variables,
// not plain functions, so tests can fake every external command without
// a real git checkout, network round trip, or Go toolchain invocation.

// gitClone clones repoURL into dir -- only ever called once, the first
// time this VM runs (dir doesn't exist yet). A shallow clone: this tool
// only ever needs to BUILD from HEAD, never inspect history, and a
// full clone's history is pure disk growth for no benefit on a small
// VM (see gw-to-earth-ng's own disk-growth notes for why that's not a
// hypothetical concern here).
var gitClone = func(repoURL, dir string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", repoURL, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %w: %s", err, out)
	}
	return nil
}

// gitFetchReset fetches ref from origin and force-resets dir's working
// tree to exactly match it -- "conflict-free" by construction: `reset
// --hard` discards any local state instead of ever attempting a merge,
// so this can never get stuck needing manual conflict resolution on a
// box nobody's watching. ref may be a branch, tag, or commit SHA --
// GitHub supports fetching an arbitrary reachable commit SHA directly,
// not just named refs.
var gitFetchReset = func(dir, ref string) error {
	fetch := exec.Command("git", "-C", dir, "fetch", "--depth", "1", "origin", ref)
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, out)
	}
	reset := exec.Command("git", "-C", dir, "reset", "--hard", "FETCH_HEAD")
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w: %s", err, out)
	}
	return nil
}

// gitRevParseHEAD returns dir's current HEAD commit SHA-1, full 40 hex
// characters -- compared directly against BuildInfo().Revision (see
// checkAndApply).
var gitRevParseHEAD = func(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// goBuild builds ./cmd/mseg-tester from srcDir into out -- a plain `go
// build`, not `go install`, since srcDir already IS the checked-out
// source tree; there's nothing to fetch from a module proxy for the
// main module itself, only its dependencies (cached after the first
// build, same as before).
var goBuild = func(srcDir, out string) error {
	cmd := exec.Command("go", "build", "-o", out, "./cmd/mseg-tester")
	cmd.Dir = srcDir
	cmd.Env = os.Environ()
	if o, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w: %s", err, o)
	}
	return nil
}

// runDeploy re-execs self's own "deploy" subcommand right after
// replaceSelf has renamed a freshly-built binary over it -- this is what
// keeps /etc/systemd/system/mseg-tester.service current across a
// self-update, not just the binary. internal/deploy.Run() is idempotent
// and always rewrites the unit file from ITS build's own go:embed'd
// content (see internal/deploy/mseg-tester.service), so any change that
// shipped in the new commit -- e.g. the PATH gaining /snap/bin once Go
// moved from a hand-extracted tarball to `snap install go --classic` --
// takes effect immediately, without waiting for a full VM rebuild or a
// human hand-editing the unit over SSH. Runs as a fresh subprocess of the
// NEW binary (not a plain function call into the currently-executing OLD
// process) specifically so it picks up the NEW build's embedded unit
// content, not the stale one already loaded into this process's memory.
var runDeploy = func(self string) error {
	cmd := exec.Command(self, "deploy")
	cmd.Env = os.Environ()
	if o, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy: %w: %s", err, o)
	}
	return nil
}

// CheckAndApply ensures srcDir is a clean checkout of repoURL at ref's
// current HEAD (cloning it first if srcDir doesn't exist yet -- belt
// and suspenders alongside cloud-init's own first-boot clone, in case
// srcDir was ever removed by hand) and, if that HEAD commit differs
// from the currently-running executable's own baked-in commit (see
// BuildInfo), builds and replaces it. ref is a git branch/tag/commit,
// defaulting to "main" if empty (see bootstrap.Bootstrap.SoftwareRef).
func CheckAndApply(srcDir, repoURL, ref string) (Result, error) {
	self, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: locating current executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: resolving %s: %w", self, err)
	}
	return checkAndApply(srcDir, repoURL, ref, self, BuildInfo().Revision)
}

// checkAndApply is CheckAndApply with self (the executable to compare
// against and, if stale, overwrite) and currentRevision (normally
// BuildInfo().Revision) passed in explicitly -- kept separate so tests
// can point self at a throwaway file and currentRevision at a
// controlled value, instead of depending on the real, running test
// binary's own build info (which a `go test` invocation can't fake).
func checkAndApply(srcDir, repoURL, ref, self, currentRevision string) (Result, error) {
	if ref == "" {
		ref = "main"
	}

	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return Result{}, fmt.Errorf("selfupdate: checking %s: %w", srcDir, err)
		}
		if err := gitClone(repoURL, srcDir); err != nil {
			return Result{}, fmt.Errorf("selfupdate: cloning %s into %s: %w", repoURL, srcDir, err)
		}
	}

	if err := gitFetchReset(srcDir, ref); err != nil {
		return Result{}, fmt.Errorf("selfupdate: updating checkout at %s: %w", srcDir, err)
	}

	head, err := gitRevParseHEAD(srcDir)
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: reading HEAD in %s: %w", srcDir, err)
	}

	// Empty currentRevision (build info unavailable) is treated as
	// "different" -- always rebuild rather than risk skipping a
	// genuinely-needed update just because we couldn't prove the two
	// are the same.
	if currentRevision != "" && currentRevision == head {
		return Result{Applied: false}, nil
	}

	buildDir, err := os.MkdirTemp("", "mseg-tester-update-")
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: creating build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	built := filepath.Join(buildDir, "mseg-tester")
	if err := goBuild(srcDir, built); err != nil {
		return Result{}, fmt.Errorf("selfupdate: building %s: %w", srcDir, err)
	}

	if err := replaceSelf(self, built); err != nil {
		return Result{}, fmt.Errorf("selfupdate: applying update: %w", err)
	}
	if err := runDeploy(self); err != nil {
		return Result{}, fmt.Errorf("selfupdate: redeploying after update: %w", err)
	}
	return Result{Applied: true}, nil
}

// Build is a human-identifiable description of the currently-running
// executable's build -- see BuildInfo.
type Build struct {
	// Version is the Go module version debug.ReadBuildInfo() resolved at
	// build time -- a pseudo-version of the form
	// "v0.0.0-<timestamp>-<commit>" for anything short of a real tagged
	// release, where <commit> is the first 12 hex characters of
	// Revision below. "(unknown)" if build info isn't available at all
	// (extremely unlikely for anything built with go1.18+ in module
	// mode).
	Version string
	// Revision is the full 40-character git commit SHA-1 mseg-tester
	// was built from -- reliably populated now that every build (the
	// very first, via cloud-init's bootstrap script, and every
	// self-update since) happens as a plain `go build` inside a real
	// local git checkout (see this package's doc comment); Go's
	// toolchain stamps VCS info automatically for any such build, no
	// -ldflags needed. This is what checkAndApply compares against a
	// checkout's HEAD to decide whether a rebuild is needed at all.
	// Empty if build info genuinely isn't available.
	Revision string
	// Modified is true if Revision was read from a git working tree that
	// had uncommitted changes at build time -- meaningless (always
	// false) when Revision is empty.
	Modified bool
	// Time is the commit time (RFC 3339), if available -- same
	// availability caveat as Revision.
	Time string
}

// BuildInfo reads the currently-running executable's own build info
// (runtime/debug.ReadBuildInfo) -- no custom -ldflags needed at build
// time, unlike a typical hand-rolled "-X main.version=..." scheme; the
// Go toolchain embeds this automatically for any module-mode build.
func BuildInfo() Build {
	b := Build{Version: "(unknown)"}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	if bi.Main.Version != "" {
		b.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.time":
			b.Time = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// CurrentVersion returns Version alone -- the identifier recorded into
// every state.Result (and therefore every push to Report.URL/Influx),
// so a result can always be traced back to the exact commit mseg-tester
// was running when it was produced.
func CurrentVersion() (string, error) {
	return BuildInfo().Version, nil
}

// replaceSelf writes built's content into a temp file NEXT TO self (same
// directory, so the final rename is same-filesystem and therefore
// atomic -- built itself may be on a different filesystem, e.g. under
// /tmp), makes it executable, and renames it over self. On Linux this is
// safe even while the old binary is executing -- the running process
// holds its original inode open; the rename just repoints the filename
// for the NEXT invocation.
func replaceSelf(self, built string) error {
	b, err := os.ReadFile(built)
	if err != nil {
		return fmt.Errorf("reading %s: %w", built, err)
	}
	tmp := self + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, self); err != nil {
		return fmt.Errorf("renaming %s -> %s: %w", tmp, self, err)
	}
	return nil
}
