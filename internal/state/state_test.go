package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestSaveLoadLastWaitRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := LastWait{Segment: "129", Ran: time.Now().UTC().Truncate(time.Second)}
	if err := SaveLastWait(dir, want); err != nil {
		t.Fatalf("SaveLastWait: %v", err)
	}
	got, err := LoadLastWait(dir, "129")
	if err != nil {
		t.Fatalf("LoadLastWait: %v", err)
	}
	if !got.Ran.Equal(want.Ran) || got.Segment != want.Segment {
		t.Errorf("LoadLastWait = %+v, want %+v", got, want)
	}
}

func TestLoadLastWaitMissingFileIsNotExist(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadLastWait(dir, "129"); !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist for a segment with no lastwait file yet, got %v", err)
	}
}

func TestSaveLastWaitIsPerSegment(t *testing.T) {
	dir := t.TempDir()
	if err := SaveLastWait(dir, LastWait{Segment: "129", Ran: time.Now()}); err != nil {
		t.Fatalf("SaveLastWait(129): %v", err)
	}
	if _, err := LoadLastWait(dir, "130"); !os.IsNotExist(err) {
		t.Errorf("expected segment 130's lastwait to be untouched by saving 129's, got %v", err)
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
