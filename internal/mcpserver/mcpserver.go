// Package mcpserver exposes the MCP tool surface over Streamable HTTP. The
// tools/call surface is the lowest common denominator across MCP clients, so
// nothing here uses resources or prompts. Until bearer-token auth lands, the
// server refuses to bind anywhere but loopback -- the local-only posture is a
// startup invariant, not a convention.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/auth"
	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/daemon"
	"github.com/enqack/cognosis/internal/lifecycle"
	"github.com/enqack/cognosis/internal/migrate"
	"github.com/enqack/cognosis/internal/persona"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// Server implements daemon.Runner: it serves MCP until its context ends.
type Server struct {
	bind      string
	vaultDir  string
	log       *slog.Logger
	pipeline  *write.Pipeline
	engine    *query.Engine
	store     *store.Store
	lifecycle *lifecycle.Engine
	personas  *persona.Registry
	tls       config.TLS
	// bindLoopback records that this daemon is bound to loopback only. It is a
	// server property because the transport hands tools headers but never a
	// peer address, so the bind is the only place the question can be asked.
	bindLoopback bool
	// Migrations, when set, backs the get_migration_status tool.
	Migrations *migrate.Coordinator
	// Version is the implementation version advertised to MCP clients. Set
	// post-construction (like Migrations); empty falls back to "dev".
	Version string
	// TrustLocalErrors mirrors config.TrustLocalErrors. See mayDiscloseTo: it is
	// one of three conditions required before a withheld cause is released, and
	// it must stay false for any daemon a reverse proxy fronts.
	TrustLocalErrors bool
}

func New(bind, vaultDir string, log *slog.Logger, p *write.Pipeline, e *query.Engine, s *store.Store,
	lc *lifecycle.Engine, pr *persona.Registry) (*Server, error) {
	return NewTLS(bind, vaultDir, log, p, e, s, lc, pr, config.TLS{})
}

// NewTLS is New with built-in TLS termination configured -- the only door to a
// non-loopback bind. The documented default remote path keeps Cognosis on
// loopback behind a TLS-terminating reverse proxy instead (see docs/remote.md).
func NewTLS(bind, vaultDir string, log *slog.Logger, p *write.Pipeline, e *query.Engine, s *store.Store,
	lc *lifecycle.Engine, pr *persona.Registry, tls config.TLS) (*Server, error) {
	if err := requireLoopback(bind, tls); err != nil {
		return nil, err
	}
	return &Server{
		bind:      bind,
		vaultDir:  vaultDir,
		log:       log.With("component", "mcpserver"),
		pipeline:  p,
		engine:    e,
		store:     s,
		lifecycle: lc,
		personas:  pr,
		tls:       tls,
		// SplitHostPort already succeeded inside requireLoopback; a bind that
		// cannot be parsed never reaches here.
		bindLoopback: func() bool {
			host, _, err := net.SplitHostPort(bind)
			return err == nil && isLoopbackHost(host)
		}(),
	}, nil
}

