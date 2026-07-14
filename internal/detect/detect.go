// Package detect inspects the *first* response of a paginated JSON API —
// request URL, response headers, and decoded body — and figures out which
// pagination style the API speaks, so the caller never has to write
// per-API adapter code. The probe order and every candidate location are
// documented in docs/detection.md; keep the two in sync.
package detect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/JaydenCJ/depage/internal/jsonptr"
	"github.com/JaydenCJ/depage/internal/linkhdr"
)

// Style is a pagination family. Auto means "detect on the first page".
type Style string

const (
	StyleAuto    Style = "auto"
	StyleLink    Style = "link-header"
	StyleNextURL Style = "next-url"
	StyleCursor  Style = "cursor"
	StyleOffset  Style = "offset"
	StylePage    Style = "page"
	StyleNone    Style = "none"
)

// ParseStyle converts a user-supplied --style value into a Style.
func ParseStyle(s string) (Style, error) {
	switch Style(strings.ToLower(s)) {
	case StyleAuto, StyleLink, StyleNextURL, StyleCursor, StyleOffset, StylePage, StyleNone:
		return Style(strings.ToLower(s)), nil
	case "link":
		return StyleLink, nil // common shorthand
	}
	return "", fmt.Errorf("unknown style %q (want auto, link-header, next-url, cursor, offset, page, or none)", s)
}

// Overrides carries the user-supplied knobs that beat auto-derivation.
// Empty fields mean "derive it".
type Overrides struct {
	NextPtr     string // JSON pointer to the next cursor / next URL field
	CursorParam string
	PageParam   string
	OffsetParam string
	LimitParam  string
}

// Plan is a resolved pagination strategy: everything the pager needs to
// compute "the next request" from "the current page".
type Plan struct {
	Style Style
	Via   string // human-readable provenance for logs, e.g. `body field /next_cursor`

	// Cursor and next-URL styles.
	NextPtr     string
	CursorParam string

	// Offset style.
	OffsetParam string
	LimitParam  string
	TotalPtr    string // pointer to a total-item-count field, "" if unknown

	// Page-number style.
	PageParam     string
	TotalPagesPtr string // pointer to a total-page-count field, "" if unknown
}

// Ordered candidate locations. Order encodes priority: the specific and
// unambiguous names come first, the generic ones (/next, /cursor) last.
var nextURLPtrs = []string{
	"/links/next", "/links/next/href", "/_links/next/href",
	"/paging/next", "/pagination/next", "/pagination/next_url",
	"/pagination/next_link", "/meta/next",
	"/next_url", "/next_page_url", "/nextUrl", "/nextLink", "/@odata.nextLink",
	"/next", "/next_page", // only when the value looks like a URL
}

var cursorPtrs = []string{
	"/next_cursor", "/nextCursor",
	"/meta/next_cursor", "/meta/cursor",
	"/pagination/next_cursor", "/pagination/cursor",
	"/response_metadata/next_cursor", // the Slack Web API dialect
	"/paging/cursors/after", "/paging/next_cursor",
	"/next_page_token", "/nextPageToken", "/nextToken",
	"/continuation_token", "/continuation",
	"/cursor",
	"/next", "/next_page", // only when the value is an opaque scalar, not a URL
}

var totalPagesPtrs = []string{
	"/total_pages", "/totalPages",
	"/meta/total_pages", "/pagination/total_pages",
	"/page_count", "/pageCount",
}

var totalItemsPtrs = []string{
	"/total", "/total_count", "/totalCount",
	"/meta/total", "/meta/total_count", "/pagination/total",
}

var pageURLParams = []string{"page", "page_number", "pageNumber", "page_no"}

var offsetURLParams = []string{"offset", "skip"}

var limitURLParams = []string{"limit", "per_page", "perPage", "page_size", "pageSize"}

// knownCursorParams are query parameter names that already carry a cursor
// in the initial request URL; when present, the same name is reused for
// subsequent requests.
var knownCursorParams = []string{
	"cursor", "page_token", "pageToken", "after",
	"continuation", "continuation_token", "next_token", "nextToken", "next_cursor",
}

