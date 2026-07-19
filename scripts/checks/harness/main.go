// Command harness drives Cognosis feature slices over real Streamable HTTP MCP
// and exits nonzero on any deviation. One shared MCP client backs every slice;
// invoke one slice at a time:
//
//	go run ./scripts/checks/harness <slice>
//
// where slice is memory-loop | retrieval | knowledge | platform | migration.
// Env: COGNOSIS_MCP_URL, COGNOSIS_TOKEN_FILE (or COGNOSIS_TOKEN); the platform
// slice needs COGNOSIS_DSN for its audit-table assertions and memory-loop needs
// it to read back the indexed note id across an edit. The check scripts under
// scripts/checks/ boot a daemon and call the matching slice.
//
// One mode is not an MCP slice:
//
//	go run ./scripts/checks/harness gen-cert <dir>
//
// writes the throwaway TLS trust chain the tls check needs, before any daemon
// exists to talk to (see gencert.go).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is the harness's advertised MCP client version. It stays "dev" for the
// usual `go run ./scripts/checks/harness` invocation and can be stamped at link
// time via -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	os.Exit(run())
}

// run returns 0 on success and 1 on any failure, including a usage error.
//
// It never returns 2, even though 2-means-usage is the usual CLI convention:
// in this subsystem 2 already means "skip" -- scripts/checks/_lib.sh's
// require_env exits 2 for a missing prerequisite and check-all.sh reports that
// check as skipped and carries on. A harness usage error is a programmer
// mistake, the opposite of a skippable one, so it must never be able to wear
// that number. Today `go run` collapses any non-zero exit to 1 and every caller
// consumes the code with `|| fail`, which would hide the collision; both are
// accidents, not guarantees.
func run() int {
	// gen-cert takes a directory, so it is dispatched ahead of the slices --
	// they all share the one-arg, MCP-driving shape and it shares neither.
	if len(os.Args) > 1 && os.Args[1] == "gen-cert" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: harness gen-cert <dir>")
			return 1
		}
		if err := genCert(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "harness gen-cert: %v\n", err)
			return 1
		}
		fmt.Println("harness gen-cert: OK")
		return 0
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: harness <memory-loop|retrieval|knowledge|platform|migration> | gen-cert <dir>")
		return 1
	}
	slice := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	slices := map[string]func(context.Context) error{
		"memory-loop": memoryLoop,
		"retrieval":   retrieval,
		"knowledge":   knowledge,
		"platform":    platform,
		"migration":   migration,
	}
	fn, ok := slices[slice]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown slice %q\n", slice)
		return 1
	}
	if err := fn(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "harness %s: %v\n", slice, err)
		return 1
	}
	fmt.Printf("harness %s: OK\n", slice)
	return 0
}

// --- memory-loop: write -> hybrid query -> get -> list, + contract + auth ----

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

// connect opens an authenticated MCP session from the standard env.
func connect(ctx context.Context) (*mcp.ClientSession, error) {
	endpoint := envOr("COGNOSIS_MCP_URL", "http://127.0.0.1:7433")
	token := strings.TrimSpace(os.Getenv("COGNOSIS_TOKEN"))
	if token == "" {
		path := os.Getenv("COGNOSIS_TOKEN_FILE")
		if path == "" {
			return nil, fmt.Errorf("set COGNOSIS_TOKEN or COGNOSIS_TOKEN_FILE")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: version}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearerTransport{token: token, base: http.DefaultTransport}},
	}, nil)
}

// call invokes one tool and returns its text content; tool errors become Go
// errors carrying the server's message.
func call(ctx context.Context, s *mcp.ClientSession, tool string, args map[string]any) (string, error) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s", text.String())
	}
	return text.String(), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
