// InfluxDB v2 line-protocol reporting -- an alternative to Push's generic
// JSON webhook, for the common case of already having an InfluxDB
// instance collecting everything else on the network. See
// config.InfluxReport's doc comment for the config shape.
package report

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mabels/mseg-tester/internal/config"
	"github.com/mabels/mseg-tester/internal/state"
)

// PushInflux reads every result currently in stateDir and writes them to
// cfg's InfluxDB v2 bucket as line protocol -- two measurements per
// segment: "mseg_tester_result" (one line, the overall pass/updated/
// version summary) and "mseg_tester_check" (one line per individual
// check, tagged with its name). Same best-effort contract as Push: the
// caller logs a failure and moves on, nothing here is fatal to the run.
func PushInflux(cfg config.InfluxReport, stateDir string) error {
	results, err := state.LoadAllResults(stateDir)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	body := lineProtocol(results)

	u := fmt.Sprintf("%s/api/v2/write?%s",
		strings.TrimRight(cfg.URL, "/"),
		url.Values{"org": {cfg.Org}, "bucket": {cfg.Bucket}, "precision": {"ns"}}.Encode())

	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("report: building influx request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+cfg.Token)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	client := &http.Client{Timeout: pushTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("report: influx write to %s: %w", cfg.URL, err)
	}
	defer resp.Body.Close()
	// InfluxDB v2's write API returns 204 on success -- anything outside
	// 200-299 (204 included) is treated as a failure, with the response
	// body (InfluxDB's error responses are small JSON) included for
	// diagnosis.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report: influx write to %s: unexpected status %s: %s", cfg.URL, resp.Status, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// lineProtocol renders results as InfluxDB line protocol.
func lineProtocol(results []state.Result) string {
	var b strings.Builder
	for _, r := range results {
		ts := r.Timestamp.UnixNano()
		fmt.Fprintf(&b, "mseg_tester_result,segment=%s pass=%t,updated=%t,version=\"%s\" %d\n",
			escapeTag(r.Segment), r.Pass(), r.Updated, escapeFieldString(r.Version), ts)
		for _, c := range r.Checks {
			fmt.Fprintf(&b, "mseg_tester_check,segment=%s,check=%s pass=%t,detail=\"%s\" %d\n",
				escapeTag(r.Segment), escapeTag(c.Name), c.Pass, escapeFieldString(c.Detail), ts)
		}
	}
	return b.String()
}

// escapeTag escapes the characters line protocol treats specially in a
// tag key/value: comma, equals sign, and space.
func escapeTag(s string) string {
	r := strings.NewReplacer(",", `\,`, "=", `\=`, " ", `\ `)
	return r.Replace(s)
}

// escapeFieldString escapes the characters line protocol treats
// specially inside a double-quoted string field value: backslash and
// double quote. Note this deliberately does NOT use fmt's %q (Go string
// escaping) -- %q also escapes things like tabs/newlines as "\t"/"\n",
// which line protocol does not recognize as escape sequences at all, so
// using it here would silently corrupt any Detail containing them.
func escapeFieldString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}
