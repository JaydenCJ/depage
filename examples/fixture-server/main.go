// Command fixture-server serves a small fake API on 127.0.0.1 that
// paginates the same 57-record dataset in five different styles — cursor,
// offset/limit, page number, Link header, and in-body next URL — plus a
// flaky variant that rate-limits the first request. It exists so you can
// try depage (and run scripts/smoke.sh) without touching any real API.
//
// Usage:
//
//	go run ./examples/fixture-server [--addr 127.0.0.1:8080]
//
// The first line printed to stdout is the base URL actually bound (use
// --addr 127.0.0.1:0 for an ephemeral port). Entirely offline: it binds
// loopback and serves in-memory data.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
)

// user is one record of the demo dataset.
type user struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Team string `json:"team"`
}

// dataset returns the fixed 57-user demo dataset. 57 is deliberately not a
// multiple of the default page size, so the last page is short.
func dataset() []user {
	teams := []string{"atlas", "borealis", "cascade", "dune"}
	users := make([]user, 57)
	for i := range users {
		users[i] = user{
			ID:   i + 1,
			Name: fmt.Sprintf("user-%02d", i+1),
			Team: teams[i%len(teams)],
		}
	}
	return users
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (host:0 picks an ephemeral port)")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fixture-server: %v", err)
	}
	fmt.Printf("http://%s\n", ln.Addr())
	os.Stdout.Sync()

	users := dataset()
	var flakyHits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/cursor/users", func(w http.ResponseWriter, r *http.Request) {
		serveCursor(w, r, users)
	})
	mux.HandleFunc("/offset/users", func(w http.ResponseWriter, r *http.Request) {
		serveOffset(w, r, users)
	})
	mux.HandleFunc("/page/users", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, users)
	})
	mux.HandleFunc("/link/users", func(w http.ResponseWriter, r *http.Request) {
		serveLink(w, r, users)
	})
	mux.HandleFunc("/nexturl/users", func(w http.ResponseWriter, r *http.Request) {
		serveNextURL(w, r, users)
	})
	mux.HandleFunc("/single/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, users) // a bare array, no pagination at all
	})
	mux.HandleFunc("/flaky/users", func(w http.ResponseWriter, r *http.Request) {
		// The very first request is rate-limited; every retry succeeds.
		if flakyHits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		serveCursor(w, r, users)
	})

	log.Fatal(http.Serve(ln, mux))
}

// slice returns users[start:start+limit] clamped to the dataset.
func slice(users []user, start, limit int) []user {
	if start < 0 {
		start = 0
	}
	if start > len(users) {
		start = len(users)
	}
	end := start + limit
	if end > len(users) {
		end = len(users)
	}
	return users[start:end]
}

// pageSize reads the given query parameter with a default of 10, capped at 20.
func pageSize(r *http.Request, param string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(param))
	if err != nil || n <= 0 {
		return 10
	}
	if n > 20 {
		return 20
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveCursor: {"data": [...], "next_cursor": "tok-N" | null}. The token
// encodes the next start position, opaquely enough to look real.
func serveCursor(w http.ResponseWriter, r *http.Request, users []user) {
	limit := pageSize(r, "limit")
	start := 0
	if tok := r.URL.Query().Get("cursor"); tok != "" {
		n, err := strconv.Atoi(trimPrefixOr(tok, "tok-", "not-a-number"))
		if err != nil {
			http.Error(w, `{"error":"bad cursor"}`, http.StatusBadRequest)
			return
		}
		start = n
	}
	page := slice(users, start, limit)
	body := map[string]any{"data": page, "next_cursor": nil}
	if start+len(page) < len(users) {
		body["next_cursor"] = fmt.Sprintf("tok-%d", start+len(page))
	}
	writeJSON(w, body)
}

// trimPrefixOr trims a required prefix, or returns the fallback so the
// downstream Atoi fails cleanly.
func trimPrefixOr(s, prefix, fallback string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return fallback
}

// serveOffset: {"total": 57, "items": [...]} driven by ?offset=&limit=.
func serveOffset(w http.ResponseWriter, r *http.Request, users []user) {
	limit := pageSize(r, "limit")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	writeJSON(w, map[string]any{
		"total": len(users),
		"items": slice(users, offset, limit),
	})
}

// servePage: {"page": N, "total_pages": M, "results": [...]} driven by
// ?page=&per_page=.
func servePage(w http.ResponseWriter, r *http.Request, users []user) {
	perPage := pageSize(r, "per_page")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	totalPages := (len(users) + perPage - 1) / perPage
	writeJSON(w, map[string]any{
		"page":        page,
		"total_pages": totalPages,
		"results":     slice(users, (page-1)*perPage, perPage),
	})
}

// serveLink: a bare JSON array with GitHub-style Link headers.
func serveLink(w http.ResponseWriter, r *http.Request, users []user) {
	perPage := pageSize(r, "per_page")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	totalPages := (len(users) + perPage - 1) / perPage
	if page < totalPages {
		next := fmt.Sprintf("http://%s/link/users?page=%d&per_page=%d", r.Host, page+1, perPage)
		last := fmt.Sprintf("http://%s/link/users?page=%d&per_page=%d", r.Host, totalPages, perPage)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, last))
	}
	writeJSON(w, slice(users, (page-1)*perPage, perPage))
}

// serveNextURL: {"records": [...], "links": {"next": "/relative/path"}} —
// note the *relative* next link, which depage resolves against the page URL.
func serveNextURL(w http.ResponseWriter, r *http.Request, users []user) {
	perPage := pageSize(r, "per_page")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	totalPages := (len(users) + perPage - 1) / perPage
	links := map[string]any{"next": nil}
	if page < totalPages {
		links["next"] = fmt.Sprintf("/nexturl/users?page=%d&per_page=%d", page+1, perPage)
	}
	writeJSON(w, map[string]any{
		"records": slice(users, (page-1)*perPage, perPage),
		"links":   links,
	})
}
