// Package configsync fetches the real, content-level config.yaml from the
// PRIVATE repo named in bootstrap.yaml, and writes it to
// bootstrap.ConfigLocalPath. Uses GitHub's Contents API with a
// fine-grained, read-only, single-repo-scoped token -- deliberately not a
// full `git clone` (no SSH key/agent to manage, no git binary dependency,
// stays "small" per this whole tool's brief) and deliberately not the
// public raw.githubusercontent.com endpoint either (that only serves
// PUBLIC repos without auth -- the Contents API is what actually accepts
// token-authenticated reads for a private one).
//
// Only ever expected to succeed when run from bootstrap.Bootstrap's
// UpdateSegment -- every other segment has no route to api.github.com at
// all, so a failure there is just "as expected," not alarming. Callers
// decide how to treat a failure (see cmd/mseg-tester's run()): the
// existing on-disk config.yaml from the last successful sync is always
// left in place if a sync fails, so one missed cycle never leaves the box
// without ANY config to test against.
package configsync

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mabels/mseg-tester/internal/bootstrap"
)

const apiTimeout = 15 * time.Second

type contentsResponse struct {
	Content  string `json:"content"`  // base64, possibly with embedded newlines
	Encoding string `json:"encoding"` // expected "base64"
}

// ownerRepoFromURL turns bootstrap.Bootstrap.ConfigRepo -- a full repo
// URL, e.g. "https://github.com/owner/repo" (also accepts a trailing
// "/" or ".git", and the "git@github.com:owner/repo.git" SSH form) --
// into the bare "owner/repo" GitHub's REST API expects in its path.
// A plain "owner/repo" with no scheme is also accepted, for callers that
// already have it in that shape.
func ownerRepoFromURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// scp-like SSH syntax: git@github.com:owner/repo
	if !strings.Contains(s, "://") {
		if i := strings.Index(s, "@"); i != -1 {
			if j := strings.Index(s[i:], ":"); j != -1 {
				ownerRepo := strings.Trim(s[i+j+1:], "/")
				if ownerRepo != "" {
					return ownerRepo, nil
				}
			}
		}
	}

	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("parsing %q as a URL: %w", raw, err)
		}
		ownerRepo := strings.Trim(u.Path, "/")
		if ownerRepo == "" {
			return "", fmt.Errorf("%q has no owner/repo path", raw)
		}
		return ownerRepo, nil
	}

	// Already bare "owner/repo".
	if strings.Count(s, "/") == 1 {
		return s, nil
	}

	return "", fmt.Errorf("cannot make sense of %q -- expected a GitHub URL like https://github.com/owner/repo", raw)
}

// Fetch downloads b.ConfigPath at b.ConfigRef from b.ConfigRepo and
// writes it atomically to b.ConfigLocalPath.
func Fetch(b bootstrap.Bootstrap) error {
	ownerRepo, err := ownerRepoFromURL(b.ConfigRepo)
	if err != nil {
		return fmt.Errorf("configsync: configRepo: %w", err)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", ownerRepo, b.ConfigPath, b.ConfigRef)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("configsync: building request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if b.ConfigToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.ConfigToken)
	}

	client := &http.Client{Timeout: apiTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("configsync: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("configsync: GET %s: unexpected status %s", url, resp.Status)
	}

	var body contentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("configsync: decoding response json: %w", err)
	}
	if body.Encoding != "base64" {
		return fmt.Errorf("configsync: unexpected content encoding %q (expected base64)", body.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(stripNewlines(body.Content))
	if err != nil {
		return fmt.Errorf("configsync: base64-decoding content: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(b.ConfigLocalPath), 0o755); err != nil {
		return fmt.Errorf("configsync: creating %s: %w", filepath.Dir(b.ConfigLocalPath), err)
	}
	tmp := b.ConfigLocalPath + ".tmp"
	if err := os.WriteFile(tmp, decoded, 0o644); err != nil {
		return fmt.Errorf("configsync: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, b.ConfigLocalPath); err != nil {
		return fmt.Errorf("configsync: renaming %s -> %s: %w", tmp, b.ConfigLocalPath, err)
	}
	return nil
}

// stripNewlines removes the embedded newlines GitHub's API wraps base64
// content with -- encoding/base64 doesn't accept those inline.
func stripNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' && s[i] != '\r' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
