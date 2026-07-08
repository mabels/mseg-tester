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

// CurrentVersion returns a short, content-derived identifier for the
// currently-running executable (the first 12 hex characters of its
// SHA-256) -- meaningful even without a semver tag baked in at build
// time: two builds share a CurrentVersion iff they're byte-identical.
func CurrentVersion() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("selfupdate: locating current executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("selfupdate: resolving %s: %w", self, err)
	}
	h, err := fileSHA256(self)
	if err != nil {
		return "", fmt.Errorf("selfupdate: hashing %s: %w", self, err)
	}
	return h[:12], nil
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
