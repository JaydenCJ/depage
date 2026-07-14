// Package cli implements the depage command line: flag parsing, NDJSON
// emission, and the stderr summary. Run is a pure function of its
// arguments and writers so tests drive it in-process.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JaydenCJ/depage/internal/detect"
	"github.com/JaydenCJ/depage/internal/jsonptr"
	"github.com/JaydenCJ/depage/internal/pager"
	"github.com/JaydenCJ/depage/internal/version"
)

// Exit codes: 0 success, 1 runtime/HTTP failure, 2 usage error.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `depage — flatten any paginated JSON API into one NDJSON stream.

Usage:
  depage [flags] <url>

The pagination style (cursor, offset, page number, Link header, or in-body
next URL) is auto-detected from the first response; every record of every
page is printed as one JSON line on stdout.

Flags:
  -H, --header <'Name: value'>   add a request header (repeatable)
      --style <name>             force a style: auto, link-header, next-url,
                                 cursor, offset, page, none   (default auto)
      --items <pointer>          JSON pointer to the items array
      --next <pointer>           JSON pointer to the next cursor / next URL
      --cursor-param <name>      query parameter that carries the cursor
      --page-param <name>        query parameter for page numbers
      --offset-param <name>      query parameter for offsets
      --limit-param <name>       query parameter naming the page size
      --max-pages <n>            stop after n pages          (0 = unlimited)
      --max-items <n>            stop after n items          (0 = unlimited)
      --pages                    emit one raw page object per line, not items
      --retries <n>              retries on 429/5xx/network errors (default 2)
      --retry-wait <dur>         base retry backoff, doubled per attempt
                                 (default 500ms; Retry-After wins when sent)
      --delay <dur>              politeness delay between page fetches
      --timeout <dur>            per-request timeout          (default 30s)
  -q, --quiet                    suppress the stderr summary
  -v, --verbose                  per-page progress on stderr
      --version                  print the version and exit
  -h, --help                     print this help and exit

Examples:
  depage 'https://api.example.test/v1/users?limit=100' > users.ndjson
  depage -H 'Authorization: Bearer TOKEN' 'https://api.example.test/v1/events'
  depage --items /data/rows --next /meta/cursor 'http://127.0.0.1:8080/report'
