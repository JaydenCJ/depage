// Package pager is the engine: it fetches page after page, resolves the
// pagination plan on the first response, extracts the records from each
// page, and computes the next request until a termination rule fires. It
// never sleeps or retries on its own clock — the sleep function is
// injected so tests stay instant and deterministic.
package pager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JaydenCJ/depage/internal/detect"
	"github.com/JaydenCJ/depage/internal/extract"
	"github.com/JaydenCJ/depage/internal/jsonptr"
	"github.com/JaydenCJ/depage/internal/linkhdr"
	"github.com/JaydenCJ/depage/internal/version"
)

// retryAfterCap bounds how long a Retry-After header can make us wait.
const retryAfterCap = 30 * time.Second

// Options configures one Run. The zero value plus a URL is a working
// configuration: auto-detection, no caps, two retries.
type Options struct {
	Style     detect.Style // StyleAuto ("" is treated as auto) or a forced family
	ItemsPtr  string       // explicit JSON pointer to the items array
	Overrides detect.Overrides

	MaxPages int // stop after N pages (0 = unlimited)
	MaxItems int // stop after N items (0 = unlimited); the last page is truncated

	Retries   int           // additional attempts on 429/5xx/network errors
	RetryWait time.Duration // base backoff, doubled per attempt
	Delay     time.Duration // politeness delay between page fetches

	// RequireItems makes a page without a locatable items array fatal.
	// Item emission needs it; raw --pages mode can do without (except for
	// styles whose termination depends on item counts).
	RequireItems bool

	Header http.Header  // extra request headers (Accept and User-Agent are defaulted)
	Client *http.Client // nil = http.DefaultClient semantics with a 30s timeout

	Sleep func(time.Duration)           // nil = time.Sleep
	Logf  func(format string, a ...any) // verbose diagnostics; nil = discard
	Warnf func(format string, a ...any) // user-facing warnings; nil = discard
}

// Page is one fetched page, handed to the emit callback in order.
type Page struct {
	Number   int    // 1-based
	URL      string // the URL actually fetched
	Doc      any    // decoded body (json.Number for numbers)
	Items    []any  // extracted records (nil when extraction failed and was not required)
	ItemsPtr string // where the items were found ("" = top-level array)
	Header   http.Header
}

// Summary reports what a completed Run did.
type Summary struct {
	Style detect.Style
	Via   string
	Pages int
	Items int
}

// Run streams pages from rawURL until the detected plan says the stream is
// over, emit returns an error, or a cap fires. Pages already emitted stay
// emitted — depage streams, it does not buffer the whole dataset.
func Run(ctx context.Context, rawURL string, opts Options, emit func(*Page) error) (*Summary, error) {
	cur, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %v", rawURL, err)
	}
	if cur.Scheme != "http" && cur.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (want http or https)", cur.Scheme)
	}
	if opts.Style == "" {
		opts.Style = detect.StyleAuto
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	warnf := opts.Warnf
	if warnf == nil {
		warnf = func(string, ...any) {}
	}

	sum := &Summary{}
	visited := map[string]bool{}
	var plan detect.Plan
	prevCursor := ""
	firstPageSize := -1

	for {
		if opts.MaxPages > 0 && sum.Pages >= opts.MaxPages {
			logf("stopping: --max-pages %d reached", opts.MaxPages)
			return sum, nil
		}
		if sum.Pages > 0 && opts.Delay > 0 {
			sleep(opts.Delay)
		}
		visited[cur.String()] = true

		doc, header, err := fetchJSON(ctx, client, cur, opts, sleep, logf)
		if err != nil {
			return sum, err
		}
		sum.Pages++

		if sum.Pages == 1 {
			plan = detect.Detect(cur, header, doc, opts.Style, opts.Overrides)
			sum.Style, sum.Via = plan.Style, plan.Via
			logf("detected style=%s via %s", plan.Style, plan.Via)
		}

		items, itemsPtr, ierr := extract.Items(doc, opts.ItemsPtr)
		if ierr != nil {
			if opts.RequireItems || plan.Style == detect.StyleOffset {
				return sum, fmt.Errorf("page %d (%s): %v", sum.Pages, cur, ierr)
			}
			items = nil
		}
		if sum.Pages == 1 {
			firstPageSize = len(items)
			if ierr == nil {
				logf("items found at %s (%d on the first page)", displayPtr(itemsPtr), len(items))
			}
		}

		truncated := false
		if opts.MaxItems > 0 && sum.Items+len(items) > opts.MaxItems {
			items = items[:opts.MaxItems-sum.Items]
			truncated = true
		}
		page := &Page{
			Number:   sum.Pages,
			URL:      cur.String(),
			Doc:      doc,
			Items:    items,
			ItemsPtr: itemsPtr,
			Header:   header,
		}
		if err := emit(page); err != nil {
			return sum, err
		}
		sum.Items += len(items)
		logf("page %d: %d item(s) from %s", page.Number, len(items), page.URL)
		if truncated {
			logf("stopping: --max-items %d reached", opts.MaxItems)
			return sum, nil
		}

		next, done, err := nextRequest(&plan, cur, header, doc, items, ierr, firstPageSize, &prevCursor, logf)
		if err != nil {
			return sum, err
		}
		if done {
			return sum, nil
		}
		if visited[next.String()] {
			warnf("next page repeats an already-fetched URL (%s); stopping to avoid a loop", next)
			return sum, nil
		}
		cur = next
	}
}

