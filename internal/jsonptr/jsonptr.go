// Package jsonptr implements the subset of RFC 6901 (JSON Pointer) that
// depage needs to address fields inside decoded JSON documents
// (map[string]any / []any as produced by encoding/json).
//
// The empty pointer "" addresses the whole document. Array indices follow
// the RFC strictly: decimal digits only, no leading zeros, no "-".
package jsonptr

import (
	"fmt"
	"strings"
)

// Validate reports whether ptr is a syntactically valid JSON Pointer.
// It checks the shape only — whether the pointer resolves against a given
// document is Get's job.
func Validate(ptr string) error {
	if ptr == "" {
		return nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return fmt.Errorf("json pointer %q must start with '/' (or be empty for the whole document)", ptr)
	}
	for i := 0; i < len(ptr); i++ {
		if ptr[i] != '~' {
			continue
		}
		if i+1 >= len(ptr) || (ptr[i+1] != '0' && ptr[i+1] != '1') {
			return fmt.Errorf("json pointer %q has a dangling '~' escape (use ~0 for '~', ~1 for '/')", ptr)
		}
	}
	return nil
}

// Get resolves ptr against doc and returns the addressed value. The second
// return value is false when any token fails to resolve (missing key, bad
// or out-of-range array index, or a scalar where a container is needed).
func Get(doc any, ptr string) (any, bool) {
	if ptr == "" {
		return doc, true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	cur := doc
	for _, raw := range strings.Split(ptr[1:], "/") {
		token := unescape(raw)
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[token]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, ok := arrayIndex(token)
			if !ok || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			// Scalar reached before the pointer was exhausted.
			return nil, false
		}
	}
	return cur, true
}

// unescape applies the RFC 6901 escape order: ~1 first, then ~0.
func unescape(token string) string {
	if !strings.Contains(token, "~") {
		return token
	}
	token = strings.ReplaceAll(token, "~1", "/")
	return strings.ReplaceAll(token, "~0", "~")
}

// arrayIndex parses an RFC 6901 array index: digits only, no leading zeros
// (except "0" itself), and no "-" (the append position is meaningless for
// reads).
func arrayIndex(token string) (int, bool) {
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 { // defensively cap absurd indices
			return 0, false
		}
	}
	return n, true
}

// Basename returns the final reference token of ptr, unescaped — e.g.
// "/meta/next_cursor" -> "next_cursor". The empty pointer yields "".
func Basename(ptr string) string {
	if ptr == "" {
		return ""
	}
	return unescape(ptr[strings.LastIndex(ptr, "/")+1:])
}
