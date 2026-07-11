// Package selfupdate replaces the running binary with a fresh build of
// itself straight from source, via `go install <modulePath>@<ref>` -- no
// GitHub Releases API, no release assets, no asset-naming convention to
// keep in sync with a separate build pipeline. Only ever called when the
// active segment is bootstrap.Bootstrap.UpdateSegment -- see
// cmd/mseg-tester.
//
// This replaced an earlier GitHub-Releases-based design for two reasons:
//   - `go install` needs no published release at all -- any pushed
//     commit, branch, or tag is installable, which is what makes testing
//     an unreleased build (verify-mseg-tester's -module-ref) possible
//     with no separate binary-hosting step.
//   - it talks to the Go module proxy (or git over https), not GitHub's
//     unauthenticated REST API -- which is rate-limited to 60
//     requests/hour per source IP, a limit that every VM polling
//     api.github.com/repos/.../releases/latest on its own cycle would
//     eventually collide with.
package selfupdate

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
)

// Result reports what happened, for the caller to fold into
// state.Result.Updated -- never an error just because no update was
// needed; that's the common case, not a failure.
type Result struct {
	Applied bool
}

// goInstall is a variable, not a plain function, so tests can fake the
// build step without a real Go toolchain or network round trip.
var goInstall = func(modulePath, ref, gobin string) error {
	cmd := exec.Command("go", "install", modulePath+"@"+ref)
	cmd.Env = append(os.Environ(), "GOBIN="+gobin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// CheckAndApply builds modulePath@ref into a scratch directory and, if
// the result differs from the currently-running executable, replaces
// it. modulePath is a full Go module path to a main package, e.g.
// "github.com/mabels/mseg-tester/cmd/mseg-tester"; ref is a git
// branch/tag/commit, or "latest" (see bootstrap.Bootstrap.SoftwareRef).
func CheckAndApply(modulePath, ref string) (Result, error) {
	self, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: locating current executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: resolving %s: %w", self, err)
	}
	return checkAndApply(modulePath, ref, self)
}

// checkAndApply is CheckAndApply with self (the executable to compare
// against and, if stale, overwrite) passed in explicitly -- kept
// separate so tests can point it at a throwaway file instead of the
// real, running test binary.
func checkAndApply(modulePath, ref, self string) (Result, error) {
	if ref == "" {
		ref = "latest"
	}

	buildDir, err := os.MkdirTemp("", "mseg-tester-update-")
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: creating build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	if err := goInstall(modulePath, ref, buildDir); err != nil {
		return Result{}, fmt.Errorf("selfupdate: building %s@%s: %w", modulePath, ref, err)
	}

	built := filepath.Join(buildDir, filepath.Base(modulePath))
	same, err := sameContent(self, built)
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: comparing binaries: %w", err)
	}
	if same {
		return Result{Applied: false}, nil
	}
	if err := replaceSelf(self, built); err != nil {
		return Result{}, fmt.Errorf("selfupdate: applying update: %w", err)
	}
	return Result{Applied: true}, nil
}

// Build is a human-identifiable description of the currently-running
// executable's build -- see BuildInfo.
type Build struct {
	// Version is the Go module version debug.ReadBuildInfo() resolved at
	// build time. For a real tagged release this is that tag; for
	// anything else (the common case here -- see
	// bootstrap.Bootstrap.SoftwareRef's doc comment, "latest" resolves
	// to the newest semver tag but a branch/commit ref doesn't) it's a
	// pseudo-version of the form "v0.0.0-<timestamp>-<commit>", where
	// <commit> is the first 12 hex characters of the git commit SHA-1
	// mseg-tester was built from -- confirmed live: `go install
	// module@<branch-or-commit>` (exactly what CheckAndApply runs)
	// embeds this even when fetched purely through the module proxy,
	// with no local git checkout involved at all, so this works
	// identically whether Build is read on a dev machine or a VM that
	// self-updated from source. "(unknown)" if build info isn't
	// available at all (extremely unlikely for anything built with
	// go1.18+ in module mode).
	Version string
	// Revision is the full 40-character git commit SHA-1, if available
	// -- only present when the build itself happened inside a git
	// working tree (a local `go build`/`go install ./...` in this repo),
	// NOT when installed via `go install module@ref` from the module
	// proxy (that path has no local .git to read -- Version's pseudo-
	// version suffix is the only commit info available there). Empty
	// when not available; check Version instead.
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
// was running when it was produced. Replaced an earlier content-hash
// (SHA-256 of the executable) scheme -- that only ever proved "two
// builds are/aren't byte-identical", not which commit either one
// actually was; this is what a person actually wants when comparing
// results across VMs or across a self-update. Never errors in practice
// (BuildInfo degrades to "(unknown)" instead) -- the error return stays
// for interface stability with callers already handling one.
func CurrentVersion() (string, error) {
	return BuildInfo().Version, nil
}

func sameContent(a, b string) (bool, error) {
	ah, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	bh, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return ah == bh, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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
