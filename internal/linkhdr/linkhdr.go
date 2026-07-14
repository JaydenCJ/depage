// Package linkhdr parses RFC 8288 Link headers well enough to follow
// rel="next" pagination (GitHub, GitLab, Datadog, and most REST APIs that
// paginate in headers). It tolerates the quirks seen in the wild: multiple
// Link headers, several links per header, unquoted rel values, space-
// separated rel lists, and commas inside <URI-Reference>s or quoted params.
package linkhdr

import (
	"net/http"
	"strings"
)

// Link is one parsed link-value: the target plus its lowercased parameters.
type Link struct {
	URL    string
	Params map[string]string
}

// Next returns the target of the first link carrying rel="next" across all
// Link header lines, or "" when there is none.
func Next(h http.Header) string {
	for _, value := range h.Values("Link") {
		for _, l := range Parse(value) {
			if hasRel(l, "next") {
				return l.URL
			}
		}
	}
	return ""
}

// hasRel reports whether the link's rel parameter contains want as one of
// its space-separated, case-insensitive members (RFC 8288 §3.3).
func hasRel(l Link, want string) bool {
	for _, rel := range strings.Fields(l.Params["rel"]) {
		if strings.EqualFold(rel, want) {
			return true
		}
	}
	return false
}

// Parse parses a single Link header value, which may carry several
// comma-separated link-values. Malformed segments are skipped rather than
// aborting the whole header: pagination should survive a sloppy proxy.
func Parse(value string) []Link {
	var links []Link
	pos := 0
	for pos < len(value) {
		// Skip whitespace and stray separators between link-values.
		for pos < len(value) && (value[pos] == ' ' || value[pos] == '\t' || value[pos] == ',') {
			pos++
		}
		if pos >= len(value) {
			break
		}
		if value[pos] != '<' {
			// Not a link-value; resynchronize at the next comma.
			next := strings.IndexByte(value[pos:], ',')
			if next < 0 {
				break
			}
			pos += next + 1
			continue
		}
		end := strings.IndexByte(value[pos:], '>')
		if end < 0 {
			break // unterminated <...>; nothing more to salvage
		}
		link := Link{URL: value[pos+1 : pos+end], Params: map[string]string{}}
		pos += end + 1
		pos = parseParams(value, pos, link.Params)
		if link.URL != "" {
			links = append(links, link)
		}
	}
	return links
}

// parseParams consumes ";"-separated parameters until the next top-level
// "," or end of input, filling params (keys lowercased, first value wins).
// It returns the position just past the link-value.
func parseParams(value string, pos int, params map[string]string) int {
	for pos < len(value) {
		for pos < len(value) && (value[pos] == ' ' || value[pos] == '\t') {
			pos++
		}
		if pos >= len(value) || value[pos] == ',' {
			return pos
		}
		if value[pos] != ';' {
			pos++ // garbage; skip a byte and retry
			continue
		}
		pos++ // consume ';'
		for pos < len(value) && (value[pos] == ' ' || value[pos] == '\t') {
			pos++
		}
		start := pos
		for pos < len(value) && value[pos] != '=' && value[pos] != ';' && value[pos] != ',' {
			pos++
		}
		key := strings.ToLower(strings.TrimSpace(value[start:pos]))
		val := ""
		if pos < len(value) && value[pos] == '=' {
			pos++
			val, pos = parseParamValue(value, pos)
		}
		if key != "" {
			if _, seen := params[key]; !seen {
				params[key] = val
			}
		}
	}
	return pos
}

// parseParamValue reads either a quoted-string (backslash escapes honored,
// commas allowed inside) or a bare token ending at ';' or ','.
func parseParamValue(value string, pos int) (string, int) {
	if pos < len(value) && value[pos] == '"' {
		pos++
		var b strings.Builder
		for pos < len(value) {
			c := value[pos]
			if c == '\\' && pos+1 < len(value) {
				b.WriteByte(value[pos+1])
				pos += 2
				continue
			}
			if c == '"' {
				pos++
				break
			}
			b.WriteByte(c)
			pos++
		}
		return b.String(), pos
	}
	start := pos
	for pos < len(value) && value[pos] != ';' && value[pos] != ',' {
		pos++
	}
	return strings.TrimSpace(value[start:pos]), pos
}
