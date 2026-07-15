package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/store/storetest"
)

func TestGenerateVerifyRoundTrip(t *testing.T) {
	plaintext, id, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	gotID, secret, ok := parseToken(plaintext)
	if !ok || gotID != id {
		t.Fatalf("parse of own token failed: %v %v", gotID, ok)
	}
	if !VerifySecret(secret, hash) {
		t.Fatal("own secret does not verify")
	}
	if VerifySecret(secret+"x", hash) {
		t.Fatal("tampered secret verified")
	}
	if !strings.HasPrefix(hash, "argon2id$") {
		t.Fatalf("hash format: %s", hash)
	}
}

func TestParseTokenRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "cog_", "cog_notauuid_secret", "bearer-junk", "cog_" + strings.Repeat("a", 36)} {
		if _, _, ok := parseToken(bad); ok {
			t.Errorf("parsed garbage token %q", bad)
		}
	}
}

// TestMiddlewareLifecycle — valid token passes with identity attached;
// revocation is effective on the very next request (no cache, no restart).
func TestMiddlewareLifecycle(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	plaintext, id, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, id, "test-client", hash); err != nil {
		t.Fatal(err)
	}

	var gotIdentity Identity
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(Middleware(s, inner))
	defer srv.Close()

	call := func(token string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := call(""); got != http.StatusUnauthorized {
		t.Fatalf("no token: %d", got)
	}
	if got := call("cog_garbage"); got != http.StatusUnauthorized {
		t.Fatalf("garbage token: %d", got)
	}
	if got := call(plaintext); got != http.StatusOK {
		t.Fatalf("valid token: %d", got)
	}
	if gotIdentity.Name != "test-client" || gotIdentity.TokenID != id {
		t.Fatalf("identity = %+v", gotIdentity)
	}

	// Revoke → the very next request 401s.
	if err := s.RevokeToken(ctx, "test-client"); err != nil {
		t.Fatal(err)
	}
	if got := call(plaintext); got != http.StatusUnauthorized {
		t.Fatalf("revoked token still accepted: %d", got)
	}
}

func TestEnsureLocalTokenZeroConfig(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	tokenFile := filepath.Join(t.TempDir(), "local-token")

	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(b))
	if !strings.HasPrefix(tok, "cog_") {
		t.Fatalf("token file content: %q", tok)
	}
	info, _ := os.Stat(tokenFile)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}

	// Idempotent: a second start neither re-mints nor rewrites.
	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(tokenFile)
	if string(b) != string(b2) {
		t.Fatal("second EnsureLocalToken rewrote the token")
	}
	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %d (%v), want 1", len(tokens), err)
	}

	// Fresh state dir against an existing database (the file is gone, a live
	// "local" row remains): plaintexts aren't recoverable from hashes, so a
	// new token must be minted under a fallback name and the file recreated.
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenFile); err != nil {
		t.Fatal("token file not re-minted for a fresh state dir")
	}
	tokens, err = s.ListTokens(ctx)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens after re-mint = %d (%v), want 2", len(tokens), err)
	}
}
