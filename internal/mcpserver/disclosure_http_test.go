package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/cogerr"
)

// TestDisclosureGateOverRealMCPSession — mayDiscloseTo reads the forwarding
// markers off req.Extra.Header, which nothing in this package populates. It is
// filled by the SDK's streamable transport from the live HTTP request, so a
// unit test that hand-builds a request proves only that the comparison works,
// never that the header survives the trip.
//
// The failure that matters is silent in both directions. If the SDK stopped
// populating Extra.Header, mayDiscloseTo would return false forever and the
// feature would be dead while every hand-built unit test stayed green. If it
// populated the header but the transport dropped the hop markers, a
// reverse-proxied deployment would hand DSNs to remote agents. Only a real
// session distinguishes those, so this drives one.
func TestDisclosureGateOverRealMCPSession(t *testing.T) {
	// The secret a withheld Unavailable cause carries in production: a DSN with
	// a socket path and a username in it.
	secret := "failed to connect to `user=sysop database=cognosis`: /Users/sysop/.pg-data/.s.PGSQL.5434"
	wrapped := cogerr.E("store.UpsertNote", cogerr.Unavailable, errors.New(secret))

	// serve stands up one daemon at a given operator setting. The tool body is
	// the smallest thing that reaches toolError with a genuine request, standing
	// in for any real tool.
	serve := func(t *testing.T, srv *Server) *httptest.Server {
		t.Helper()
		m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		mcp.AddTool(m, &mcp.Tool{Name: "boom", Description: "always fails"},
			func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
				return nil, nil, srv.toolError(req, wrapped)
			})
		ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return m }, nil))
		t.Cleanup(ts.Close)
		return ts
	}

	// call returns the text of the failed tool result.
	call := func(t *testing.T, ts *httptest.Server, forwarded bool) string {
		t.Helper()
		httpc := &http.Client{}
		if forwarded {
			httpc.Transport = headerRoundTripper{"X-Forwarded-For", "203.0.113.9"}
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil)
		sess, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint:             ts.URL,
			HTTPClient:           httpc,
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sess.Close() }()

		res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "boom", Arguments: struct{}{}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("tool reported success; the error path never ran")
		}
		var text string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
		return text
	}

	discloses := func(body string) bool {
		return strings.Contains(body, ".s.PGSQL") || strings.Contains(body, "user=sysop")
	}

	loopbackTrusting := &Server{TrustLocalErrors: true, bindLoopback: true}
	loopbackDefault := &Server{TrustLocalErrors: false, bindLoopback: true}
	exposedTrusting := &Server{TrustLocalErrors: true, bindLoopback: false}

	// Opted in, loopback bind, no hop markers: all three keys turn, and the
	// operator gets the cause they asked for. This case failing means the header
	// never arrived and the opt-in is inert.
	if body := call(t, serve(t, loopbackTrusting), false); !discloses(body) {
		t.Errorf("all three conditions hold but no cause was released, so trust_local_errors does nothing over a real session: %s", body)
	}

	// Opted in, but this call carries a proxy marker. httptest binds loopback,
	// so the packet is indistinguishable from the local CLI by address — the
	// header is the only evidence, and it must withdraw the detail.
	if body := call(t, serve(t, loopbackTrusting), true); discloses(body) {
		t.Errorf("disclosed the DSN to a caller carrying X-Forwarded-For; every reverse-proxied deployment leaks: %s", body)
	}

	// Not opted in. Loopback is never sufficient alone, or the default posture
	// would be the unsafe one.
	if body := call(t, serve(t, loopbackDefault), false); discloses(body) {
		t.Errorf("disclosed with trust_local_errors off: %s", body)
	}

	// Opted in on a non-loopback bind. An operator asserting trust does not make
	// a TLS-exposed daemon local, and no header can prove otherwise.
	if body := call(t, serve(t, exposedTrusting), false); discloses(body) {
		t.Errorf("disclosed from a non-loopback bind on the operator's word alone: %s", body)
	}
}

// headerRoundTripper adds one header to every request, standing in for the
// proxy that would add it.
type headerRoundTripper struct{ key, value string }

func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(clone)
}

// TestMayDiscloseToWithholdsWithoutHeaderMetadata — absent metadata must read
// as "cannot verify", not as "nothing suspicious". A request with no Extra, or
// an Extra with no Header, has not been shown to be local; treating it as local
// would make every non-HTTP transport disclose by default.
func TestMayDiscloseToWithholdsWithoutHeaderMetadata(t *testing.T) {
	s := &Server{TrustLocalErrors: true, bindLoopback: true}
	for _, c := range []struct {
		name string
		req  *mcp.CallToolRequest
	}{
		{"nil request", nil},
		{"no extra", &mcp.CallToolRequest{}},
		{"extra without header", &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if s.mayDiscloseTo(c.req) {
				t.Error("disclosed to a request whose origin could not be checked")
			}
		})
	}
}

// Each forwarding marker must withdraw disclosure on its own. A proxy sets
// whichever its vendor chose, so a list that silently covers only the common
// two is a leak on every deployment behind the others.
func TestEveryForwardedHeaderWithdrawsDisclosure(t *testing.T) {
	s := &Server{TrustLocalErrors: true, bindLoopback: true}
	for _, h := range forwardedHeaders {
		t.Run(h, func(t *testing.T) {
			req := localReq()
			req.Extra.Header.Set(h, "203.0.113.9")
			if s.mayDiscloseTo(req) {
				t.Errorf("%s present but disclosure still granted", h)
			}
		})
	}
}
