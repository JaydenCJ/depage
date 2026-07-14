// Package extract locates the array of records inside an arbitrary JSON
// page. Real APIs wrap their items ten different ways — {"data": [...]},
// {"results": [...]}, {"data": {"items": [...]}}, GraphQL edges/node,
// Elasticsearch hits.hits — and extract finds them without configuration,
// falling back to an explicit --items JSON pointer for the exotic cases.
package extract

import (
	"fmt"
	"strings"

	"github.com/JaydenCJ/depage/internal/jsonptr"
)

// priorityNames is the ordered list of wrapper keys probed at each level.
// Order encodes how unambiguous a name is; scanning is deterministic
// regardless of map iteration order because this list drives it.
var priorityNames = []string{
	"data", "items", "results", "records", "entries", "rows",
	"values", "value", "hits", "edges", "nodes", "documents", "content",
	"elements", "objects", "list", "_embedded", "embedded",
	"result", "response", "payload",
}

// maxDepth bounds the recursive descent through wrapper objects. Three
// levels covers every observed real-world shape (e.g. GraphQL's
// data -> connection -> edges) without wandering into record bodies.
const maxDepth = 3

// Items returns the records of one page. When ptr is non-empty it is used
// verbatim (and must address an array); otherwise the page is searched:
//
//  1. a page that *is* an array is its own item list;
//  2. wrapper keys are probed in priority order at each level, descending
//     into objects up to three levels deep;
//  3. an object with exactly one key descends through it (GraphQL roots);
//  4. an "edges" array of {"node": ...} wrappers is unwrapped to the nodes.
//
// The second return value reports where the items were found (a JSON
// pointer, "" for a top-level array) for logging.
func Items(doc any, ptr string) ([]any, string, error) {
	if ptr != "" {
		val, ok := jsonptr.Get(doc, ptr)
		if !ok {
			return nil, "", fmt.Errorf("--items pointer %s does not resolve in the response body", ptr)
		}
		arr, ok := val.([]any)
		if !ok {
			return nil, "", fmt.Errorf("--items pointer %s addresses a %s, not an array", ptr, typeName(val))
		}
		return unwrapEdges(arr, ptr), ptr, nil
	}
	if arr, ok := doc.([]any); ok {
		return arr, "", nil
	}
	arr, at, ok := find(doc, "", 0)
	if !ok {
		return nil, "", fmt.Errorf(
			"could not locate an items array in the response (tried %s, …); pass --items with a JSON pointer",
			strings.Join(priorityNames[:6], ", "))
	}
	return unwrapEdges(arr, at), at, nil
}

// find performs the deterministic search described on Items.
func find(doc any, at string, depth int) ([]any, string, bool) {
	obj, ok := doc.(map[string]any)
	if !ok || depth >= maxDepth {
		return nil, "", false
	}
	// Pass 1: a priority key holding an array wins immediately.
	for _, name := range priorityNames {
		if arr, ok := obj[name].([]any); ok {
			return arr, at + "/" + escape(name), true
		}
	}
	// Pass 2: descend into priority keys holding objects.
	for _, name := range priorityNames {
		if inner, ok := obj[name].(map[string]any); ok {
			if arr, ptr, ok := find(inner, at+"/"+escape(name), depth+1); ok {
				return arr, ptr, true
			}
		}
	}
	// Pass 3: a single-key wrapper object descends through its only key,
	// whatever it is called ({"data": {...}}, {"viewer": {...}}, …).
	if len(obj) == 1 {
		for key, inner := range obj {
			if arr, ok := inner.([]any); ok {
				return arr, at + "/" + escape(key), true
			}
			if arr, ptr, ok := find(inner, at+"/"+escape(key), depth+1); ok {
				return arr, ptr, true
			}
		}
	}
	// Pass 4: a single unambiguous array of records under a non-standard
	// key (Slack's "members", GitHub's "workflow_runs", …). Taken only when
	// exactly one non-empty array-of-objects exists at this level — two
	// candidates would turn the pick into a guess, so those still error
	// with the --items hint. Counting is order-independent, so this stays
	// deterministic despite ranging over the map.
	var candArr []any
	candKey, candidates := "", 0
	for key, v := range obj {
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 || !allObjects(arr) {
			continue
		}
		candidates++
		candKey, candArr = key, arr
	}
	if candidates == 1 {
		return candArr, at + "/" + escape(candKey), true
	}
	return nil, "", false
}

// allObjects reports whether every element of arr is a JSON object —
// the shape a record list has, as opposed to a tag list or ID list.
func allObjects(arr []any) bool {
	for _, el := range arr {
		if _, ok := el.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// unwrapEdges converts a GraphQL-style edges array to its nodes when — and
// only when — every element is an object carrying a "node" key. Mixed or
// plain arrays pass through untouched.
func unwrapEdges(arr []any, at string) []any {
	if jsonptr.Basename(at) != "edges" || len(arr) == 0 {
		return arr
	}
	nodes := make([]any, len(arr))
	for i, el := range arr {
		edge, ok := el.(map[string]any)
		if !ok {
			return arr
		}
		node, ok := edge["node"]
		if !ok {
			return arr
		}
		nodes[i] = node
	}
	return nodes
}

// escape applies RFC 6901 token escaping so reported pointers stay valid.
func escape(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// typeName names a decoded JSON value for error messages.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "number"
	}
}
