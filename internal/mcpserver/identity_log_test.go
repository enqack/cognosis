package mcpserver

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/auth"
	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
)

// TestIdentityReachesTheLogOverRealMCPSession — the token attribute is written
// by auth.NewIdentityHandler from an Identity that only auth.Middleware ever
// puts in a context, and the context has to survive the HTTP transport, the MCP
// session, and the .With("component") decoration applied at construction.
//
// A hand-built request proves none of that, for the same reason recorded on
// TestDisclosureGateOverRealMCPSession: the value under test is populated by a
// layer such a test skips. The in-memory transport used by registeredTools
// never passes through Middleware either, so it can never see an identity — a
// future author reaching for that harness to test attribution will find it
// missing and should come here instead.
//
// The failure is silent in both directions. A non-rewrapping WithAttrs, or a
// missed *Context conversion, leaves every line looking correct and
// unattributable; and if the wrapper were applied but the identity never
// arrived, every query's leg counts would be pinned to one anonymous caller.
func TestIdentityReachesTheLogOverRealMCPSession(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(auth.NewIdentityHandler(slog.NewTextHandler(buf, nil)))
	// .With reproduces mcpserver.NewTLS:78 — the decoration that a broken
	// WithAttrs would silently strip identity from.
	srv := &Server{log: log.With("component", "mcpserver")}

	fake := &identityTokenStore{}
	plaintext := mintIdentityToken(t, fake, "desktop")

	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	// Stand-ins: the real tools need a live pipeline, engine and store. These
	// are the smallest bodies that exercise the two logging styles.
	mcp.AddTool(m, &mcp.Tool{Name: "attributed", Description: "logs with context"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			srv.log.InfoContext(ctx, "attributed_tool", "path", "x")
			return textResult("ok"), nil, nil
		})
	mcp.AddTool(m, &mcp.Tool{Name: "unattributed", Description: "logs without context"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			//nolint:sloglint // deliberate: this is the failure mode under test
			srv.log.Info("unattributed_tool", "path", "x")
			return textResult("ok"), nil, nil
		})

	ts := httptest.NewServer(auth.Middleware(fake, mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return m }, nil)))
	t.Cleanup(ts.Close)

	call := func(t *testing.T, tool string) {
		t.Helper()
		c := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil)
		sess, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint:             ts.URL,
			HTTPClient:           &http.Client{Transport: headerRoundTripper{"Authorization", "Bearer " + plaintext}},
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sess.Close() }()
		if _, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: struct{}{}}); err != nil {
			t.Fatal(err)
		}
	}

	call(t, "attributed")
	got := buf.String()
	if !strings.Contains(got, "token=desktop") {
		t.Fatalf("identity did not reach the log over a real session: %q", got)
	}
	if !strings.Contains(got, "component=mcpserver") {
		t.Fatalf("construction attrs lost: %q", got)
	}

	// The documented failure mode, asserted rather than described: a plain
	// Info() inside a handler produces a line with no caller. This is what
	// TestRequestScopedLogsCarryContext exists to prevent.
	buf.Reset()
	call(t, "unattributed")
	if got := buf.String(); strings.Contains(got, "token=") {
		t.Fatalf("plain Info() unexpectedly carried identity — the static guard may be "+
			"unnecessary, or this test is not measuring what it claims: %q", got)
	}
}

// TestIdentityHandlerAddsNothingWithoutAnIdentity — daemon-internal work (the
// watcher, migrations, CLI-driven lifecycle compiles) logs with no caller, and
// must not grow a misleading empty attribute.
func TestIdentityHandlerAddsNothingWithoutAnIdentity(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(auth.NewIdentityHandler(slog.NewTextHandler(buf, nil)))
	log.InfoContext(context.Background(), "internal_work")
	if got := buf.String(); strings.Contains(got, "token=") {
		t.Fatalf("annotated a record with no identity: %q", got)
	}
}

// syncBuffer guards the buffer: the log write happens on the server's request
// goroutine while the assertion runs on the test goroutine, and the suite runs
// under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// identityTokenStore is an in-memory auth.TokenStore, so this needs no Postgres.
type identityTokenStore struct {
	tok   store.Token
	found bool
}

func (f *identityTokenStore) GetTokenByID(_ context.Context, id uuid.UUID) (store.Token, error) {
	if !f.found || f.tok.ID != id {
		return store.Token{}, cogerr.E("fake.GetTokenByID", cogerr.NotFound, nil)
	}
	return f.tok, nil
}

func (f *identityTokenStore) TouchToken(_ context.Context, _ uuid.UUID) {}

func mintIdentityToken(t *testing.T, f *identityTokenStore, name string) string {
	t.Helper()
	pt, id, hash, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.tok = store.Token{ID: id, Name: name, Hash: hash}
	f.found = true
	return pt
}
