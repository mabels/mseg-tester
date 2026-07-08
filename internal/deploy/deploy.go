// Package deploy is what `mseg-tester deploy` runs: install THIS
// executable to a stable path and register it as a systemd unit. This is
// the piece cloud-init used to do by hand-writing the unit file directly
// -- moving it here means cloud-init's only job is "get a working
// mseg-tester binary onto the box, then run it," and everything about how
// this tool wires itself into systemd lives in one place, versioned and
// released alongside the binary itself rather than duplicated in a
// separate cloud-init template that can drift out of sync with it.
//
// Idempotent, same philosophy as everywhere else in this project: safe to
// run again (e.g. after a self-update replaces the binary) without
// leaving stale units or duplicate installs behind.
package deploy

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed mseg-tester.service
var unitContent []byte

const (
	installPath = "/usr/local/bin/mseg-tester"
	unitPath    = "/etc/systemd/system/mseg-tester.service"
	unitName    = "mseg-tester.service"
)

// Run installs the currently-running executable to installPath (if not
// already running from there) and (re)installs+enables the systemd unit.
func Run() error {
	if err := installSelf(); err != nil {
		return fmt.Errorf("deploy: installing executable: %w", err)
	}
	if err := writeAtomic(unitPath, unitContent, 0o644); err != nil {
		return fmt.Errorf("deploy: writing %s: %w", unitPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if err := runSystemctl("enable", unitName); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	return nil
}

// installSelf copies the currently-running executable to installPath,
// unless it's already running from exactly there (the common case once
// deployed: the systemd unit re-execs this same path every cycle, and a
// self-update replaces installPath in place -- deploy re-running after
// that has nothing left to do here).
func installSelf() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", self, err)
	}
	resolvedInstallPath := installPath
	if existing, err := filepath.EvalSymlinks(installPath); err == nil {
		resolvedInstallPath = existing
	}
	if self == resolvedInstallPath {
		return nil
	}

	b, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("reading %s: %w", self, err)
	}
	return writeAtomic(installPath, b, 0o755)
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, string(out))
	}
	return nil
}
