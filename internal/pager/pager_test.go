// Tests for the paging engine, driven against in-process httptest servers
// on 127.0.0.1. Every server is deterministic; every sleep goes through an
// injected recorder, so nothing here waits on a real clock.
package pager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/JaydenCJ/depage/internal/detect"
)

// collect runs the pager and gathers every emitted item's "id" field, so
// assertions can check both count and order.
func collect(t *testing.T, url string, opts Options) ([]int, *Summary) {
	t.Helper()
	opts.RequireItems = true
	opts.Sleep = func(time.Duration) {} // never block a test
	var ids []int
	sum, err := Run(context.Background(), url, opts, func(p *Page) error {
		for _, item := range p.Items {
			obj := item.(map[string]any)
			n, _ := obj["id"].(json.Number).Int64()
			ids = append(ids, int(n))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	return ids, sum
}

// wantIDs asserts ids == 1..n in order: pagination must neither drop nor
// duplicate records.
func wantIDs(t *testing.T, ids []int, n int) {
	t.Helper()
	if len(ids) != n {
		t.Fatalf("got %d items, want %d", len(ids), n)
	}
	for i, id := range ids {
		if id != i+1 {
			t.Fatalf("ids[%d] = %d, want %d (order or dedup broken)", i, id, i+1)
		}
	}
}

// records renders records start..end (1-based, inclusive) as JSON objects.
func records(start, end int) []map[string]any {
	out := []map[string]any{}
	for i := start; i <= end; i++ {
		out = append(out, map[string]any{"id": i})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestCursorPaginationWalksAllPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 1
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		end := start + 9
		body := map[string]any{"data": records(start, min(end, 25)), "next_cursor": nil}
		if end < 25 {
			body["next_cursor"] = strconv.Itoa(end + 1)
		}
		writeJSON(w, body)
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{})
	wantIDs(t, ids, 25)
	if sum.Style != detect.StyleCursor || sum.Pages != 3 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestCursorRepeatedValueEndsStream(t *testing.T) {
	// Some APIs re-send the final cursor forever; the repeat guard must
	// treat that as end-of-stream, not loop.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			writeJSON(w, map[string]any{"data": records(1, 10), "next_cursor": "LAST"})
			return
		}
		writeJSON(w, map[string]any{"data": records(11, 12), "next_cursor": "LAST"})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{})
	wantIDs(t, ids, 12)
	if sum.Pages != 2 {
		t.Fatalf("pages = %d, want 2 (stopped on repeated cursor)", sum.Pages)
	}
}

func TestOffsetPaginationHonorsTotal(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		end := min(offset+10, 23)
		writeJSON(w, map[string]any{"total": 23, "items": records(offset+1, end)})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?offset=0&limit=10", Options{})
	wantIDs(t, ids, 23)
	if sum.Style != detect.StyleOffset {
		t.Fatalf("style = %s", sum.Style)
	}
	if hits != 3 {
		t.Fatalf("server hit %d times, want 3 (total must stop the walk)", hits)
	}
}

func TestOffsetShortPageStopsWithoutTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		writeJSON(w, map[string]any{"items": records(offset+1, min(offset+10, 15))})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?offset=0&limit=10", Options{})
	wantIDs(t, ids, 15)
	if sum.Pages != 2 {
		t.Fatalf("pages = %d, want 2 (short page = last page)", sum.Pages)
	}
}

func TestPagePaginationHonorsTotalPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		writeJSON(w, map[string]any{
			"page": page, "total_pages": 3,
			"results": records((page-1)*10+1, min(page*10, 25)),
		})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?page=1", Options{})
	wantIDs(t, ids, 25)
	if sum.Style != detect.StylePage || sum.Pages != 3 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestPageShortPageHeuristicWithoutTotalPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		writeJSON(w, map[string]any{"results": records((page-1)*10+1, min(page*10, 12))})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?page=1", Options{})
	wantIDs(t, ids, 12)
	if sum.Pages != 2 {
		t.Fatalf("pages = %d, want 2", sum.Pages)
	}
}

func TestPageEmptyPageStops(t *testing.T) {
	// Constant page size with a trailing empty page — the other way page
	// APIs signal the end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page > 2 {
			writeJSON(w, map[string]any{"results": []any{}})
			return
		}
		writeJSON(w, map[string]any{"results": records((page-1)*5+1, page*5)})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?page=1", Options{})
	wantIDs(t, ids, 10)
	if sum.Pages != 3 {
		t.Fatalf("pages = %d, want 3 (last one empty)", sum.Pages)
	}
}

func TestLinkHeaderPaginationWithRelativeTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if page < 3 {
			// Relative reference: must be resolved against the page URL.
			w.Header().Set("Link", fmt.Sprintf(`</?page=%d>; rel="next", </?page=3>; rel="last"`, page+1))
		}
		writeJSON(w, records((page-1)*10+1, min(page*10, 27)))
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{})
	wantIDs(t, ids, 27)
	if sum.Style != detect.StyleLink || sum.Pages != 3 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestNextURLBodyLinkRelativeResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		links := map[string]any{"next": nil}
		if page < 2 {
			links["next"] = fmt.Sprintf("/?page=%d", page+1)
		}
		writeJSON(w, map[string]any{"records": records((page-1)*10+1, min(page*10, 14)), "links": links})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{})
	wantIDs(t, ids, 14)
	if sum.Style != detect.StyleNextURL {
		t.Fatalf("style = %s", sum.Style)
	}
}

func TestUnpaginatedResponseIsSinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, records(1, 8))
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{})
	wantIDs(t, ids, 8)
	if sum.Style != detect.StyleNone || sum.Pages != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestMaxPagesCapsTheWalk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 1
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		writeJSON(w, map[string]any{"data": records(start, start+4), "next_cursor": strconv.Itoa(start + 5)})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{MaxPages: 3})
	wantIDs(t, ids, 15)
	if sum.Pages != 3 {
		t.Fatalf("pages = %d, want 3", sum.Pages)
	}
}

func TestMaxItemsTruncatesMidPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 1
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		writeJSON(w, map[string]any{"data": records(start, start+9), "next_cursor": strconv.Itoa(start + 10)})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL, Options{MaxItems: 17})
	wantIDs(t, ids, 17)
	if sum.Pages != 2 || sum.Items != 17 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestLoopingNextURLStopsWithWarning(t *testing.T) {
	// A next link pointing back at an already-fetched URL must stop the
	// walk (records already streamed stay valid) and warn, not spin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			writeJSON(w, map[string]any{"items": records(1, 5), "next": "/?page=2"})
			return
		}
		writeJSON(w, map[string]any{"items": records(6, 10), "next": "/?page=1"})
	}))
	defer srv.Close()

	var warned string
	opts := Options{
		RequireItems: true,
		Sleep:        func(time.Duration) {},
		Warnf:        func(f string, a ...any) { warned = fmt.Sprintf(f, a...) },
	}
	count := 0
	sum, err := Run(context.Background(), srv.URL+"/?page=1", opts, func(p *Page) error {
		count += len(p.Items)
		return nil
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if sum.Pages != 2 || count != 10 {
		t.Fatalf("pages=%d count=%d, want 2/10", sum.Pages, count)
	}
	if !strings.Contains(warned, "loop") {
		t.Fatalf("warning = %q, want a loop mention", warned)
	}
}

func TestRetryAfter429HonorsHeader(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "3")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		writeJSON(w, map[string]any{"data": records(1, 4)})
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := Options{
		RequireItems: true,
		Retries:      2,
		RetryWait:    time.Millisecond,
		Sleep:        func(d time.Duration) { slept = append(slept, d) },
	}
	sum, err := Run(context.Background(), srv.URL, opts, func(*Page) error { return nil })
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if sum.Items != 4 || hits != 2 {
		t.Fatalf("items=%d hits=%d", sum.Items, hits)
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("slept %v, want exactly the Retry-After of 3s", slept)
	}
}

func TestRetriesExhaustedReturnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	opts := Options{Retries: 2, RetryWait: time.Millisecond, Sleep: func(time.Duration) {}}
	_, err := Run(context.Background(), srv.URL, opts, func(*Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("err = %v, want a 503 after 3 attempts", err)
	}
}