// Detect resolves a Plan for the given first response. want narrows the
// search to one family (StyleAuto tries them all in priority order:
// Link header, body next-URL, cursor, page, offset). Detect never fails:
// when nothing matches — or the forced family has no marker on this page —
// it returns a usable Plan whose termination rules simply end the stream.
func Detect(req *url.URL, header http.Header, doc any, want Style, ov Overrides) Plan {
	if ov.NextPtr != "" && (want == StyleAuto || want == StyleCursor || want == StyleNextURL) {
		if p, ok := planFromNextPtr(req, doc, want, ov); ok {
			return p
		}
	}
	switch want {
	case StyleAuto:
		if p, ok := detectLink(header); ok {
			return p
		}
		if p, ok := detectNextURL(doc); ok {
			return p
		}
		if p, ok := detectCursor(req, doc, ov); ok {
			return p
		}
		if p, ok := detectPage(req, doc, ov, false); ok {
			return p
		}
		if p, ok := detectOffset(req, doc, ov, false); ok {
			return p
		}
		return Plan{Style: StyleNone, Via: "no pagination markers found on the first page"}
	case StyleLink:
		if p, ok := detectLink(header); ok {
			return p
		}
		return Plan{Style: StyleLink, Via: "forced by --style (no rel=\"next\" on the first page)"}
	case StyleNextURL:
		if p, ok := detectNextURL(doc); ok {
			return p
		}
		return Plan{Style: StyleNextURL, Via: "forced by --style (no next-URL field on the first page)"}
	case StyleCursor:
		if p, ok := detectCursor(req, doc, ov); ok {
			return p
		}
		return Plan{Style: StyleCursor, Via: "forced by --style (no cursor field on the first page)"}
	case StylePage:
		p, _ := detectPage(req, doc, ov, true)
		return p
	case StyleOffset:
		p, _ := detectOffset(req, doc, ov, true)
		return p
	default:
		return Plan{Style: StyleNone, Via: "forced by --style"}
	}
}

// planFromNextPtr honors an explicit --next pointer: if the addressed value
// looks like a URL the plan is next-url, otherwise cursor (unless the
// caller forced one of the two).
func planFromNextPtr(req *url.URL, doc any, want Style, ov Overrides) (Plan, bool) {
	val, ok := jsonptr.Get(doc, ov.NextPtr)
	if !ok || val == nil {
		return Plan{}, false
	}
	s, isStr := val.(string)
	urlish := isStr && isURLish(s)
	style := want
	if style == StyleAuto {
		if urlish {
			style = StyleNextURL
		} else {
			style = StyleCursor
		}
	}
	if style == StyleNextURL {
		return Plan{Style: StyleNextURL, Via: fmt.Sprintf("--next pointer %s", ov.NextPtr), NextPtr: ov.NextPtr}, true
	}
	return Plan{
		Style:       StyleCursor,
		Via:         fmt.Sprintf("--next pointer %s", ov.NextPtr),
		NextPtr:     ov.NextPtr,
		CursorParam: cursorParamFor(ov.NextPtr, req, ov),
	}, true
}

func detectLink(header http.Header) (Plan, bool) {
	if linkhdr.Next(header) == "" {
		return Plan{}, false
	}
	return Plan{Style: StyleLink, Via: `Link header rel="next"`}, true
}

func detectNextURL(doc any) (Plan, bool) {
	for _, ptr := range nextURLPtrs {
		val, ok := jsonptr.Get(doc, ptr)
		if !ok || val == nil {
			continue
		}
		if s, isStr := val.(string); isStr && isURLish(s) {
			return Plan{Style: StyleNextURL, Via: "body field " + ptr, NextPtr: ptr}, true
		}
	}
	return Plan{}, false
}

func detectCursor(req *url.URL, doc any, ov Overrides) (Plan, bool) {
	for _, ptr := range cursorPtrs {
		val, ok := jsonptr.Get(doc, ptr)
		if !ok || val == nil {
			continue
		}
		if !scalarCursor(val) {
			continue
		}
		return Plan{
			Style:       StyleCursor,
			Via:         "body field " + ptr,
			NextPtr:     ptr,
			CursorParam: cursorParamFor(ptr, req, ov),
		}, true
	}
	return Plan{}, false
}

// scalarCursor accepts non-empty strings that are not URLs (those belong to
// the next-url family) and numbers; booleans and containers are not cursors.
func scalarCursor(val any) bool {
	switch v := val.(type) {
	case string:
		return v != "" && !isURLish(v)
	case json.Number:
		return true
	case float64: // callers that decoded without UseNumber
		return true
	}
	return false
}

