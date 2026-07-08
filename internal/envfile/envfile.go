// Package envfile loads a simple "KEY=VALUE" .env file and expands
// "${VAR}" references found in config.yaml's raw text before it's
// parsed (see internal/config.Load) -- lets config.yaml reference a
// secret (e.g. report.influx.token) by name instead of embedding it
// directly, so config.yaml itself stays safe to keep in a shared or even
// public repo while the actual value lives in a small, local-only, 0600
// file cloud-init writes once and never syncs anywhere (the same
// "local, rarely-changing, never-repo-synced" tier as bootstrap.yaml
// itself -- see internal/bootstrap's package doc).
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Load parses path as a simple ".env" file: one KEY=VALUE per line,
// blank lines and lines starting with "#" ignored, optional matching
// single/double quotes around VALUE stripped. Returns an empty map (not
// an error) if path is empty or the file doesn't exist at all -- an env
// file is entirely optional, exactly like bootstrap.Bootstrap.ConfigRepo
// being empty means "nothing to fetch."
func Load(path string) (map[string]string, error) {
	vars := map[string]string{}
	if path == "" {
		return vars, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return vars, nil
		}
		return nil, fmt.Errorf("envfile: reading %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue // not a KEY=VALUE line -- ignored, not fatal
		}
		vars[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: reading %s: %w", path, err)
	}
	return vars, nil
}

// unquote strips one layer of matching single or double quotes, if
// present -- e.g. `TOKEN="abc def"` keeps the embedded space, matching
// the common .env convention.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// varRE matches "${VAR}" -- VAR restricted to the usual shell-identifier
// charset, so stray "${" in some other context (unlikely in YAML, but
// cheap to guard) doesn't get misread.
var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand replaces every "${VAR}" in s: first checking vars (the loaded
// .env file), then falling back to the real process environment
// (os.LookupEnv -- so a systemd Environment= override, or just running
// by hand with the variable already exported, works even with no .env
// file at all). A name matching NEITHER is left as "${VAR}", untouched
// -- silently turning a typo'd variable name into an empty string would
// be far more confusing than leaving visible evidence it never resolved.
func Expand(s string, vars map[string]string) string {
	return varRE.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1] // strip "${" prefix and "}" suffix
		if v, ok := vars[name]; ok {
			return v
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return match
	})
}
