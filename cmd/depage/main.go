// Command depage flattens any paginated JSON API into one NDJSON stream.
package main

import (
	"os"

	"github.com/JaydenCJ/depage/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
