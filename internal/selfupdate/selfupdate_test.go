package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeGitState is the shared state a test's faked git/go seams read and
// write, so a test can drive "no checkout yet" -> "cloned" -> "fetched
// to some HEAD" -> "built" without a real git binary, network, or Go
// toolchain.
type fakeGitState struct {
	cloned       bool
	clonedFrom   string
	head         string // what gitFetchReset/gitRevParseHEAD report after being called
	built        []byte // content goBuild "produces"
	deployed     bool   // set by the faked runDeploy
	deployedSelf string // self path runDeploy was called with
}

// withFakeGitAndBuild replaces gitClone/gitFetchReset/gitRevParseHEAD/
// goBuild/runDeploy for the duration of one test, backed by fs (a real
// temp dir standing in for srcDir -- gitClone/gitFetchReset just touch a
// marker file there so os.Stat(filepath.Join(srcDir, ".git")) behaves
// correctly for checkAndApply's own "does the checkout already exist"
// check). runDeploy is faked too since a real one would try to exec the
// fake "self" file as a subprocess.
func withFakeGitAndBuild(t *testing.T, st *fakeGitState) {
	t.Helper()
	origClone, origFetch, origRev, origBuild, origDeploy := gitClone, gitFetchReset, gitRevParseHEAD, goBuild, runDeploy

	gitClone = func(repoURL, dir string) error {
		st.cloned = true
		st.clonedFrom = repoURL
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".git"), []byte("fake"), 0o644)
	}
	gitFetchReset = func(dir, ref string) error {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			return fmt.Errorf("gitFetchReset called before clone: %w", err)
		}
		return nil
	}
	gitRevParseHEAD = func(dir string) (string, error) {
		return st.head, nil
	}
	goBuild = func(srcDir, out string) error {
		return os.WriteFile(out, st.built, 0o755)
	}
	runDeploy = func(self string) error {
		st.deployed = true
		st.deployedSelf = self
		return nil
	}

	t.Cleanup(func() {
		gitClone, gitFetchReset, gitRevParseHEAD, goBuild, runDeploy = origClone, origFetch, origRev, origBuild, origDeploy
	})
}

func writeSelf(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	self := filepath.Join(dir, "mseg-tester")
	if err := os.WriteFile(self, content, 0o755); err != nil {
		t.Fatalf("writing fake self: %v", err)
	}
	return self
}

func TestCheckAndApplyCurrentRevisionMatchesHeadIsNoop(t *testing.T) {
	self := writeSelf(t, []byte("running binary bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "abc123", built: []byte("would-be-new-bytes")}
	withFakeGitAndBuild(t, st)

	res, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, "abc123")
	if err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if res.Applied {
		t.Error("expected Applied=false when currentRevision already matches HEAD")
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("reading self: %v", err)
	}
	if string(got) != "running binary bytes" {
		t.Errorf("self was modified even though no update was needed, got %q", got)
	}
}

func TestCheckAndApplyDifferentHeadRebuildsAndReplacesSelf(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "def456", built: []byte("new bytes")}
	withFakeGitAndBuild(t, st)

	res, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, "abc123")
	if err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if !res.Applied {
		t.Error("expected Applied=true when HEAD differs from currentRevision")
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("reading self: %v", err)
	}
	if string(got) != "new bytes" {
		t.Errorf("self = %q, want %q", got, "new bytes")
	}
	info, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat self: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("expected self to remain executable after replacement")
	}
	if !st.deployed {
		t.Error("expected runDeploy to be called after a successful update, so the systemd unit gets refreshed from the new build too")
	}
	if st.deployedSelf != self {
		t.Errorf("runDeploy called with self=%q, want %q", st.deployedSelf, self)
	}
}

func TestCheckAndApplyNoopDoesNotRedeploy(t *testing.T) {
	// No update needed -- redeploying here would just be pointless extra
	// work (systemctl daemon-reload etc.) on every single cycle instead
	// of only when something actually changed.
	self := writeSelf(t, []byte("running binary bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "abc123", built: []byte("would-be-new-bytes")}
	withFakeGitAndBuild(t, st)

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, "abc123"); err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if st.deployed {
		t.Error("expected runDeploy NOT to be called when no update was applied")
	}
}

func TestCheckAndApplyRedeployFailureIsAnError(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "def456", built: []byte("new bytes")}
	withFakeGitAndBuild(t, st)
	runDeploy = func(self string) error { return fmt.Errorf("systemctl not found") }

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, "abc123"); err == nil {
		t.Error("expected an error when runDeploy fails")
	}
}