`

// config is the parsed command line.
type config struct {
	url       string
	headers   http.Header
	style     detect.Style
	itemsPtr  string
	overrides detect.Overrides
	maxPages  int
	maxItems  int
	pagesMode bool
	retries   int
	retryWait time.Duration
	delay     time.Duration
	timeout   time.Duration
	quiet     bool
	verbose   bool
}

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, code, done := parseArgs(args, stdout, stderr)
	if done {
		return code
	}

	out := bufio.NewWriter(stdout)
	defer out.Flush()

	opts := pager.Options{
		Style:        cfg.style,
		ItemsPtr:     cfg.itemsPtr,
		Overrides:    cfg.overrides,
		MaxPages:     cfg.maxPages,
		MaxItems:     cfg.maxItems,
		Retries:      cfg.retries,
		RetryWait:    cfg.retryWait,
		Delay:        cfg.delay,
		RequireItems: !cfg.pagesMode,
		Header:       cfg.headers,
		Client:       &http.Client{Timeout: cfg.timeout},
		Warnf: func(format string, a ...any) {
			fmt.Fprintf(stderr, "depage: warning: "+format+"\n", a...)
		},
	}
	if cfg.verbose {
		opts.Logf = func(format string, a ...any) {
			fmt.Fprintf(stderr, "depage: "+format+"\n", a...)
		}
	}

	var emitErr error
	emit := func(p *pager.Page) error {
		if cfg.pagesMode {
			emitErr = writeLine(out, p.Doc)
		} else {
			for _, item := range p.Items {
				if emitErr = writeLine(out, item); emitErr != nil {
					break
				}
			}
		}
		if emitErr == nil {
			emitErr = out.Flush() // stream page by page; surface broken pipes early
		}
		return emitErr
	}

	sum, err := pager.Run(context.Background(), cfg.url, opts, emit)
	if flushErr := out.Flush(); err == nil {
		err = flushErr
	}
	if err != nil {
		fmt.Fprintf(stderr, "depage: error: %v\n", err)
		return exitError
	}
	if !cfg.quiet {
		unit := "items"
		if cfg.pagesMode {
			unit = "pages emitted"
		}
		count := sum.Items
		if cfg.pagesMode {
			count = sum.Pages
		}
		fmt.Fprintf(stderr, "depage: style=%s (via %s), pages=%d, %s=%d\n",
			sum.Style, sum.Via, sum.Pages, unit, count)
	}
	return exitOK
}

// writeLine emits one compact JSON value terminated by \n.
func writeLine(out *bufio.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding record: %v", err)
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	return out.WriteByte('\n')
}

// parseArgs parses the command line. done=true means Run should return
// code immediately (help, version, or a usage error already reported).
func parseArgs(args []string, stdout, stderr io.Writer) (*config, int, bool) {
	cfg := &config{
		headers:   http.Header{},
		style:     detect.StyleAuto,
		retries:   2,
		retryWait: 500 * time.Millisecond,
		timeout:   30 * time.Second,
	}
	usageErr := func(format string, a ...any) (*config, int, bool) {
		fmt.Fprintf(stderr, "depage: "+format+"\n", a...)
		fmt.Fprintln(stderr, "run 'depage --help' for usage")
		return nil, exitUsage, true
	}

	i := 0
	next := func() (string, bool) {
		if i+1 >= len(args) {
			return "", false
		}
		i++
		return args[i], true
	}
	for ; i < len(args); i++ {
		arg := args[i]
		flag, inline, hasInline := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if cfg.url != "" {
				return usageErr("unexpected extra argument %q (exactly one URL expected)", arg)
			}
			cfg.url = arg
			continue
		}
		value := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			return next()
		}
		switch flag {
		case "-h", "--help":
			fmt.Fprint(stdout, usage)
			return nil, exitOK, true
		case "--version":
			fmt.Fprintf(stdout, "depage %s\n", version.Version)
			return nil, exitOK, true
		case "-q", "--quiet":
			cfg.quiet = true
		case "-v", "--verbose":
			cfg.verbose = true
		case "--pages":
			cfg.pagesMode = true
		case "-H", "--header":
			v, ok := value()
			if !ok {
				return usageErr("%s needs a 'Name: value' argument", flag)
			}
			name, val, found := strings.Cut(v, ":")
			name = strings.TrimSpace(name)
			if !found || name == "" {
				return usageErr("malformed header %q (want 'Name: value')", v)
			}
			cfg.headers.Add(name, strings.TrimSpace(val))
		case "--style":
			v, ok := value()
			if !ok {
				return usageErr("--style needs a value")
			}
			style, err := detect.ParseStyle(v)
			if err != nil {
				return usageErr("%v", err)
			}
			cfg.style = style
		case "--items", "--next":
			v, ok := value()
			if !ok {
				return usageErr("%s needs a JSON pointer argument", flag)
			}
			if err := jsonptr.Validate(v); err != nil {
				return usageErr("%s: %v", flag, err)
			}
			if flag == "--items" {
				cfg.itemsPtr = v
			} else {
				cfg.overrides.NextPtr = v
			}
		case "--cursor-param", "--page-param", "--offset-param", "--limit-param":
			v, ok := value()
			if !ok || v == "" {
				return usageErr("%s needs a parameter name", flag)
			}
			switch flag {
			case "--cursor-param":
				cfg.overrides.CursorParam = v
			case "--page-param":
				cfg.overrides.PageParam = v
			case "--offset-param":
				cfg.overrides.OffsetParam = v
			case "--limit-param":
				cfg.overrides.LimitParam = v
			}
		case "--max-pages", "--max-items", "--retries":
			v, ok := value()
			if !ok {
				return usageErr("%s needs an integer argument", flag)
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return usageErr("%s: %q is not a non-negative integer", flag, v)
			}
			switch flag {
			case "--max-pages":
				cfg.maxPages = n
			case "--max-items":
				cfg.maxItems = n
			case "--retries":
				cfg.retries = n
			}
		case "--retry-wait", "--delay", "--timeout":
			v, ok := value()
			if !ok {
				return usageErr("%s needs a duration argument (e.g. 500ms, 2s)", flag)
			}
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return usageErr("%s: %q is not a non-negative duration", flag, v)
			}
			switch flag {
			case "--retry-wait":
				cfg.retryWait = d
			case "--delay":
				cfg.delay = d
			case "--timeout":
				cfg.timeout = d
			}
		default:
			return usageErr("unknown flag %q", arg)
		}
	}
	if cfg.url == "" {
		return usageErr("missing URL argument")
	}
	return cfg, exitOK, false
}
