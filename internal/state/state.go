// Package state reads/writes the two files this tool lives on:
//
//	<stateDir>/active.yaml           -- which segment is under test right now,
//	                                    and the full cycle order to advance through.
//	<stateDir>/<segment>.result.yaml -- the last result recorded for that segment.
//
// Both are written atomically (temp file + rename) so a power loss mid-write
// never leaves a half-written, unparseable file behind -- the same
// "converge, don't corrupt" concern as everywhere else in this project.
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Active is the whole content of active.yaml. Segment is the one netplan is
// CURRENTLY configured for (and therefore the one this run's checks apply
// to) -- Cycle is the fixed order to advance through, wrapping at the end.
type Active struct {
	Segment string   `yaml:"segment"`
	Cycle   []string `yaml:"cycle"`
}

// Next returns the segment that should follow Segment in Cycle, wrapping
// around. Panics if Segment isn't actually in Cycle -- that's a config
// error (active.yaml and config.yaml disagreeing), not a runtime condition
// to silently paper over.
func (a Active) Next() string {
	for i, s := range a.Cycle {
		if s == a.Segment {
			return a.Cycle[(i+1)%len(a.Cycle)]
		}
	}
	panic(fmt.Sprintf("state: active segment %q not found in cycle %v", a.Segment, a.Cycle))
}

// CheckResult is one named check (dhcp/dns/geo/routing) within a Result.
type CheckResult struct {
	Name   string `yaml:"name"`
	Pass   bool   `yaml:"pass"`
	Detail string `yaml:"detail,omitempty"`
}

// Result is what gets written to <segment>.result.yaml after every test
// run against that segment -- overwritten each time this segment comes
// back around in the cycle, so it always reflects the MOST RECENT pass,
// not a growing history (the cycle itself provides the time dimension;
// anything wanting history should read these on a schedule, not rely on
// this file accumulating it).
type Result struct {
	Segment   string        `yaml:"segment"`
	Timestamp time.Time     `yaml:"timestamp"`
	Checks    []CheckResult `yaml:"checks"`
	Version   string        `yaml:"version"`
	Updated   bool          `yaml:"updated"` // a self-update was applied during this run
}

// Pass reports whether every check in this result passed.
func (r Result) Pass() bool {
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

func activePath(stateDir string) string { return filepath.Join(stateDir, "active.yaml") }

func resultPath(stateDir, segment string) string {
	return filepath.Join(stateDir, segment+".result.yaml")
}

// LoadActive reads active.yaml. Callers decide what a missing file means
// (first boot vs. a real error) -- this just wraps the os.Stat/read error
// through unchanged rather than guessing.
func LoadActive(stateDir string) (Active, error) {
	var a Active
	b, err := os.ReadFile(activePath(stateDir))
	if err != nil {
		return a, err
	}
	if err := yaml.Unmarshal(b, &a); err != nil {
		return a, fmt.Errorf("state: parsing active.yaml: %w", err)
	}
	return a, nil
}

// SaveActive writes active.yaml atomically.
func SaveActive(stateDir string, a Active) error {
	b, err := yaml.Marshal(a)
	if err != nil {
		return fmt.Errorf("state: marshaling active.yaml: %w", err)
	}
	return writeAtomic(activePath(stateDir), b)
}

// SaveResult writes <segment>.result.yaml atomically.
func SaveResult(stateDir string, r Result) error {
	b, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("state: marshaling result for %s: %w", r.Segment, err)
	}
	return writeAtomic(resultPath(stateDir, r.Segment), b)
}

// LoadAllResults reads every <segment>.result.yaml present in stateDir,
// sorted by segment name for a stable report ordering. A segment that
// hasn't come back around in the cycle yet (or a fresh state dir) simply
// has no file and is omitted -- not an error, since the report is always
// "whatever's accumulated so far," not a guarantee every segment is
// represented on every push.
func LoadAllResults(stateDir string) ([]Result, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("state: reading %s: %w", stateDir, err)
	}
	var results []Result
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".result.yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("state: reading %s: %w", e.Name(), err)
		}
		var r Result
		if err := yaml.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("state: parsing %s: %w", e.Name(), err)
		}
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Segment < results[j].Segment })
	return results, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("state: creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("state: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: renaming %s -> %s: %w", tmp, path, err)
	}
	return nil
}
