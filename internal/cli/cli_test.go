// Tests for the command line: flag validation and exit codes, plus full
// in-process end-to-end runs against httptest servers, asserting on the
// exact NDJSON bytes a user would pipe into the next tool.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/JaydenCJ/depage/internal/version"
)

// run executes the CLI in-process and returns exit code, stdout, stderr.
func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// cursorServer paginates n records, 10 per page, cursor style.
func cursorServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 1
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		end := start + 9
		if end > n {
			end = n
		}
		items := []map[string]any{}
		for i := start; i <= end; i++ {
			items = append(items, map[string]any{"id": i, "name": fmt.Sprintf("rec-%02d", i)})
		}
		body := map[string]any{"data": items, "next_cursor": nil}
		if end < n {
			body["next_cursor"] = strconv.Itoa(end + 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVersionFlag(t *testing.T) {
	code, out, _ := run("--version")
	if code != 0 || out != "depage "+version.Version+"\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestHelpFlagPrintsUsage(t *testing.T) {
	code, out, _ := run("--help")
	if code != 0 || !strings.Contains(out, "Usage:") || !strings.Contains(out, "--max-pages") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestMissingURLIsUsageError(t *testing.T) {
	code, _, errOut := run("-q")
	if code != 2 || !strings.Contains(errOut, "missing URL") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	code, _, errOut := run("--frobnicate", "http://127.0.0.1:1/x")
	if code != 2 || !strings.Contains(errOut, "unknown flag") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestExtraPositionalArgumentRejected(t *testing.T) {
	code, _, errOut := run("http://127.0.0.1:1/a", "http://127.0.0.1:1/b")
	if code != 2 || !strings.Contains(errOut, "exactly one URL") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestFlagValueValidationErrors(t *testing.T) {
	// Every malformed flag value must exit 2 with a message that names the
	// problem — no request may ever be sent (the URL points at a dead port).
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-H", "NoColonHere"}, "malformed header"},
		{[]string{"--style", "token"}, "unknown style"},
		{[]string{"--items", "no-leading-slash"}, "must start with '/'"},
		{[]string{"--max-pages", "-1"}, "non-negative integer"},
		{[]string{"--retries", "two"}, "non-negative integer"},
		{[]string{"--delay", "fast"}, "non-negative duration"},
		{[]string{"--cursor-param", ""}, "needs a parameter name"},
	}
	for _, c := range cases {
		code, _, errOut := run(append(c.args, "http://127.0.0.1:1/x")...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Fatalf("args %v: code=%d stderr=%q, want exit 2 mentioning %q", c.args, code, errOut, c.want)
		}
	}
}

func TestEndToEndEmitsOneJSONObjectPerLine(t *testing.T) {
	srv := cursorServer(t, 23)
	code, out, errOut := run("-q", srv.URL)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 23 {
		t.Fatalf("got %d lines, want 23", len(lines))
	}
	// Every line must be standalone JSON, in dataset order.
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %q", i+1, line)
		}
		if int(obj["id"].(float64)) != i+1 {
			t.Fatalf("line %d has id %v, want %d", i+1, obj["id"], i+1)
		}
	}
	if lines[0] != `{"id":1,"name":"rec-01"}` {
		t.Fatalf("first line = %q (output must stay compact and stable)", lines[0])
	}
}

func TestSummaryLineReportsStyleAndCounts(t *testing.T) {
	srv := cursorServer(t, 23)
	code, _, errOut := run(srv.URL)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	want := "depage: style=cursor (via body field /next_cursor), pages=3, items=23\n"
	if errOut != want {
		t.Fatalf("stderr = %q, want %q", errOut, want)
	}
	// And -q must silence it entirely.
	if _, _, errOut := run("-q", srv.URL); errOut != "" {
		t.Fatalf("with -q, stderr = %q, want empty", errOut)
	}
}

func TestVerboseTracesDetectionAndPages(t *testing.T) {
	srv := cursorServer(t, 23)
	_, _, errOut := run("-v", "-q", srv.URL)
	for _, want := range []string{"detected style=cursor", "items found at /data", "page 3:"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

func TestHeadersAreForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	code, _, _ := run("-q", "-H", "Authorization: Bearer sesame", srv.URL)
	if code != 0 || got != "Bearer sesame" {
		t.Fatalf("code=%d Authorization=%q", code, got)
	}
}

func TestPagesModeEmitsWholePages(t *testing.T) {
	srv := cursorServer(t, 23)
	code, out, errOut := run("--pages", srv.URL)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 raw pages", len(lines))
	}
	if !strings.Contains(lines[0], `"next_cursor"`) {
		t.Fatalf("page line lost its envelope: %q", lines[0])
	}
	if !strings.Contains(errOut, "pages emitted=3") {
		t.Fatalf("summary = %q", errOut)
	}
}

func TestItemsPointerOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"id": 99}], "audit": [{"id": 1}, {"id": 2}]}`)
	}))
	t.Cleanup(srv.Close)
	code, out, _ := run("-q", "--items", "/audit", srv.URL)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if got := strings.Count(out, "\n"); got != 2 {
		t.Fatalf("got %d lines, want 2 (from /audit, not /data)", got)
	}
}

func TestMaxItemsFlagLimitsOutput(t *testing.T) {
	srv := cursorServer(t, 40)
	code, out, _ := run("-q", "--max-items", "15", srv.URL)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if got := strings.Count(out, "\n"); got != 15 {
		t.Fatalf("got %d lines, want 15", got)
	}
}

func TestHTTPFailureExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	code, _, errOut := run("-q", "--retries", "0", srv.URL)
	if code != 1 || !strings.Contains(errOut, "403") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestMissingItemsArrayExitsOneWithHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id": 7, "status": "ok"}`)
	}))
	t.Cleanup(srv.Close)
	code, _, errOut := run("-q", srv.URL)
	if code != 1 || !strings.Contains(errOut, "--items") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestInlineFlagValueSyntax(t *testing.T) {
	srv := cursorServer(t, 12)
	code, out, _ := run("-q", "--max-items=4", srv.URL)
	if code != 0 || strings.Count(out, "\n") != 4 {
		t.Fatalf("code=%d lines=%d", code, strings.Count(out, "\n"))
	}
}
