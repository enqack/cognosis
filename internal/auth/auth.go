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
	"errors"
	"fmt"
	"io/fs"
	"math"
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
	// UUIDv7: time-ordered, so ids sort lexically by creation and the id itself
	// dates a credential — the same contract note ids carry. Safe despite being
	// predictable, because the id is a lookup key rather than a secret: auth
	// rests on the Argon2id-verified secret half, and Middleware returns one
	// 401 for both an unknown id and a bad secret, so there is no oracle.
	id, err = uuid.NewV7()
	if err != nil {
		return "", uuid.Nil, "", cogerr.E(op, cogerr.Internal, err)
	}
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
	wantLen := len(want)
	if wantLen < 0 || wantLen > math.MaxUint32 {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, t, m, p, uint32(wantLen))
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
	// Token ids are UUIDv7, same contract as note ids (vault.NewNoteID). v4 is
	// rejected rather than merely discouraged: accepting both would leave the
	// time-ordering property unenforced and silently optional.
	if id.Version() != 7 {
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
		ctx := context.WithValue(r.Context(), ctxKey{},
			Identity{TokenID: t.ID, Name: t.Name})
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
//
// Provisioned means the stashed plaintext still authenticates, not merely that
// the file exists. Both halves are droppable independently: the tokens table
// lives in the database schema, which the vault contract declares derived and
// rebuildable, so `drop schema public cascade` — the documented remedy for a
// migration renumber — takes every token row with it while the file survives.
// A presence-only check left that file pointing at a deleted row: the daemon
// reported healthy, `status` said ok, and every client silently 401'd with
// nothing in the logs connecting the two.
func EnsureLocalToken(ctx context.Context, s *store.Store, tokenFile string) error {
	const op = "auth.EnsureLocalToken"
	switch b, err := os.ReadFile(tokenFile); {
	case err == nil:
		usable, err := localTokenUsable(ctx, s, tokenFile, strings.TrimSpace(string(b)))
		if err != nil {
			return err
		}
		if usable {
			return nil
		}
		// Fall through and re-mint over the dead file.
	case errors.Is(err, fs.ErrNotExist):
		// Not provisioned yet.
	default:
		return cogerr.E(op, cogerr.Internal, err)
	}
	plaintext, id, hash, err := Generate()
	if err != nil {
		return err
	}
	// Always the plain name. Uniqueness is scoped to live tokens, so a revoked
	// `local` frees the name and rotation keeps it — which is what makes
	// `token=local` in a log line always mean exactly the daemon.
	//
	// A *live* `local` with no state-dir file means a fresh state dir pointed at
	// an existing database, or a rotation done out of order. That is operator
	// error, and this refuses rather than minting under a mangled name: a daemon
	// whose own token is called something unrecognisable is worse than one that
	// says what is wrong. Mirrors the revocation refusal in localTokenUsable.
	if err := s.CreateToken(ctx, id, LocalTokenName, hash); err != nil {
		return cogerr.Ef(op, cogerr.Validation,
			"a live token named %q already exists but %s is missing; "+
				"run `cognosis token revoke %s` and restart to mint a fresh one",
			LocalTokenName, tokenFile, LocalTokenName)
	}
	if err := os.WriteFile(tokenFile, []byte(plaintext+"\n"), 0o600); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// CheckLocalToken reports whether the stashed local token would authenticate,
// for `cognosis status`.
//
// It runs through the same localTokenUsable the provisioning path uses, rather
// than reimplementing the checks: a status line that says "ok" via a second
// copy of the logic is worth less than no line at all, and the failure it
// exists to catch is precisely provisioning and request-time disagreeing.
func CheckLocalToken(ctx context.Context, s *store.Store, tokenFile string) error {
	const op = "auth.CheckLocalToken"
	b, err := os.ReadFile(tokenFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cogerr.Ef(op, cogerr.NotFound, "no local token at %s; it is minted on daemon start", tokenFile)
	case err != nil:
		return cogerr.E(op, cogerr.Internal, err)
	}
	usable, err := localTokenUsable(ctx, s, tokenFile, strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	if !usable {
		return cogerr.Ef(op, cogerr.Validation,
			"the token in %s does not authenticate: its row is gone (a schema rebuild drops the tokens "+
				"table) or the file was replaced. Restart the daemon to re-mint, then update any client "+
				"config holding a copy", tokenFile)
	}
	return nil
}

// localTokenUsable reports whether the stashed plaintext would still pass
// Middleware. It runs the same checks in the same order — parse, look up by
// embedded id, reject revoked, verify the secret — because a divergence here
// is exactly the failure being fixed: a token that provisioning calls fine and
// every request calls invalid.
//
// The error return is load-bearing and distinct from a false: false means
// "known dead, re-mint", an error means "unknown". Re-minting on a transient
// database failure would overwrite a working credential in every client's
// config with one they have not been given.
func localTokenUsable(ctx context.Context, s *store.Store, tokenFile, plaintext string) (bool, error) {
	const op = "auth.EnsureLocalToken"
	id, secret, ok := parseToken(plaintext)
	if !ok {
		return false, nil // truncated or hand-edited: unusable by construction
	}
	t, err := s.GetTokenByID(ctx, id)
	if cogerr.Is(err, cogerr.NotFound) {
		return false, nil // row dropped with the schema, or a different database
	}
	if err != nil {
		return false, err
	}
	if t.RevokedAt != nil {
		// Deliberate operator action, not drift. Minting a replacement would
		// undo a revocation silently — and, since the name is taken, under a
		// second name, leaving the revoked row looking effective. Fail loud
		// and make deleting the file the explicit re-provision gesture, which
		// is what the file being the source of truth already means.
		return false, cogerr.Ef(op, cogerr.Validation,
			"the local token in %s was revoked; delete the file to provision a new one", tokenFile)
	}
	return VerifySecret(secret, t.Hash), nil
}
