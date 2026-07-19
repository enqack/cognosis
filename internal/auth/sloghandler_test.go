package auth

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// logWith renders one Info record through the identity handler and returns the
// emitted line. decorate applies whatever .With/.WithGroup chain the case is
// exercising, which is the part that actually distinguishes a correct wrapper
// from one that silently stops annotating.
func logWith(ctx context.Context, decorate func(*slog.Logger) *slog.Logger) string {
	var buf bytes.Buffer
	l := slog.New(NewIdentityHandler(slog.NewTextHandler(&buf, nil)))
	if decorate != nil {
		l = decorate(l)
	}
	l.InfoContext(ctx, "probe", "k", "v")
	return buf.String()
}

func identityCtx(t *testing.T, name string) context.Context {
	t.Helper()
	// Constructed directly against the unexported ctxKey rather than through an
	// exported helper: auth.WithIdentity existed once and was removed with the
	// feature that needed it (234ed54). Middleware is the only production
	// writer, and an in-package test can reach the key without re-widening the
	// API surface for testing's sake.
	return context.WithValue(context.Background(), ctxKey{}, Identity{TokenID: uuid.New(), Name: name})
}

func TestIdentityHandlerAnnotatesWhenIdentityPresent(t *testing.T) {
	got := logWith(identityCtx(t, "desktop"), nil)
	if !strings.Contains(got, "token=desktop") {
		t.Fatalf("no token attribute in %q", got)
	}
}

func TestIdentityHandlerSilentWithoutIdentity(t *testing.T) {
	got := logWith(context.Background(), nil)
	if strings.Contains(got, "token=") {
		t.Fatalf("annotated a record with no identity in context: %q", got)
	}
}

// TestIdentityHandlerSurvivesWith is the test that matters. mcpserver.NewTLS
// calls log.With("component", "mcpserver") on construction, so a WithAttrs that
// returns the inner handler instead of re-wrapping drops identity from every
// line the server ever emits — while every other test here still passes.
func TestIdentityHandlerSurvivesWith(t *testing.T) {
	ctx := identityCtx(t, "code")
	got := logWith(ctx, func(l *slog.Logger) *slog.Logger {
		return l.With("component", "mcpserver").With("extra", 1)
	})
	if !strings.Contains(got, "token=code") {
		t.Fatalf("identity lost after .With(): %q", got)
	}
	if !strings.Contains(got, "component=mcpserver") {
		t.Fatalf("inner attrs lost: %q", got)
	}
}

// TestIdentityHandlerNestsUnderGroup pins the documented WithGroup behaviour so
// the nesting is a decision rather than an accident. Nothing in the tree calls
// WithGroup today.
func TestIdentityHandlerNestsUnderGroup(t *testing.T) {
	ctx := identityCtx(t, "probe")
	got := logWith(ctx, func(l *slog.Logger) *slog.Logger { return l.WithGroup("g") })
	if !strings.Contains(got, "g.token=probe") {
		t.Fatalf("expected identity nested as g.token: %q", got)
	}
}

// TestIdentityHandlerDelegatesEnabled guards against the wrapper forcing work
// the inner handler would have skipped.
func TestIdentityHandlerDelegatesEnabled(t *testing.T) {
	var buf bytes.Buffer
	h := NewIdentityHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(Info) = true against an Error-level inner handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("Enabled(Error) = false against an Error-level inner handler")
	}
}
