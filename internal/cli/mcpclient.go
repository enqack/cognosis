package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/config"
)

// mcpCallTimeout bounds a whole tool call. Deliberately generous: a restore
// shells out to git, so the 2s the `context inject` client uses would time out
// on a perfectly healthy daemon. This is a CLI operation a human is waiting on,
// not a request path.
const mcpCallTimeout = 60 * time.Second

// bearerTransport attaches the daemon's bearer token to every request. The
// streamable transport owns its own request construction, so the header goes on
// as a RoundTripper rather than per-call.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrippers must not modify the caller's request.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// daemonEndpoint renders the URL the CLI should use to reach the local daemon.
//
// The scheme follows the daemon's own TLS configuration. The `context inject`
// client hardcodes http and is simply wrong against a TLS-enabled daemon; do
// not copy it.
//
// Known limitation: `bind_address` is a *server* concern being read as a
// *client* one. A daemon bound to 0.0.0.0 yields a URL that is not usefully
// dialable and will not match a certificate, and the reverse-proxy topology in
// docs/remote.md has no config key naming the proxy at all. Both are remote
// setups; this path is for a CLI on the same host as its daemon.
func daemonEndpoint(cfg *config.Config) string {
	scheme := "http"
	if cfg.TLS.Enabled() {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: cfg.BindAddress, Path: "/"}
	return u.String()
}

// dialDaemon opens an MCP session against the local daemon. The returned
// close func must be called.
func dialDaemon(ctx context.Context, cfg *config.Config) (*mcp.ClientSession, func(), error) {
	token, err := localToken(cfg)
	if err != nil {
		return nil, nil, err
	}
	httpc := &http.Client{
		Timeout:   mcpCallTimeout,
		Transport: &bearerTransport{token: token},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "cognosis-cli", Version: buildVersion}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   daemonEndpoint(cfg),
		HTTPClient: httpc,
		// Request/response only. The CLI makes one call and exits, so a
		// standalone SSE stream for server-initiated messages is pure overhead
		// and one more thing to fail.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("cognosis daemon unreachable at %s: %w", daemonEndpoint(cfg), err)
	}
	return sess, func() { _ = sess.Close() }, nil
}

// callDaemonTool invokes one MCP tool and returns its text output.
//
// A tool that reports failure comes back as IsError with the message in the
// content rather than as a transport error, so both are folded into one Go
// error here — a caller deciding whether to fall back must not have to
// distinguish "the daemon refused" from "the daemon answered with a refusal".
func callDaemonTool(ctx context.Context, cfg *config.Config, name string, args map[string]any) (string, error) {
	sess, closeSess, err := dialDaemon(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer closeSess()

	cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()

	res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}
