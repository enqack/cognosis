// Package mcpserver exposes the MCP tool surface over Streamable HTTP. The
// tools/call surface is the lowest common denominator across MCP clients, so
// nothing here uses resources or prompts. Until bearer-token auth lands, the
// server refuses to bind anywhere but loopback — the local-only posture is a
// startup invariant, not a convention.
package mcpserver

import (
	"context"
	"errors"
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
	// Migrations, when set, backs the get_migration_status tool.
	Migrations *migrate.Coordinator
	// Version is the implementation version advertised to MCP clients. Set
	// post-construction (like Migrations); empty falls back to "dev".
	Version string
}

func New(bind, vaultDir string, log *slog.Logger, p *write.Pipeline, e *query.Engine, s *store.Store,
	lc *lifecycle.Engine, pr *persona.Registry) (*Server, error) {
	return NewTLS(bind, vaultDir, log, p, e, s, lc, pr, config.TLS{})
}

// NewTLS is New with built-in TLS termination configured — the only door to a
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
	}, nil
}

// requireLoopback rejects any bind address that isn't loopback unless
// built-in TLS is configured — never plaintext bearer tokens on a network.
func requireLoopback(bind string, tls config.TLS) error {
	const op = "mcpserver.requireLoopback"
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return cogerr.Ef(op, cogerr.Validation, "bind_address %q: %v", bind, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if tls.Enabled() {
		return nil
	}
	return cogerr.Ef(op, cogerr.Validation,
		"bind_address %q is not loopback; configure tls.cert_file/tls.key_file for direct TLS, or keep loopback behind a TLS-terminating reverse proxy", bind)
}

// audit records one tool call under the caller's token identity. summary must
// already be redacted (identifying args only — never note content).
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
		s.log.Error("audit append failed", "tool", tool, "reason", err)
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