// requireLoopback rejects any bind address that isn't loopback unless
// built-in TLS is configured -- never plaintext bearer tokens on a network.
func requireLoopback(bind string, tls config.TLS) error {
	const op = "mcpserver.requireLoopback"
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return cogerr.Ef(op, cogerr.Validation, "bind_address %q: %v", bind, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if tls.Enabled() {
		return nil
	}
	return cogerr.Ef(op, cogerr.Validation,
		"bind_address %q is not loopback; configure tls.cert_file/tls.key_file for direct TLS, or keep loopback behind a TLS-terminating reverse proxy", bind)
}

// isLoopbackHost reports whether a bind host names only this machine.
//
// "localhost" is resolved rather than trusted as a string. /etc/hosts can map it
// to a routable address, and the consequences are not symmetric: a string
// exemption would let requireLoopback accept a reachable bind without TLS *and*
// let mayDiscloseTo hand that reachable bind's callers full internal errors.
// Every resolved address must be loopback -- a name resolving to both is not
// loopback-only, and treating it as such is the whole failure.
//
// Resolution failure reports false. A name that cannot be resolved has not been
// shown to be local, and both callers fail safe on false.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Bounded: this runs on the startup path, and a name that needs longer than
	// this to resolve has not been shown to be local either way.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := (&net.Resolver{}).LookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// forwardedHeaders are the hop markers a proxy adds. Any one of them present
// means this request did not originate on this machine, whatever the peer
// address says.
//
// A forged header only ever *withholds* detail, so an attacker gains nothing by
// adding one; the failure mode is a caller seeing a terser error.
var forwardedHeaders = []string{
	"X-Forwarded-For",
	"Forwarded",
	"X-Real-Ip",
	"X-Forwarded-Host",
	"X-Client-Ip",
	"Cf-Connecting-Ip",
	"True-Client-Ip",
}

// mayDiscloseTo reports whether this specific call may be handed a withheld
// cause. All three conditions must hold; see toolError for why none suffices
// alone.
//
// Per-request rather than per-server on purpose: a daemon can be bound to
// loopback and still be fronted by a proxy on the same host, so the only thing
// that distinguishes the local CLI from a forwarded remote is what arrived on
// *this* request. Absent header metadata withholds -- the SDK not surfacing a
// request is indistinguishable from a request whose origin we cannot check.
func (s *Server) mayDiscloseTo(req *mcp.CallToolRequest) bool {
	if !s.TrustLocalErrors || !s.bindLoopback {
		return false
	}
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return false
	}
	for _, h := range forwardedHeaders {
		if req.Extra.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// toolError is what the calling agent sees when a tool fails.
//
// cogerr.Error prints as "op: kind: cause", which is the right shape for a log
// line and the wrong one for a tool result: an agent reading
// "write.Pipeline.Edit: validation: old_string appears 2 times" has to parse
// past two internal identifiers to reach the sentence it can act on. The op and
// kind are not lost -- audit and the structured log still record the full error.
//
// Internal and Unavailable are deliberately not passed through. Both wrap raw
// pgx, os and net errors that carry DSNs, unix socket paths, database users and
// embedding endpoints -- one of this project's own log lines contains
// "failed to connect to `user=sysop database=cognosis`: /Users/.../.s.PGSQL.5434"
// verbatim. An agent cannot act on any of it, and a tool result is the one
// place it would travel furthest.
//
// Gating on Kind at this boundary rather than redacting at each call site is
// deliberate. There are 16 raw-wrapped Unavailable sites today and the
// author-written ones still interpolate the endpoint, so per-site redaction is
// 22 edits that a 23rd site silently defeats. Here it holds for every site that
// will ever exist.
//
// Nothing actionable is lost: Unavailable means a dependency is down, and the
// remedy is always operator-side. The two kinds get distinct messages so the
// caller can still tell "retry later" from "report a bug".
func (s *Server) toolError(req *mcp.CallToolRequest, err error) error {
	if err == nil {
		return nil
	}
	var e *cogerr.Error
	if !errors.As(err, &e) {
		return err // not a domain error (argument checks, SDK errors): already plain
	}
	// Exhaustive on purpose. This is a redaction boundary, so a newly added
	// Kind must force an explicit decision about whether its causes are safe to
	// hand a remote caller, rather than defaulting to exposure.
	// Three independent keys before releasing a withheld cause, because none is
	// sufficient alone. See mayDiscloseTo.
	if s.mayDiscloseTo(req) {
		return fmt.Errorf("%s", cogerr.Message(err))
	}
	switch e.Kind {
	case cogerr.Internal:
		return fmt.Errorf("internal error (see the daemon log for detail)")
	case cogerr.Unavailable:
		return fmt.Errorf("a required service is unavailable (see the daemon log for detail)")
	case cogerr.NotFound, cogerr.Conflict, cogerr.Validation:
		// Author-written and actionable: which field, which path, what to do
		// instead. These are the messages the caller needs to fix its call.
	}
	return fmt.Errorf("%s", cogerr.Message(err))
}

// audit records one tool call under the caller's token identity. summary must
// already be redacted (identifying args only -- never note content).
//
// Stores TokenID rather than the name, which is the opposite of what the
// structured log does (see auth.NewIdentityHandler). Deliberate: audit_log is
// queryable and can join to tokens.name at read time, and internal/store/tokens.go
// has no delete -- revocation only sets revoked_at -- so that join never dangles.
// The log is an append-only text stream with no join available to whoever reads
// it, so it carries the human-readable name. Different media, different
// normalization; a denormalized copy here would be a second source of truth.
func (s *Server) audit(ctx context.Context, tool, project, summary string, callErr error) {
	var tokenID *uuid.UUID
	if id, ok := auth.FromContext(ctx); ok {
		tokenID = &id.TokenID
	}
	msg := ""
	if callErr != nil {
		msg = callErr.Error()
	}
	if err := s.store.AppendAudit(ctx, tokenID, tool, project, summary, callErr == nil, msg); err != nil {
		s.log.ErrorContext(ctx, "audit append failed", "tool", tool, "reason", err)
	}
}

// Run serves until ctx is cancelled (graceful shutdown).
func (s *Server) Run(ctx context.Context) error {
	const op = "mcpserver.Run"

	version := s.Version
	if version == "" {
		version = "dev"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "cognosis", Version: version}, nil)
	s.addTools(srv)

	mux := http.NewServeMux()
	mux.Handle("/", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	mux.HandleFunc("GET /context", s.handleContext)

	httpSrv := &http.Server{
		Addr:              s.bind,
		Handler:           auth.Middleware(s.store, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		func() {
			defer daemon.RecoverPanic(s.log, "mcpserver.Run listener", func(err error) { serveErr = err })
			if s.tls.Enabled() {
				serveErr = httpSrv.ListenAndServeTLS(s.tls.CertFile, s.tls.KeyFile)
				return
			}
			serveErr = httpSrv.ListenAndServe()
		}()
		errCh <- serveErr
	}()
	// Deliberately not InfoContext: this is startup, before any request, so
	// there is no authenticated caller to attribute it to. logcontext_test.go
	// allowlists this message -- see auth.NewIdentityHandler on why a missing
	// token= means daemon-internal work rather than broken attribution.
	//nolint:sloglint // startup, before any request: no caller to attribute to
	s.log.Info("mcp server listening", "addr", s.bind, "tls", s.tls.Enabled())

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return cogerr.E(op, cogerr.Unavailable, err)
		}
		return nil
	}
}

// readNoteFile returns the raw file for a vault-relative path after the same
// path validation the write pipeline applies.
func (s *Server) readNoteFile(rel string) (string, error) {
	const op = "mcpserver.readNoteFile"
	rel = filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", cogerr.Ef(op, cogerr.Validation, "path %q escapes the vault", rel)
	}
	if _, ok := vault.StageOf(rel); !ok && !vault.IsReserved(rel) {
		return "", cogerr.Ef(op, cogerr.Validation, "path %q is outside the vault layout", rel)
	}
	b, err := os.ReadFile(filepath.Join(s.vaultDir, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return "", cogerr.Ef(op, cogerr.NotFound, "no note at %q", rel)
		}
		return "", cogerr.E(op, cogerr.Internal, err)
	}
	return string(b), nil
}
