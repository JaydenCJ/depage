// Tests for pagination-style detection: one case per real-world response
// shape, plus the priority rules that arbitrate when several markers are
// present at once.
package detect

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("bad test literal %q: %v", s, err)
	}
	return v
}

func reqURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("bad test URL %q: %v", s, err)
	}
	return u
}

func auto(t *testing.T, rawURL string, h http.Header, body string) Plan {
	t.Helper()
	return Detect(reqURL(t, rawURL), h, doc(t, body), StyleAuto, Overrides{})
}

func TestLinkHeaderBeatsBodyMarkers(t *testing.T) {
	// GitHub sends a Link header AND count fields; the header is the
	// authoritative signal and must win.
	h := http.Header{}
	h.Set("Link", `<https://api.example.test/u?page=2>; rel="next"`)
	p := auto(t, "https://api.example.test/u", h, `{"total_count": 99, "items": []}`)
	if p.Style != StyleLink {
		t.Fatalf("style = %s, want %s (via %s)", p.Style, StyleLink, p.Via)
	}
}

func TestNextURLBodyShapes(t *testing.T) {
	// One case per real-world next-URL envelope, including a relative path.
	cases := map[string]string{
		`{"records": [], "links": {"next": "https://api.example.test/u?page=2"}}`:    "/links/next",
		`{"value": [], "@odata.nextLink": "https://api.example.test/u?$skiptoken="}`: "/@odata.nextLink",
		`{"data": [], "next": "/u?page=2"}`:                                          "/next",
		`{"items": [], "_links": {"next": {"href": "/u?page=2"}}}`:                   "/_links/next/href",
	}
	for body, wantPtr := range cases {
		p := auto(t, "https://api.example.test/u", nil, body)
		if p.Style != StyleNextURL || p.NextPtr != wantPtr {
			t.Fatalf("body %s: got style=%s ptr=%s, want ptr %s", body, p.Style, p.NextPtr, wantPtr)
		}
	}
}

func TestOpaqueNextFieldIsCursorNotURL(t *testing.T) {
	// The same /next field with an opaque token must land in the cursor
	// family — this string is not fetchable.
	p := auto(t, "https://api.example.test/u", nil,
		`{"data": [], "next": "eyJvZmZzZXQiOjEwfQ=="}`)
	if p.Style != StyleCursor || p.NextPtr != "/next" || p.CursorParam != "cursor" {
		t.Fatalf("got style=%s ptr=%s param=%s", p.Style, p.NextPtr, p.CursorParam)
	}
}

func TestCursorFromNextCursorField(t *testing.T) {
	p := auto(t, "https://api.example.test/u", nil,
		`{"data": [], "next_cursor": "abc"}`)
	if p.Style != StyleCursor || p.NextPtr != "/next_cursor" || p.CursorParam != "cursor" {
		t.Fatalf("got style=%s ptr=%s param=%s", p.Style, p.NextPtr, p.CursorParam)
	}
	// The Slack Web API nests its token under response_metadata; it must
	// be found there and be sent back as ?cursor=.
	p = auto(t, "https://api.example.test/u", nil,
		`{"ok": true, "members": [], "response_metadata": {"next_cursor": "dXNlcjpV"}}`)
	if p.Style != StyleCursor || p.NextPtr != "/response_metadata/next_cursor" || p.CursorParam != "cursor" {
		t.Fatalf("slack shape: got style=%s ptr=%s param=%s", p.Style, p.NextPtr, p.CursorParam)
	}
}

func TestCursorFromNumericCursor(t *testing.T) {
	p := auto(t, "https://api.example.test/u", nil,
		`{"data": [], "next_cursor": 1234}`)
	if p.Style != StyleCursor {
		t.Fatalf("numeric cursors are cursors too; got %s", p.Style)
	}
}

func TestCursorParamResolution(t *testing.T) {
	// Priority: explicit --cursor-param, then a cursor-like parameter
	// already in the request URL, then a mapping from the field name.
	cases := []struct {
		url, body string
		ov        Overrides
		want      string
	}{
		{"https://api.example.test/u", `{"items": [], "nextPageToken": "t1"}`, Overrides{}, "pageToken"},
		{"https://api.example.test/u", `{"items": [], "next_page_token": "t1"}`, Overrides{}, "page_token"},
		{"https://api.example.test/u?after=x0", `{"items": [], "next_cursor": "x1"}`, Overrides{}, "after"},
		{"https://api.example.test/u", `{"items": [], "next_cursor": "x1"}`, Overrides{CursorParam: "starting_after"}, "starting_after"},
	}
	for _, c := range cases {
		p := Detect(reqURL(t, c.url), nil, doc(t, c.body), StyleAuto, c.ov)
		if p.Style != StyleCursor || p.CursorParam != c.want {
			t.Fatalf("url=%s body=%s: got style=%s param=%q, want %q", c.url, c.body, p.Style, p.CursorParam, c.want)
		}
	}
}

func TestBooleanNextFieldIsNotPagination(t *testing.T) {
	// {"next": true} (a has-more flag alone) is neither a URL nor a cursor.
	p := auto(t, "https://api.example.test/u", nil, `{"items": [], "next": true}`)
	if p.Style != StyleNone {
		t.Fatalf("style = %s, want %s", p.Style, StyleNone)
	}
}

