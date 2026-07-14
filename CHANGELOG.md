# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Pagination auto-detection from the first response: RFC 8288 `Link`
  headers (`rel="next"`, including rel lists, unquoted tokens, and commas
  inside URLs), in-body next URLs (`links.next`, HAL `_links.next.href`,
  `@odata.nextLink`, relative paths resolved against the page URL),
  cursors (`next_cursor`, `nextPageToken`, `paging.cursors.after`, and a
  dozen more, with the cursor query parameter derived from the field name
  or reused from the request URL), page numbers (`?page=` /
  `total_pages`), and offset/limit (`?offset=` / `?skip=` / `total`) —
  with a documented priority order in `docs/detection.md`.
- Items-array extraction without configuration: top-level arrays, common
  wrapper keys (`data`, `items`, `results`, `hits.hits`, …) probed in a
  deterministic priority order up to three levels deep, single-key
  wrapper descent, HAL `_embedded`, and GraphQL `edges` → `node`
  unwrapping; `--items <json-pointer>` for the exotic cases.
- Streaming NDJSON emission (one record per line, flushed page by page)
  plus `--pages` for one raw page envelope per line.
- Termination and safety rules: cursor repeat guard, visited-URL loop
  guard with a warning, short-page and empty-page heuristics, totals
  honored when present, `--max-pages` / `--max-items` caps.
- Retry with exponential backoff on 429/5xx/network errors, honoring
  `Retry-After` (capped at 30s), never retrying other 4xx; `--delay` for
  politeness between pages; per-request `--timeout`.
- CLI ergonomics: repeatable `-H 'Name: value'` headers, `--style` to
  force a family, `--next` / `--cursor-param` / `--page-param` /
  `--offset-param` / `--limit-param` overrides, `-v` detection trace,
  `-q` quiet mode, and documented exit codes (0/1/2).
- A bundled offline fixture server (`examples/fixture-server`) that
  paginates one dataset in five styles on 127.0.0.1, used by the
  quickstart, the examples, and the smoke script.
- 92 deterministic offline tests (unit + in-process CLI runs against
  loopback servers with injected sleeps) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/depage/releases/tag/v0.1.0