// nextRequest computes the follow-up request per the plan's style, or
// reports that the stream is complete. itemsErr is the extraction error
// (if any) so count-dependent styles can fail loudly instead of looping.
func nextRequest(plan *detect.Plan, cur *url.URL, header http.Header, doc any, items []any, itemsErr error, firstPageSize int, prevCursor *string, logf func(string, ...any)) (*url.URL, bool, error) {
	switch plan.Style {
	case detect.StyleLink:
		target := linkhdr.Next(header)
		if target == "" {
			return nil, true, nil
		}
		return resolveRef(cur, target)

	case detect.StyleNextURL:
		if plan.NextPtr == "" {
			return nil, true, nil // forced style, no field on page 1: single page
		}
		val, ok := jsonptr.Get(doc, plan.NextPtr)
		if !ok || val == nil {
			return nil, true, nil
		}
		target, isStr := val.(string)
		if !isStr {
			return nil, false, fmt.Errorf("next-URL field %s is not a string", plan.NextPtr)
		}
		if target == "" {
			return nil, true, nil
		}
		return resolveRef(cur, target)

	case detect.StyleCursor:
		if plan.NextPtr == "" {
			return nil, true, nil
		}
		val, ok := jsonptr.Get(doc, plan.NextPtr)
		if !ok || val == nil {
			return nil, true, nil
		}
		cursor, err := scalarString(val)
		if err != nil {
			return nil, false, fmt.Errorf("cursor field %s: %v", plan.NextPtr, err)
		}
		if cursor == "" {
			return nil, true, nil
		}
		if cursor == *prevCursor {
			logf("cursor repeated (%q); treating as end of stream", cursor)
			return nil, true, nil
		}
		*prevCursor = cursor
		return withParam(cur, plan.CursorParam, cursor), false, nil

	case detect.StyleOffset:
		if itemsErr != nil {
			return nil, false, fmt.Errorf("offset pagination needs a locatable items array: %v", itemsErr)
		}
		if len(items) == 0 {
			return nil, true, nil
		}
		offset := queryInt(cur, plan.OffsetParam, 0)
		next := offset + len(items)
		if total, ok := numberAt(doc, plan.TotalPtr); ok && int64(next) >= total {
			return nil, true, nil
		}
		if limit := queryInt(cur, plan.LimitParam, 0); limit > 0 && len(items) < limit {
			return nil, true, nil // short page = last page
		}
		return withParam(cur, plan.OffsetParam, strconv.Itoa(next)), false, nil

	case detect.StylePage:
		pageNo := queryInt(cur, plan.PageParam, 1)
		if total, ok := numberAt(doc, plan.TotalPagesPtr); ok {
			if int64(pageNo) >= total {
				return nil, true, nil
			}
		} else {
			if itemsErr != nil {
				return nil, false, fmt.Errorf("page pagination without a total-pages field needs a locatable items array: %v", itemsErr)
			}
			if len(items) == 0 {
				return nil, true, nil
			}
			if firstPageSize > 0 && len(items) < firstPageSize {
				return nil, true, nil // short page = last page
			}
		}
		return withParam(cur, plan.PageParam, strconv.Itoa(pageNo+1)), false, nil

	default: // StyleNone
		return nil, true, nil
	}
}

