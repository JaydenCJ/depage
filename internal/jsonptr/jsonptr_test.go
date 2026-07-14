// Tests for the RFC 6901 JSON Pointer subset. The escape-order and
// array-index rules are where hand-rolled pointer code usually goes wrong,
// so they get their own cases.
package jsonptr

import (
	"encoding/json"
	"strings"
	"testing"
)

// doc decodes a JSON literal the same way the pager does (UseNumber).
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

func TestGetEmptyPointerReturnsWholeDocument(t *testing.T) {
	d := doc(t, `{"a": 1}`)
	got, ok := Get(d, "")
	if !ok {
		t.Fatal("empty pointer should resolve")
	}
	if _, isMap := got.(map[string]any); !isMap {
		t.Fatalf("got %T, want the document itself", got)
	}
}

func TestGetNestedObjectField(t *testing.T) {
	d := doc(t, `{"meta": {"next_cursor": "abc"}}`)
	got, ok := Get(d, "/meta/next_cursor")
	if !ok || got != "abc" {
		t.Fatalf("got %v (ok=%v), want abc", got, ok)
	}
}

func TestGetArrayElementByIndex(t *testing.T) {
	d := doc(t, `{"items": ["a", "b", "c"]}`)
	got, ok := Get(d, "/items/2")
	if !ok || got != "c" {
		t.Fatalf("got %v (ok=%v), want c", got, ok)
	}
}

func TestGetFailsOnUnresolvableTokens(t *testing.T) {
	d := doc(t, `{"a": "scalar", "arr": ["only"]}`)
	for _, ptr := range []string{
		"/b",      // missing key
		"/arr/1",  // index out of range
		"/a/b",    // descends through a scalar
		"/arr/-",  // append position is meaningless for reads
		"/arr/x",  // non-numeric index
		"/arr/ 1", // whitespace is not a digit
	} {
		if _, ok := Get(d, ptr); ok {
			t.Fatalf("pointer %q should not resolve", ptr)
		}
	}
}

func TestGetArrayIndexRejectsLeadingZeros(t *testing.T) {
	// RFC 6901: "01" is not a valid array index; treating it as 1 would
	// silently address the wrong element.
	d := doc(t, `["a", "b"]`)
	if _, ok := Get(d, "/01"); ok {
		t.Fatal("leading-zero index should be rejected")
	}
	if got, ok := Get(d, "/0"); !ok || got != "a" {
		t.Fatalf("plain 0 must still work, got %v (ok=%v)", got, ok)
	}
}

func TestGetTildeEscapesInCorrectOrder(t *testing.T) {
	// "~01" must decode to "~1" (unescape ~1 first, then ~0), not to "/1".
	d := doc(t, `{"a/b": 1, "m~n": 2, "~1": 3}`)
	cases := map[string]string{
		"/a~1b": "1",
		"/m~0n": "2",
		"/~01":  "3",
	}
	for ptr, want := range cases {
		got, ok := Get(d, ptr)
		if !ok {
			t.Fatalf("pointer %q did not resolve", ptr)
		}
		if got.(json.Number).String() != want {
			t.Fatalf("pointer %q: got %v, want %s", ptr, got, want)
		}
	}
}

func TestGetEmptyTokenAddressesEmptyKey(t *testing.T) {
	d := doc(t, `{"": "empty-key"}`)
	got, ok := Get(d, "/")
	if !ok || got != "empty-key" {
		t.Fatalf(`pointer "/" should address the "" key, got %v (ok=%v)`, got, ok)
	}
}

func TestValidateAcceptsWellFormedPointers(t *testing.T) {
	for _, ptr := range []string{"", "/", "/a", "/a/b/0", "/a~0b/c~1d"} {
		if err := Validate(ptr); err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", ptr, err)
		}
	}
}

func TestValidateRejectsMalformedPointers(t *testing.T) {
	for _, ptr := range []string{"a/b", "/a~", "/a~2b", "/~x"} {
		if err := Validate(ptr); err == nil {
			t.Fatalf("Validate(%q) should fail", ptr)
		}
	}
}

func TestBasenameReturnsFinalUnescapedToken(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"/next_cursor":   "next_cursor",
		"/meta/cursor":   "cursor",
		"/a/b~1c":        "b/c",
		"/paging/~0next": "~next",
	}
	for ptr, want := range cases {
		if got := Basename(ptr); got != want {
			t.Fatalf("Basename(%q) = %q, want %q", ptr, got, want)
		}
	}
}
