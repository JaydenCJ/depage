# depage examples

Everything here is offline and self-contained: the only server involved
binds 127.0.0.1 and serves in-memory data.

- **`fixture-server/`** — a small Go program that paginates the same
  57-record dataset in five different styles, so you can watch depage
  auto-detect each one:

  | Endpoint | Style it exercises |
  |---|---|
  | `/cursor/users?limit=10` | cursor (`next_cursor` in the body) |
  | `/offset/users?offset=0&limit=10` | offset/limit with a `total` field |
  | `/page/users?page=1&per_page=10` | page numbers with `total_pages` |
  | `/link/users?page=1` | RFC 8288 `Link` headers, GitHub style |
  | `/nexturl/users?page=1` | relative next URL in `links.next` |
  | `/single/users` | no pagination at all (`style=none`) |
  | `/flaky/users` | first request answers 429 + `Retry-After` |

- **`quickstart.sh`** — builds both binaries, starts the fixture server
  on an ephemeral port, and runs depage against every endpoint above,
  printing the detection summary for each. This is the README quickstart
  in runnable form.

```bash
bash examples/quickstart.sh

# or drive it by hand:
go run ./examples/fixture-server &        # prints its base URL first
go run ./cmd/depage 'http://127.0.0.1:8080/cursor/users?limit=10' | head
```

Both need nothing but a Go ≥1.22 toolchain and never touch the network.