// detectPage recognizes page-number pagination from a ?page= style query
// parameter or a total-pages field in the body. forced makes the family
// unconditional (defaults filled in).
func detectPage(req *url.URL, doc any, ov Overrides, forced bool) (Plan, bool) {
	plan := Plan{Style: StylePage, PageParam: ov.PageParam}
	q := req.Query()
	if plan.PageParam == "" {
		for _, p := range pageURLParams {
			if q.Has(p) {
				plan.PageParam = p
				plan.Via = "query parameter ?" + p + "="
				break
			}
		}
	} else {
		plan.Via = "--page-param " + plan.PageParam
	}
	for _, ptr := range totalPagesPtrs {
		if isNumber(doc, ptr) {
			plan.TotalPagesPtr = ptr
			if plan.Via == "" {
				plan.Via = "body field " + ptr
			}
			break
		}
	}
	if plan.PageParam == "" {
		plan.PageParam = "page"
	}
	if plan.Via == "" {
		if !forced {
			return Plan{}, false
		}
		plan.Via = "forced by --style"
	}
	return plan, true
}

// detectOffset recognizes offset/limit pagination from ?offset= / ?skip=
// query parameters or a total-count field in the body.
func detectOffset(req *url.URL, doc any, ov Overrides, forced bool) (Plan, bool) {
	plan := Plan{Style: StyleOffset, OffsetParam: ov.OffsetParam, LimitParam: ov.LimitParam}
	q := req.Query()
	if plan.OffsetParam == "" {
		for _, p := range offsetURLParams {
			if q.Has(p) {
				plan.OffsetParam = p
				plan.Via = "query parameter ?" + p + "="
				break
			}
		}
	} else {
		plan.Via = "--offset-param " + plan.OffsetParam
	}
	for _, ptr := range totalItemsPtrs {
		if isNumber(doc, ptr) {
			plan.TotalPtr = ptr
			if plan.Via == "" {
				plan.Via = "body field " + ptr
			}
			break
		}
	}
	if plan.LimitParam == "" {
		for _, p := range limitURLParams {
			if q.Has(p) {
				plan.LimitParam = p
				break
			}
		}
	}
	if plan.OffsetParam == "" {
		plan.OffsetParam = "offset"
	}
	if plan.Via == "" {
		if !forced {
			return Plan{}, false
		}
		plan.Via = "forced by --style"
	}
	return plan, true
}

// cursorParamFor picks the query parameter that carries the cursor on
// subsequent requests: an explicit --cursor-param wins, then a cursor-like
// parameter already present in the request URL, then a mapping from the
// field name the cursor was found under.
func cursorParamFor(ptr string, req *url.URL, ov Overrides) string {
	if ov.CursorParam != "" {
		return ov.CursorParam
	}
	q := req.Query()
	for _, p := range knownCursorParams {
		if q.Has(p) {
			return p
		}
	}
	switch name := jsonptr.Basename(ptr); name {
	case "next_cursor", "nextCursor", "cursor", "next", "next_page":
		return "cursor"
	case "next_page_token":
		return "page_token"
	case "nextPageToken":
		return "pageToken"
	case "nextToken":
		return "nextToken"
	case "after":
		return "after"
	case "continuation", "continuation_token":
		return name
	default:
		if trimmed := strings.TrimPrefix(name, "next_"); trimmed != name && trimmed != "" {
			return trimmed
		}
		return "cursor"
	}
}

// isURLish reports whether s plausibly names a follow-up request target:
// absolute http(s) URLs and absolute-path references count; opaque tokens
// (even ones containing slashes, like base64) do not.
func isURLish(s string) bool {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}
	if strings.HasPrefix(s, "/") {
		// Require something path-like, not a stray "/" or "//opaque".
		return len(s) > 1 && !strings.HasPrefix(s, "//")
	}
	return false
}

// isNumber reports whether ptr resolves inside doc to a JSON number.
func isNumber(doc any, ptr string) bool {
	val, ok := jsonptr.Get(doc, ptr)
	if !ok {
		return false
	}
	switch val.(type) {
	case json.Number, float64:
		return true
	}
	return false
}
