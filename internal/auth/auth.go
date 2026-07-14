// Package auth implements per-client bearer tokens: Argon2id-hashed at rest,
// checked synchronously against the tokens table on every request (no cache —
// revocation is effective on the very next request, by design), with every
// tool call audit-logged under the resolved token identity.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
)

// Argon2id parameters — server-side hashing of high-entropy secrets, so the
// moderate memory cost is about defense in depth, not password stretching.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// Generate mints a new token: "cog_<token-id>_<secret>". Embedding the id
// makes verification a single-row lookup instead of a scan over every hash.
// The plaintext is returned once and never stored.
func Generate() (plaintext string, id uuid.UUID, hash string, err error) {
	const op = "auth.Generate"
	id = uuid.New()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", uuid.Nil, "", cogerr.E(op, cogerr.Internal, err)
	}
	secretStr := base64.RawURLEncoding.EncodeToString(secret)
	plaintext = fmt.Sprintf("cog_%s_%s", id, secretStr)
	hash, err = HashSecret(secretStr)
	if err != nil {
		return "", uuid.Nil, "", err
	}
	return plaintext, id, hash, nil
}

// HashSecret produces a self-describing Argon2id hash string.
func HashSecret(secret string) (string, error) {
	const op = "auth.HashSecret"
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", cogerr.E(op, cogerr.Internal, err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifySecret checks a secret against a stored hash in constant time.
func VerifySecret(secret, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// parseToken splits "cog_<uuid>_<secret>".
func parseToken(tok string) (uuid.UUID, string, bool) {
	rest, ok := strings.CutPrefix(tok, "cog_")
	if !ok {
		return uuid.Nil, "", false
	}
	// The uuid is fixed-width (36 chars) followed by "_<secret>".
	if len(rest) < 38 || rest[36] != '_' {
		return uuid.Nil, "", false
	}
	id, err := uuid.Parse(rest[:36])
	if err != nil {
		return uuid.Nil, "", false
	}
	return id, rest[37:], true
}

// Identity is the resolved caller attached to the request context.
type Identity struct {
	TokenID uuid.UUID
	Name    string
}

type ctxKey struct{}

// FromContext returns the authenticated identity, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// TokenStore is the minimal token surface Middleware needs — the concrete
// *store.Store satisfies it, and tests can supply a fake without a database.
type TokenStore interface {
	GetTokenByID(ctx context.Context, id uuid.UUID) (store.Token, error)
	TouchToken(ctx context.Context, id uuid.UUID)
}

// Middleware resolves the bearer token against the tokens table on every
// request. Latency is traded for correctness: no cache means no
// revoked-token window.
func Middleware(s TokenStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || raw == r.Header.Get("Authorization") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		id, secret, ok := parseToken(raw)
		if !ok {
			http.Error(w, "malformed token", http.StatusUnauthorized)
			return
		}
		t, err := s.GetTokenByID(r.Context(), id)
		if err != nil || t.RevokedAt != nil || !VerifySecret(secret, t.Hash) {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}
		s.TouchToken(r.Context(), t.ID)
		ctx := context.WithValue(r.Context(), ctxKey{}, Identity{TokenID: t.ID, Name: t.Name})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// EnsureLocalToken implements the zero-config local posture: local clients
// (the CLI, hooks, Claude Code registration) authenticate with the plaintext
// token stashed (0600) in the state dir. The file is the source of truth for
// "is local access provisioned" — a live DB token without the file (fresh
// state dir against an existing database) still needs a fresh mint, since
// plaintexts are never recoverable from hashes. The file is the local trust
// boundary — same as the vault itself.
func EnsureLocalToken(ctx context.Context, s *store.Store, tokenFile string) error {
	const op = "auth.EnsureLocalToken"
	if _, err := os.Stat(tokenFile); err == nil {
		return nil // provisioned; the DB row backing it stays authoritative
	}
	plaintext, id, hash, err := Generate()
	if err != nil {
		return err
	}
	// "local" may already be taken by a previous state dir's token — mint
	// under a unique name rather than failing (old ones are revocable via
	// `cognosis token list`/`revoke`).
	name := "local"
	if err := s.CreateToken(ctx, id, name, hash); err != nil {
		name = "local-" + id.String()[:8]
		if err := s.CreateToken(ctx, id, name, hash); err != nil {
			return err
		}
	}
	if err := os.WriteFile(tokenFile, []byte(plaintext+"\n"), 0o600); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}