func TestSingleAttemptErrorReadsSingular(t *testing.T) {
	// --retries 0 means exactly one attempt; the error must not say
	// "after 1 attempts" (and should point at the knob that adds retries).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	opts := Options{Retries: 0, Sleep: func(time.Duration) {}}
	_, err := Run(context.Background(), srv.URL, opts, func(*Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "after 1 attempt;") {
		t.Fatalf("err = %v, want a singular 'after 1 attempt'", err)
	}
	if strings.Contains(err.Error(), "1 attempts") {
		t.Fatalf("err = %v, grammar regression: '1 attempts'", err)
	}
}

func TestSnippetTruncatesOnRuneBoundary(t *testing.T) {
	// An error body of multibyte text longer than the cap must be cut at a
	// rune boundary — a snippet ending in a broken UTF-8 sequence would
	// garble the one error message the user gets.
	long := strings.Repeat("エラー", 60) // 3 bytes per rune, 540 bytes total
	got := snippet([]byte(long))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("snippet %q: expected truncation marker", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("snippet %q is not valid UTF-8", got)
	}
	if len(got) > 160+len("…") {
		t.Fatalf("snippet is %d bytes, want ≤ %d", len(got), 160+len("…"))
	}
}

func TestClientErrorFailsWithoutRetry(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, `{"error":"no such collection"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	opts := Options{Retries: 3, Sleep: func(time.Duration) {}}
	_, err := Run(context.Background(), srv.URL, opts, func(*Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want a 404", err)
	}
	if hits != 1 {
		t.Fatalf("server hit %d times; 4xx must not be retried", hits)
	}
}

func TestNonJSONBodiesErrorLoudly(t *testing.T) {
	// HTML error pages, empty 200s, and truncated/concatenated JSON must
	// all fail with a named cause — never emit half a dataset silently.
	cases := []struct{ body, want string }{
		{`<html>definitely not json</html>`, "not valid JSON"},
		{"", "empty response body"},
		{`{"data": []}{"data": []}`, "trailing data"},
	}
	for _, c := range cases {
		body := c.body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		_, err := Run(context.Background(), srv.URL, Options{}, func(*Page) error { return nil })
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("body %q: err = %v, want %q", c.body, err, c.want)
		}
	}
}

func TestForcedStyleBeatsAutoDetection(t *testing.T) {
	// The body dangles a bogus cursor; auto would follow it forever. The
	// forced offset style paginates by ?offset= and terminates on total.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		writeJSON(w, map[string]any{
			"total": 12, "next_cursor": "bogus-constant",
			"items": records(offset+1, min(offset+10, 12)),
		})
	}))
	defer srv.Close()

	ids, sum := collect(t, srv.URL+"?offset=0&limit=10", Options{Style: detect.StyleOffset})
	wantIDs(t, ids, 12)
	if sum.Style != detect.StyleOffset {
		t.Fatalf("style = %s", sum.Style)
	}
}

func TestDelayRunsBetweenPagesOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 1
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		body := map[string]any{"data": records(start, start+4), "next_cursor": nil}
		if start < 10 {
			body["next_cursor"] = strconv.Itoa(start + 5)
		}
		writeJSON(w, body)
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := Options{
		RequireItems: true,
		Delay:        250 * time.Millisecond,
		Sleep:        func(d time.Duration) { slept = append(slept, d) },
	}
	sum, err := Run(context.Background(), srv.URL, opts, func(*Page) error { return nil })
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// 3 pages -> exactly 2 gaps; no delay before the first request.
	if sum.Pages != 3 || len(slept) != 2 || slept[0] != 250*time.Millisecond {
		t.Fatalf("pages=%d slept=%v", sum.Pages, slept)
	}
}

func TestEmitErrorAbortsTheWalk(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(w, map[string]any{"data": records(1, 5), "next_cursor": strconv.Itoa(hits)})
	}))
	defer srv.Close()

	wantErr := fmt.Errorf("downstream pipe closed")
	_, err := Run(context.Background(), srv.URL, Options{}, func(*Page) error { return wantErr })
	if err != wantErr {
		t.Fatalf("err = %v, want the emit error verbatim", err)
	}
	if hits != 1 {
		t.Fatalf("server hit %d times; the walk must stop on emit failure", hits)
	}
}

func TestUnsupportedSchemeRejected(t *testing.T) {
	_, err := Run(context.Background(), "ftp://example.test/data", Options{}, func(*Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("err = %v", err)
	}
}

func TestCanceledContextSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, records(1, 3))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, srv.URL, Options{Sleep: func(time.Duration) {}}, func(*Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context cancellation", err)
	}
}
