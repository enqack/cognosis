// Command cognosis is the thin entrypoint; all wiring lives in internal/cli.
package main

import (
	"fmt"
	"os"

	"github.com/enqack/cognosis/internal/cli"
)

// version is stamped at link time via -ldflags "-X main.version=<v>" (see the
// magefile, which sources it from the VERSION file). It stays "dev" for a plain
// `go build`.
var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "cognosis:", err)
		os.Exit(1)
	}
}