// fetchJSON performs one GET with retry/backoff and decodes the body. All
// numbers decode as json.Number so cursors and totals keep their exact
// textual form.
func fetchJSON(ctx context.Context, client *http.Client, u *url.URL, opts Options, sleep func(time.Duration), logf func(string, ...any)) (any, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if attempt > 0 {
			wait := backoff(opts.RetryWait, attempt)
			if ra, ok := lastRetryAfter(lastErr); ok {
				wait = ra
			}
			logf("retrying %s in %s (attempt %d/%d)", u, wait, attempt+1, opts.Retries+1)
			if wait > 0 {
				sleep(wait)
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "depage/"+version.Version)
		for name, values := range opts.Header {
			req.Header[http.CanonicalHeaderKey(name)] = values
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			lastErr = fmt.Errorf("GET %s: %v", u, err)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("GET %s: reading body: %v", u, readErr)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			doc, err := decodeJSON(body)
			if err != nil {
				return nil, nil, fmt.Errorf("GET %s: %v", u, err)
			}
			return doc, resp.Header, nil
		}
		herr := &httpError{url: u.String(), status: resp.Status, snippet: snippet(body)}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 {
				d := time.Duration(secs) * time.Second
				if d > retryAfterCap {
					d = retryAfterCap
				}
				herr.retryAfter = &d
			}
		}
		if !retryable(resp.StatusCode) {
			return nil, nil, herr
		}
		lastErr = herr
	}
	attempts := opts.Retries + 1
	if attempts == 1 {
		return nil, nil, fmt.Errorf("%v (after 1 attempt; raise --retries to retry)", lastErr)
	}
	return nil, nil, fmt.Errorf("%v (after %d attempts)", lastErr, attempts)
}

// httpError is a non-2xx response, carrying any Retry-After hint.
type httpError struct {
	url        string
	status     string
	snippet    string
	retryAfter *time.Duration
}

func (e *httpError) Error() string {
	if e.snippet == "" {
		return fmt.Sprintf("GET %s: HTTP %s", e.url, e.status)
	}
	return fmt.Sprintf("GET %s: HTTP %s: %s", e.url, e.status, e.snippet)
}

func lastRetryAfter(err error) (time.Duration, bool) {
	var herr *httpError
	if errors.As(err, &herr) && herr.retryAfter != nil {
		return *herr.retryAfter, true
	}
	return 0, false
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff doubles the base wait per attempt: base, 2*base, 4*base, …
func backoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > retryAfterCap {
			return retryAfterCap
		}
	}
	return d
}

// decodeJSON decodes exactly one JSON document, rejecting empty bodies and
// trailing garbage (a truncated proxy response should fail loudly, not
// silently drop records).
func decodeJSON(body []byte) (any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("empty response body (expected a JSON document)")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, errors.New("response contains trailing data after the JSON document")
	}
	return doc, nil
}

// scalarString renders a cursor value: strings verbatim, numbers in their
// original textual form.
func scalarString(val any) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("value is a %T, not a string or number", val)
}

// resolveRef resolves a next-page reference (absolute or relative) against
// the current page URL and validates the result.
func resolveRef(cur *url.URL, ref string) (*url.URL, bool, error) {
	parsed, err := url.Parse(ref)
	if err != nil {
		return nil, false, fmt.Errorf("next page reference %q is not a valid URL: %v", ref, err)
	}
	next := cur.ResolveReference(parsed)
	if next.Scheme != "http" && next.Scheme != "https" {
		return nil, false, fmt.Errorf("next page reference %q resolves to unsupported scheme %q", ref, next.Scheme)
	}
	return next, false, nil
}

// withParam returns a copy of u with one query parameter set.
func withParam(u *url.URL, key, value string) *url.URL {
	next := *u
	q := next.Query()
	q.Set(key, value)
	next.RawQuery = q.Encode()
	return &next
}

// queryInt reads an integer query parameter, falling back to def when the
// parameter is absent or malformed.
func queryInt(u *url.URL, key string, def int) int {
	if key == "" {
		return def
	}
	if s := u.Query().Get(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// numberAt resolves ptr to an integer, tolerating json.Number and float64.
func numberAt(doc any, ptr string) (int64, bool) {
	if ptr == "" {
		return 0, false
	}
	val, ok := jsonptr.Get(doc, ptr)
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	}
	return 0, false
}

// snippet renders the first line-ish of an error body for messages,
// truncating on a rune boundary so multibyte text is never cut mid-character.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > 160 {
		cut := 160
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// displayPtr renders an items location for logs ("" means top level).
func displayPtr(ptr string) string {
	if ptr == "" {
		return "the top-level array"
	}
	return ptr
}
