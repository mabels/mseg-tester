// Package report POSTs every accumulated <segment>.result.yaml to
// config.Report.URL -- e.g. the user's own k8s-hosted collector on the
// management segment (192.168.129.88), or a Prometheus Pushgateway.
// Only ever attempted from bootstrap.Bootstrap's UpdateSegment, the one
// segment with a route anywhere outside the segment under test -- same
// reachability constraint as internal/selfupdate and internal/configsync.
//
// A failed push is never fatal to the run: the whole point of this tool
// is the reboot-into-next-segment cycle completing regardless, and
// whatever didn't get reported this pass is still sitting on disk in
// <stateDir>/*.result.yaml for the next attempt to pick up.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mabels/mseg-tester/internal/state"
)

const pushTimeout = 15 * time.Second

// Push reads every result currently in stateDir and POSTs them as a JSON
// array to url.
func Push(url, stateDir string) error {
	results, err := state.LoadAllResults(stateDir)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	body, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("report: marshaling results: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("report: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: pushTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("report: POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("report: POST %s: unexpected status %s", url, resp.Status)
	}
	return nil
}
