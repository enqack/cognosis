package auth

import (
	"context"
	"log/slog"
)

// NewIdentityHandler wraps a slog.Handler so that any record logged with a
// context carrying an authenticated Identity is annotated with that caller's
// token name.
//
// It exists because per-leg retrieval counters (query_knowledge's vector/fts/
// graph/fused/sources numbers) live only in the structured log, never in
// audit_log -- so without this there is no way to tell one client's query shape
// from another's. An agent investigating retrieval writes telemetry
// indistinguishable from ordinary use and silently becomes the majority of the
// sample: 19 of 22 recorded queries, on the first occasion this was checked.
//
// The attribute is deliberately Identity.Name rather than TokenID. The UUID is
// noise in a text stream and is already audit_log's key; the name is what an
// operator reads. EnsureLocalToken always mints under exactly "local" (live-name
// uniqueness frees the name on revocation), so token=local in a log line always
// means the daemon's own token.
//
// # Absence is meaningful, not a failure
//
// Only operations that arrived through Middleware carry an identity. The
// watcher, the migration worker, lifecycle compiles driven from the CLI, and
// every startup line run with no request context and log without the attribute.
// A missing token= identifies daemon-internal work rather than broken
// attribution.
func NewIdentityHandler(inner slog.Handler) slog.Handler {
	return identityHandler{inner: inner}
}

type identityHandler struct{ inner slog.Handler }

func (h identityHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h identityHandler) Handle(ctx context.Context, r slog.Record) error {
	id, ok := FromContext(ctx)
	if !ok {
		return h.inner.Handle(ctx, r)
	}
	// Clone before AddAttrs: slog's Handler contract forbids mutating a record
	// we were handed, and Record's attr storage can be shared with the caller's.
	r = r.Clone()
	r.AddAttrs(slog.String("token", id.Name))
	return h.inner.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap. Returning h.inner.WithAttrs(as)
// directly compiles and passes any test that logs through a freshly constructed
// handler -- and silently drops identity from every line the MCP server emits,
// because mcpserver.NewTLS calls .With("component", "mcpserver") on
// construction. That bug disables this entirely while leaving it looking wired.
func (h identityHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return identityHandler{inner: h.inner.WithAttrs(as)}
}

// WithGroup nests the identity attribute under the group, so a record logged
// after WithGroup("g") carries g.token rather than token. Nothing in this tree
// calls WithGroup; the alternative -- holding a second pre-group handler to emit
// the attribute ungrouped -- is materially more code for a case that does not
// occur. Pinned by a test so the nesting is a decision rather than an accident.
func (h identityHandler) WithGroup(name string) slog.Handler {
	return identityHandler{inner: h.inner.WithGroup(name)}
}
