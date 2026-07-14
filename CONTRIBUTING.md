# Contributing to depage

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — the tool and its tests are pure
standard library, and the test suite never leaves 127.0.0.1.

```bash
git clone https://github.com/JaydenCJ/depage && cd depage
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the CLI and the bundled fixture server, starts
the server on an ephemeral loopback port, and flattens the same dataset
through every pagination style, asserting on real NDJSON output and exit
codes; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (92 deterministic tests, no network).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (detection, extraction, pointer, and Link-header parsing never
   touch the network — only the pager fetches, and tests drive it against
   in-process loopback servers).

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in
  the PR.
- No network calls except the requests the user asked for, and no
  telemetry — depage talks to exactly one host: the API in the URL.
- Detection is a contract: a new candidate field or termination rule
  needs a row in `docs/detection.md` and a test that exercises exactly
  that shape.
- Never sleep in tests; the pager takes an injected sleep function for
  a reason.
- Code comments and doc comments are written in English.
- Determinism first: identical responses must produce byte-identical
  NDJSON, including record order.

## Reporting bugs

Include the output of `depage --version`, the full command you ran with
`-v` (redact your tokens), and — most importantly — one page of the API's
JSON response with the pagination fields intact. Detection sees nothing
but that body, the headers, and the URL, so a single trimmed page is
usually a complete repro.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
