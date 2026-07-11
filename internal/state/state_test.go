package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSaveLoadActiveRoundTripsStopOn(t *testing.T) {
	dir := t.TempDir()
	want := Active{Segment: "129", Cycle: []string{"128", "129", "130", "131"}, StopOn: "128"}
	if err := SaveActive(dir, want); err != nil {
		t.Fatalf("SaveActive: %v", err)
	}
	got, err := LoadActive(dir)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadActive = %+v, want %+v", got, want)
	}
}

func TestSaveActiveOmitsStopOnWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := SaveActive(dir, Active{Segment: "129", Cycle: []string{"129"}}); err != nil {
		t.Fatalf("SaveActive: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "active.yaml"))
	if err != nil {
		t.Fatalf("reading active.yaml: %v", err)
	}
	if strings.Contains(string(b), "stopOn") {
		t.Errorf("expected no stopOn key when StopOn is empty, got:\n%s", b)
	}
}

func TestNextIgnoresStopOn(t *testing.T) {
	// StopOn only affects whether cmd/mseg-tester's run() advances at
	// all -- Active.Next() itself (what it would advance TO) is
	// unaffected by StopOn.
	a := Active{Segment: "128", Cycle: []string{"128", "129", "130", "131"}, StopOn: "128"}
	if got := a.Next(); got != "129" {
		t.Errorf("Next() = %q, want %q", got, "129")
	}
}
