package config

import (
	"os"
	"path/filepath"
	"testing"
)

const minimalYAML = `
segments:
  - name: "128"
    type: native
    dnsServer: "192.168.128.5"
    routingCheck: "1.1.1.1:443"
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsVarFromEnvVarsMap(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${INFLUX_TOKEN}"
`)
	cfg, err := Load(path, map[string]string{"INFLUX_TOKEN": "secret-value"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report == nil || cfg.Report.Influx == nil {
		t.Fatalf("expected report.influx to be parsed, got %+v", cfg.Report)
	}
	if cfg.Report.Influx.Token != "secret-value" {
		t.Errorf("Token = %q, want the expanded value", cfg.Report.Influx.Token)
	}
}

func TestLoadExpandsVarFromRealEnvWhenMapNil(t *testing.T) {
	t.Setenv("MSEG_TESTER_CONFIG_TEST_TOKEN", "from-real-env")
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${MSEG_TESTER_CONFIG_TEST_TOKEN}"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report.Influx.Token != "from-real-env" {
		t.Errorf("Token = %q, want the real-env fallback value", cfg.Report.Influx.Token)
	}
}

func TestLoadLeavesUnresolvedVarAsLiteralPlaceholder(t *testing.T) {
	path := writeTemp(t, minimalYAML+`
report:
  influx:
    url: "https://mam-influx.adviser.com"
    org: "homeassistant"
    bucket: "mseg-tester"
    token: "${THIS_VAR_IS_NOT_SET_ANYWHERE}"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Report.Influx.Token != "${THIS_VAR_IS_NOT_SET_ANYWHERE}" {
		t.Errorf("Token = %q, want the placeholder left untouched", cfg.Report.Influx.Token)
	}
}

func TestLoadWithNoVarReferencesUnaffected(t *testing.T) {
	path := writeTemp(t, minimalYAML)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Segments) != 1 || cfg.Segments[0].Name != "128" {
		t.Errorf("unexpected segments: %+v", cfg.Segments)
	}
}