func TestPageStyleFromQueryParameter(t *testing.T) {
	p := auto(t, "https://api.example.test/u?page=1&per_page=50", nil, `{"results": []}`)
	if p.Style != StylePage || p.PageParam != "page" {
		t.Fatalf("got style=%s param=%s", p.Style, p.PageParam)
	}
}

func TestPageStyleFromTotalPagesField(t *testing.T) {
	p := auto(t, "https://api.example.test/u", nil,
		`{"results": [], "total_pages": 6}`)
	if p.Style != StylePage || p.TotalPagesPtr != "/total_pages" {
		t.Fatalf("got style=%s totalPages=%s", p.Style, p.TotalPagesPtr)
	}
}

func TestOffsetStyleFromQueryParameters(t *testing.T) {
	// ?offset=&limit= and the ?skip=&page_size= dialect both count, and
	// the exact parameter names in use are carried into the plan.
	for _, c := range []struct{ url, offset, limit string }{
		{"https://api.example.test/u?offset=0&limit=25", "offset", "limit"},
		{"https://api.example.test/u?skip=0&page_size=25", "skip", "page_size"},
	} {
		p := auto(t, c.url, nil, `{"items": []}`)
		if p.Style != StyleOffset || p.OffsetParam != c.offset || p.LimitParam != c.limit {
			t.Fatalf("%s: got style=%s offset=%s limit=%s", c.url, p.Style, p.OffsetParam, p.LimitParam)
		}
	}
}

func TestOffsetStyleFromTotalField(t *testing.T) {
	p := auto(t, "https://api.example.test/u", nil,
		`{"items": [], "total": 57}`)
	if p.Style != StyleOffset || p.TotalPtr != "/total" {
		t.Fatalf("got style=%s total=%s", p.Style, p.TotalPtr)
	}
}

func TestPageMarkersBeatOffsetMarkers(t *testing.T) {
	// A body with both total_pages and total is page-number pagination;
	// advancing by offset against it would re-fetch overlapping windows.
	p := auto(t, "https://api.example.test/u", nil,
		`{"results": [], "total": 57, "total_pages": 6}`)
	if p.Style != StylePage {
		t.Fatalf("style = %s, want %s", p.Style, StylePage)
	}
}

func TestCursorBeatsPageQueryParameter(t *testing.T) {
	// A body cursor is more specific than a ?page= param that might just
	// be an unrelated leftover; cursor sits higher in the priority order.
	p := auto(t, "https://api.example.test/u?page=1", nil,
		`{"items": [], "next_cursor": "abc"}`)
	if p.Style != StyleCursor {
		t.Fatalf("style = %s, want %s", p.Style, StyleCursor)
	}
}

func TestNothingDetectedIsStyleNone(t *testing.T) {
	p := auto(t, "https://api.example.test/u", nil, `{"items": [{"id": 1}]}`)
	if p.Style != StyleNone {
		t.Fatalf("style = %s, want %s", p.Style, StyleNone)
	}
}

func TestForcedCursorWithoutFieldKeepsStyle(t *testing.T) {
	// Forcing a family must not error on a last-page-like first response;
	// the plan simply has no pointer and the stream ends after one page.
	p := Detect(reqURL(t, "https://api.example.test/u"), nil,
		doc(t, `{"items": []}`), StyleCursor, Overrides{})
	if p.Style != StyleCursor || p.NextPtr != "" {
		t.Fatalf("got style=%s ptr=%q", p.Style, p.NextPtr)
	}
}

func TestExplicitNextPointerOpaqueValueIsCursor(t *testing.T) {
	p := Detect(reqURL(t, "https://api.example.test/u"), nil,
		doc(t, `{"items": [], "meta": {"resume": "r-10"}}`),
		StyleAuto, Overrides{NextPtr: "/meta/resume"})
	if p.Style != StyleCursor || p.NextPtr != "/meta/resume" || p.CursorParam != "cursor" {
		t.Fatalf("got style=%s ptr=%s param=%s", p.Style, p.NextPtr, p.CursorParam)
	}
}

func TestExplicitNextPointerURLValueIsNextURL(t *testing.T) {
	p := Detect(reqURL(t, "https://api.example.test/u"), nil,
		doc(t, `{"items": [], "meta": {"more": "https://api.example.test/u?p=2"}}`),
		StyleAuto, Overrides{NextPtr: "/meta/more"})
	if p.Style != StyleNextURL || p.NextPtr != "/meta/more" {
		t.Fatalf("got style=%s ptr=%s", p.Style, p.NextPtr)
	}
}

func TestParseStyleNamesAndAlias(t *testing.T) {
	for in, want := range map[string]Style{
		"auto": StyleAuto, "link-header": StyleLink, "link": StyleLink,
		"next-url": StyleNextURL, "cursor": StyleCursor,
		"offset": StyleOffset, "page": StylePage, "none": StyleNone,
		"CURSOR": StyleCursor,
	} {
		got, err := ParseStyle(in)
		if err != nil || got != want {
			t.Fatalf("ParseStyle(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ParseStyle("token"); err == nil {
		t.Fatal("unknown style must be rejected")
	}
}
