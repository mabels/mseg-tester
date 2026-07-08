package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsEmptyNotError(t *testing.T) {
	vars, err := Load(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("expected an empty map, got %+v", vars)
	}
}

func TestLoadEmptyPathReturnsEmptyNotError(t *testing.T) {
	vars, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("expected an empty map, got %+v", vars)
	}
}

func TestLoadParsesKeyValueSkipsBlankAndComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# a comment\n\nINFLUX_TOKEN=abc123\nQUOTED=\"has a space\"\nSINGLE='also quoted'\nnot-a-line-at-all\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	vars, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{
		"INFLUX_TOKEN": "abc123",
		"QUOTED":       "has a space",
		"SINGLE":       "also quoted",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("vars[%q] = %q, want %q", k, vars[k], v)
		}
	}
	if len(vars) != len(want) {
		t.Errorf("expected exactly %d vars (malformed line ignored), got %+v", len(want), vars)
	}
}

func TestExpandUsesVarsMapFirst(t *testing.T) {
	got := Expand(`token: "${INFLUX_TOKEN}"`, map[string]string{"INFLUX_TOKEN": "secret"})
	if got != `token: "secret"` {
		t.Errorf("Expand() = %q", got)
	}
}

func TestExpandFallsBackToRealEnv(t *testing.T) {
	t.Setenv("MSEG_TESTER_TEST_VAR", "from-real-env")
	got := Expand("x: ${MSEG_TESTER_TEST_VAR}", nil)
	if got != "x: from-real-env" {
		t.Errorf("Expand() = %q", got)
	}
}

func TestExpandLeavesUnresolvedReferencesUntouched(t *testing.T) {
	got := Expand("x: ${THIS_VAR_DOES_NOT_EXIST_ANYWHERE}", nil)
	if got != "x: ${THIS_VAR_DOES_NOT_EXIST_ANYWHERE}" {
		t.Errorf("Expand() = %q, expected the placeholder left as-is", got)
	}
}

func TestExpandVarsMapTakesPrecedenceOverRealEnv(t *testing.T) {
	t.Setenv("MSEG_TESTER_TEST_VAR2", "from-real-env")
	got := Expand("x: ${MSEG_TESTER_TEST_VAR2}", map[string]string{"MSEG_TESTER_TEST_VAR2": "from-dot-env"})
	if got != "x: from-dot-env" {
		t.Errorf("Expand() = %q, expected the .env value to win", got)
	}
}