func TestCheckAndApplyEmptyCurrentRevisionAlwaysRebuilds(t *testing.T) {
	// An empty currentRevision (build info unavailable) must never be
	// treated as "matches" -- always rebuild rather than risk silently
	// skipping a genuinely-needed update.
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "same-as-nothing", built: []byte("new bytes")}
	withFakeGitAndBuild(t, st)

	res, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, "")
	if err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if !res.Applied {
		t.Error("expected Applied=true when currentRevision is empty")
	}
}

func TestCheckAndApplyClonesWhenSrcDirMissing(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src") // deliberately not created
	st := &fakeGitState{head: "abc123", built: []byte("new bytes")}
	withFakeGitAndBuild(t, st)

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, ""); err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if !st.cloned {
		t.Error("expected gitClone to be called when srcDir doesn't exist yet")
	}
	if st.clonedFrom != "https://github.com/mabels/mseg-tester.git" {
		t.Errorf("cloned from %q, want the given repoURL", st.clonedFrom)
	}
}

func TestCheckAndApplyDoesNotReCloneWhenSrcDirExists(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, ".git"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("seeding .git marker: %v", err)
	}
	st := &fakeGitState{head: "abc123", built: []byte("new bytes")}
	withFakeGitAndBuild(t, st)

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, ""); err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if st.cloned {
		t.Error("expected gitClone NOT to be called when srcDir already has a .git checkout")
	}
}

func TestCheckAndApplyDefaultsRefToMain(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "abc123", built: []byte("old bytes")}

	var gotRef string
	origClone, origFetch, origRev, origBuild := gitClone, gitFetchReset, gitRevParseHEAD, goBuild
	gitClone = func(repoURL, dir string) error {
		return os.MkdirAll(dir, 0o755)
	}
	gitFetchReset = func(dir, ref string) error {
		gotRef = ref
		return nil
	}
	gitRevParseHEAD = func(dir string) (string, error) { return st.head, nil }
	goBuild = func(srcDir, out string) error { return os.WriteFile(out, st.built, 0o755) }
	t.Cleanup(func() { gitClone, gitFetchReset, gitRevParseHEAD, goBuild = origClone, origFetch, origRev, origBuild })

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "", self, "abc123"); err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if gotRef != "main" {
		t.Errorf("ref = %q, want %q (empty should default)", gotRef, "main")
	}
}

func TestCheckAndApplyFetchFailureIsAnError(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, ".git"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("seeding .git marker: %v", err)
	}
	orig := gitFetchReset
	gitFetchReset = func(dir, ref string) error { return fmt.Errorf("network is down") }
	t.Cleanup(func() { gitFetchReset = orig })

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, ""); err == nil {
		t.Error("expected an error when gitFetchReset fails")
	}
}

func TestCheckAndApplyBuildFailureIsAnError(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	srcDir := filepath.Join(t.TempDir(), "src")
	st := &fakeGitState{head: "abc123"}
	withFakeGitAndBuild(t, st)
	orig := goBuild
	goBuild = func(srcDir, out string) error { return os.ErrPermission }
	t.Cleanup(func() { goBuild = orig })

	if _, err := checkAndApply(srcDir, "https://github.com/mabels/mseg-tester.git", "main", self, ""); err == nil {
		t.Error("expected an error when goBuild fails")
	}
}

func TestBuildInfoNeverPanicsAndReturnsAVersion(t *testing.T) {
	// Running inside `go test`, this reads the TEST binary's own build
	// info, not a real mseg-tester build -- can't assert an exact commit
	// here, just that reading it degrades gracefully (never panics,
	// Version is never empty -- "(unknown)" is the deliberate fallback).
	b := BuildInfo()
	if b.Version == "" {
		t.Errorf("BuildInfo().Version should never be empty, got %q", b.Version)
	}
}

func TestCurrentVersionMatchesBuildInfoVersion(t *testing.T) {
	v, err := CurrentVersion()
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if v != BuildInfo().Version {
		t.Errorf("CurrentVersion() = %q, want it to match BuildInfo().Version = %q", v, BuildInfo().Version)
	}
}
