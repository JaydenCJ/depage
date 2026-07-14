# How depage detects pagination

depage inspects exactly three things about the **first** response — the
request URL, the response headers, and the decoded JSON body — and locks
in a *plan*. The plan is then applied mechanically to every following
page. This document is the reference for what is probed, in which order,
and when the stream is considered finished. It is a contract: changing
any list here requires a matching test.

## Probe order

The first family that matches wins:

| # | Family | Signal |
|---|---|---|
| 1 | `link-header` | a `Link` header with `rel="next"` (RFC 8288) |
| 2 | `next-url` | a body field holding a URL (absolute or a `/path`) |
| 3 | `cursor` | a body field holding an opaque scalar token |
| 4 | `page` | a `?page=`-style query parameter on the request URL |
| 5 | `offset` | an `?offset=` / `?skip=` query parameter |
| 6 | `page` | a total-page-count field in the body |
| 7 | `offset` | a total-item-count field in the body |
| 8 | `none` | nothing matched: the response is a single page |

Headers beat body fields because APIs that send both (GitHub, GitLab)
treat the header as authoritative. Cursors beat `?page=` because a body
cursor is deliberate, while a `page` parameter might be a leftover.
`--style` forces a family; `--next` pins the exact field (a URL-ish value
makes it `next-url`, anything else `cursor`).

## Candidate locations

All lookups are JSON pointers, checked in the order listed.

**Next-URL fields** (value must be a string starting with `http://`,
`https://`, or `/`):

```text
/links/next   /links/next/href   /_links/next/href
/paging/next  /pagination/next   /pagination/next_url  /pagination/next_link
/meta/next    /next_url          /next_page_url        /nextUrl
/nextLink     /@odata.nextLink   /next                 /next_page
```

**Cursor fields** (value must be a non-empty non-URL string, or a number):

```text
/next_cursor        /nextCursor          /meta/next_cursor   /meta/cursor
/pagination/next_cursor                  /pagination/cursor
/response_metadata/next_cursor
/paging/cursors/after                    /paging/next_cursor
/next_page_token    /nextPageToken       /nextToken
/continuation_token /continuation        /cursor
/next               /next_page
```

**Total-page-count fields** (make the family `page`):

```text
/total_pages  /totalPages  /meta/total_pages  /pagination/total_pages
/page_count   /pageCount
```

**Total-item-count fields** (make the family `offset`):

```text
/total  /total_count  /totalCount  /meta/total  /meta/total_count  /pagination/total
```

**Query parameters** recognized on the request URL: page numbers via
`page`, `page_number`, `pageNumber`, `page_no`; offsets via `offset`,
`skip`; page sizes via `limit`, `per_page`, `perPage`, `page_size`,
`pageSize`.

## The cursor query parameter

When the next page is requested, the cursor value has to be sent back
under some parameter name. depage picks, in order:

1. `--cursor-param`, when given;
2. a cursor-like parameter already present in the request URL (`cursor`,
   `page_token`, `pageToken`, `after`, `continuation`,
   `continuation_token`, `next_token`, `nextToken`, `next_cursor`);
3. a mapping from the field name the cursor was found under
   (`next_cursor` → `cursor`, `nextPageToken` → `pageToken`,
   `next_page_token` → `page_token`, `…cursors/after` → `after`, …);
4. `cursor` as the final fallback.

## Termination rules

| Family | The stream ends when… |
|---|---|
| `link-header` | no `rel="next"` link is present |
| `next-url` | the field is absent, `null`, or `""` |
| `cursor` | the field is absent, `null`, `""` — or repeats the previous value |
| `offset` | the page is empty; the known total is reached; or the page is shorter than the limit |
| `page` | the known total-page count is reached; or (without one) the page is empty or shorter than the first page |
| `none` | after the first page |

Two guards apply to every family: a **visited-URL set** stops the walk
with a warning if a next link points at an already-fetched URL, and
`--max-pages` / `--max-items` cap the walk unconditionally (`--max-items`
truncates the final page mid-way).

## Items extraction

Independent of the pagination family, each page's records are located by:

1. the page itself, when the body is a JSON array;
2. wrapper keys probed in a fixed priority order at each level (`data`,
   `items`, `results`, `records`, `entries`, `rows`, `values`, `value`,
   `hits`, `edges`, `nodes`, `documents`, `content`, `elements`,
   `objects`, `list`, `_embedded`, `embedded`, `result`, `response`,
   `payload`), descending into wrapper objects at most three levels
   (so `data.items`, `hits.hits`, `_embedded.orders` all resolve);
3. single-key objects, which are looked through whatever the key is
   called (GraphQL's `{"data": {"search": {...}}}`);
4. a GraphQL `edges` array of `{"node": …}` wrappers, which is unwrapped
   to the nodes;
5. as a last resort, the single array-of-objects in the page, whatever
   its key is called (Slack's `members`) — but only when exactly one
   such array exists; two candidates would make the pick a guess.

When nothing is found, depage refuses to guess and asks for
`--items <json-pointer>`. The same pointer syntax (RFC 6901) works for
`--next`.
