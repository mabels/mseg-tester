package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeGoInstall replaces goInstall for the duration of one test with
// a fake that writes content to <gobin>/<base of modulePath> instead of
// actually invoking the Go toolchain -- these tests exercise the
// hash-compare/replace logic, not `go install` itself.
func withFakeGoInstall(t *testing.T, content []byte) {
	t.Helper()
	orig := goInstall
	goInstall = func(modulePath, ref, gobin string) error {
		return os.WriteFile(filepath.Join(gobin, filepath.Base(modulePath)), content, 0o755)
	}
	t.Cleanup(func() { goInstall = orig })
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

func TestCheckAndApplySameContentIsNoop(t *testing.T) {
	content := []byte("same bytes")
	self := writeSelf(t, content)
	withFakeGoInstall(t, content)

	res, err := checkAndApply("github.com/mabels/mseg-tester/cmd/mseg-tester", "latest", self)
	if err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if res.Applied {
		t.Error("expected Applied=false when the built binary matches self")
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("reading self: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("self was modified, got %q want %q", got, content)
	}
}

func TestCheckAndApplyDifferentContentReplacesSelf(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	withFakeGoInstall(t, []byte("new bytes"))

	res, err := checkAndApply("github.com/mabels/mseg-tester/cmd/mseg-tester", "latest", self)
	if err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if !res.Applied {
		t.Error("expected Applied=true when the built binary differs from self")
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
}

func TestCheckAndApplyDefaultsRefToLatest(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	var gotRef string
	orig := goInstall
	goInstall = func(modulePath, ref, gobin string) error {
		gotRef = ref
		return os.WriteFile(filepath.Join(gobin, filepath.Base(modulePath)), []byte("old bytes"), 0o755)
	}
	t.Cleanup(func() { goInstall = orig })

	if _, err := checkAndApply("github.com/mabels/mseg-tester/cmd/mseg-tester", "", self); err != nil {
		t.Fatalf("checkAndApply: %v", err)
	}
	if gotRef != "latest" {
		t.Errorf("ref = %q, want %q (empty should default)", gotRef, "latest")
	}
}

func TestCheckAndApplyBuildFailureIsAnError(t *testing.T) {
	self := writeSelf(t, []byte("old bytes"))
	orig := goInstall
	goInstall = func(modulePath, ref, gobin string) error {
		return os.ErrPermission
	}
	t.Cleanup(func() { goInstall = orig })

	if _, err := checkAndApply("github.com/mabels/mseg-tester/cmd/mseg-tester", "latest", self); err == nil {
		t.Error("expected an error when goInstall fails")
	}
}

func TestCurrentVersionIsShortAndStable(t *testing.T) {
	// CheckAndApply's own logic is exercised above via checkAndApply;
	// CurrentVersion itself just calls os.Executable(), so this only
	// confirms the trivial hashing contract on a controlled file.
	self := writeSelf(t, []byte("hello"))
	h, err := fileSHA256(self)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}
	if len(h) < 12 {
		t.Fatalf("hash too short to slice to 12: %q", h)
	}
}
