// Tests for the RFC 8288 Link header parser, exercising the header shapes
// GitHub, GitLab, and assorted proxies actually emit — including the
// awkward ones (commas inside URLs, unquoted rel, rel lists).
package linkhdr

import (
	"net/http"
	"testing"
)

func header(values ...string) http.Header {
	h := http.Header{}
	for _, v := range values {
		h.Add("Link", v)
	}
	return h
}

func TestNextSingleLink(t *testing.T) {
	h := header(`<https://api.example.test/users?page=2>; rel="next"`)
	if got := Next(h); got != "https://api.example.test/users?page=2" {
		t.Fatalf("got %q", got)
	}
}

func TestNextAmongMultipleRelsGitHubStyle(t *testing.T) {
	h := header(`<https://api.example.test/u?page=1>; rel="prev", ` +
		`<https://api.example.test/u?page=3>; rel="next", ` +
		`<https://api.example.test/u?page=9>; rel="last"`)
	if got := Next(h); got != "https://api.example.test/u?page=3" {
		t.Fatalf("got %q", got)
	}
}

func TestNextAcrossMultipleHeaderLines(t *testing.T) {
	h := header(
		`<https://api.example.test/u?page=9>; rel="last"`,
		`<https://api.example.test/u?page=2>; rel="next"`,
	)
	if got := Next(h); got != "https://api.example.test/u?page=2" {
		t.Fatalf("got %q", got)
	}
}

func TestNextRelSyntaxVariants(t *testing.T) {
	// RFC 8288 rel matching is case-insensitive, allows unquoted tokens,
	// and allows space-separated relation-type lists.
	for _, value := range []string{
		`</p2>; rel="NEXT"`,
		`</p2>; rel=next`,
		`</p2>; rel="next last"`,
	} {
		if got := Next(header(value)); got != "/p2" {
			t.Fatalf("Next(%q) = %q, want /p2", value, got)
		}
	}
}

func TestNextAbsentWhenNoNextRel(t *testing.T) {
	h := header(`</p9>; rel="last", </p1>; rel="first"`)
	if got := Next(h); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := Next(http.Header{}); got != "" {
		t.Fatalf("empty header: got %q, want empty", got)
	}
}

func TestParseCommaInsideURLTarget(t *testing.T) {
	// Commas are legal in URLs; a naive comma-split parser breaks here.
	links := Parse(`<https://api.example.test/u?ids=1,2,3&page=2>; rel="next"`)
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	if links[0].URL != "https://api.example.test/u?ids=1,2,3&page=2" {
		t.Fatalf("got %q", links[0].URL)
	}
}

func TestParseQuotedParamWithCommaAndEscape(t *testing.T) {
	links := Parse(`</p2>; rel="next"; title="page two, the \"good\" one"`)
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	if got := links[0].Params["title"]; got != `page two, the "good" one` {
		t.Fatalf("title = %q", got)
	}
}

func TestParseSkipsMalformedSegmentAndRecovers(t *testing.T) {
	links := Parse(`garbage-without-brackets, </p2>; rel="next"`)
	if len(links) != 1 || links[0].URL != "/p2" {
		t.Fatalf("got %+v, want the valid link only", links)
	}
}

func TestParseUnterminatedTargetYieldsNothing(t *testing.T) {
	if links := Parse(`<https://api.example.test/broken`); len(links) != 0 {
		t.Fatalf("got %+v, want none", links)
	}
}

func TestParseFirstParamValueWins(t *testing.T) {
	// RFC 8288 §3: parsers must not fail on duplicates; first occurrence wins.
	links := Parse(`</p2>; rel="next"; rel="prev"`)
	if len(links) != 1 || links[0].Params["rel"] != "next" {
		t.Fatalf("got %+v", links)
	}
}
