// Tests for items-array extraction: the wrapper shapes real APIs use,
// the deterministic priority order, the depth limit, and the GraphQL
// edges/node unwrap.
package extract

import (
	"encoding/json"
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

func mustItems(t *testing.T, body, ptr string) ([]any, string) {
	t.Helper()
	items, at, err := Items(doc(t, body), ptr)
	if err != nil {
		t.Fatalf("Items failed: %v", err)
	}
	return items, at
}

func TestTopLevelArrayIsItsOwnItemList(t *testing.T) {
	items, at := mustItems(t, `[{"id": 1}, {"id": 2}]`, "")
	if len(items) != 2 || at != "" {
		t.Fatalf("got %d items at %q", len(items), at)
	}
}

func TestCommonWrapperKeys(t *testing.T) {
	cases := map[string]string{
		`{"data": [{"id": 1}], "next_cursor": null}`:            "/data",
		`{"page": 1, "results": [{"id": 1}]}`:                   "/results",
		`{"records": [{"id": 1}], "links": {}}`:                 "/records",
		`{"@odata.context": "$metadata", "value": [{"id": 1}]}`: "/value", // OData
	}
	for body, want := range cases {
		if _, at := mustItems(t, body, ""); at != want {
			t.Fatalf("body %s: found at %q, want %s", body, at, want)
		}
	}
}

func TestPriorityOrderDataBeatsItems(t *testing.T) {
	// When two candidate arrays exist the priority list decides — always
	// the same one, never map-iteration luck.
	_, at := mustItems(t, `{"items": [1], "data": [2]}`, "")
	if at != "/data" {
		t.Fatalf("found at %q, want /data (priority order)", at)
	}
}

func TestNestedDataItems(t *testing.T) {
	_, at := mustItems(t, `{"data": {"items": [{"id": 1}], "count": 1}}`, "")
	if at != "/data/items" {
		t.Fatalf("found at %q, want /data/items", at)
	}
}

func TestElasticsearchHitsHits(t *testing.T) {
	_, at := mustItems(t, `{"took": 3, "hits": {"total": 2, "hits": [{"_id": "a"}]}}`, "")
	if at != "/hits/hits" {
		t.Fatalf("found at %q, want /hits/hits", at)
	}
}

func TestHALEmbeddedWithCustomRelName(t *testing.T) {
	// _embedded holds a single rel whose name we cannot know up front;
	// the single-key descent finds it anyway.
	_, at := mustItems(t, `{"_embedded": {"orders": [{"id": 1}]}, "_links": {}}`, "")
	if at != "/_embedded/orders" {
		t.Fatalf("found at %q, want /_embedded/orders", at)
	}
}

func TestGraphQLEdgesUnwrapToNodes(t *testing.T) {
	body := `{"data": {"search": {"edges": [
		{"cursor": "c1", "node": {"id": 1}},
		{"cursor": "c2", "node": {"id": 2}}
	]}}}`
	items, at := mustItems(t, body, "")
	if at != "/data/search/edges" {
		t.Fatalf("found at %q", at)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	first, ok := items[0].(map[string]any)
	if !ok || first["id"].(json.Number).String() != "1" {
		t.Fatalf("edges were not unwrapped to nodes: %v", items[0])
	}
}

func TestEdgesWithoutNodeKeysPassThrough(t *testing.T) {
	// An "edges" array whose elements are plain records must be emitted
	// as-is; unwrapping would drop user data.
	items, _ := mustItems(t, `{"edges": [{"from": 1, "to": 2}]}`, "")
	el := items[0].(map[string]any)
	if _, ok := el["from"]; !ok {
		t.Fatalf("plain edges array was mangled: %v", items[0])
	}
}

func TestExplicitPointerWins(t *testing.T) {
	// --items must beat auto-detection even when a priority name exists.
	items, at := mustItems(t, `{"data": [1], "audit": [1, 2, 3]}`, "/audit")
	if at != "/audit" || len(items) != 3 {
		t.Fatalf("got %d items at %q", len(items), at)
	}
}

func TestExplicitPointerErrors(t *testing.T) {
	_, _, err := Items(doc(t, `{"data": {"a": 1}}`), "/data")
	if err == nil || !strings.Contains(err.Error(), "object, not an array") {
		t.Fatalf("err = %v, want a type complaint", err)
	}
	_, _, err = Items(doc(t, `{"data": []}`), "/nope")
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("err = %v, want a resolution error", err)
	}
}

func TestNoArrayAnywhereErrorsWithHint(t *testing.T) {
	_, _, err := Items(doc(t, `{"id": 7, "status": "ok"}`), "")
	if err == nil || !strings.Contains(err.Error(), "--items") {
		t.Fatalf("err = %v, want a hint to pass --items", err)
	}
}

func TestDepthLimitStopsRunawayDescent(t *testing.T) {
	// The array sits four wrapper levels deep — past maxDepth. Refusing to
	// find it (rather than scanning arbitrarily deep into record bodies)
	// keeps detection predictable; --items still reaches it.
	body := `{"data": {"result": {"response": {"payload": {"items": [1]}}}}}`
	if _, _, err := Items(doc(t, body), ""); err == nil {
		t.Fatal("expected the depth limit to stop the search")
	}
	items, _, err := Items(doc(t, body), "/data/result/response/payload/items")
	if err != nil || len(items) != 1 {
		t.Fatalf("explicit pointer should still work: %v", err)
	}
}

func TestEmptyItemsArrayIsValid(t *testing.T) {
	items, at := mustItems(t, `{"data": [], "next_cursor": null}`, "")
	if items == nil || len(items) != 0 || at != "/data" {
		t.Fatalf("got %v at %q, want an empty (non-nil) slice", items, at)
	}
}

func TestSingleKeyDescentThroughUnknownName(t *testing.T) {
	// {"viewer": {...}} is no priority name, but as the only key it is the
	// obvious wrapper to look through.
	_, at := mustItems(t, `{"viewer": {"repositories": {"nodes": [{"id": 1}]}}}`, "")
	if at != "/viewer/repositories/nodes" {
		t.Fatalf("found at %q", at)
	}
	// A single unambiguous array-of-objects under a non-standard key —
	// Slack's "members" — is the record list even among sibling scalars
	// and objects.
	_, at = mustItems(t,
		`{"ok": true, "members": [{"id": "U1"}], "response_metadata": {"next_cursor": ""}}`, "")
	if at != "/members" {
		t.Fatalf("slack members: found at %q, want /members", at)
	}
	// Two candidate arrays make the pick a guess; depage must refuse and
	// ask for --items instead of silently choosing one.
	_, _, err := Items(doc(t, `{"users": [{"a": 1}], "groups": [{"b": 2}]}`), "")
	if err == nil || !strings.Contains(err.Error(), "--items") {
		t.Fatalf("ambiguous arrays: err = %v, want a --items hint", err)
	}
	// An array of scalars ("tags") is not a record list; it must not be
	// chosen by the fallback.
	_, _, err = Items(doc(t, `{"tags": ["a", "b"], "count": 2}`), "")
	if err == nil {
		t.Fatal("scalar array must not be picked as the item list")
	}
}
