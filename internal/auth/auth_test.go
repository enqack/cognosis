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

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
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

// authenticates reports whether a plaintext token passes Middleware — the only
// definition of "works" that matters, since Middleware is what every client
// hits. Asserting on store state instead would re-check the code under test
// against itself.
func authenticates(t *testing.T, s *store.Store, plaintext string) bool {
	t.Helper()
	srv := httptest.NewServer(Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// TestEnsureLocalTokenRepairsAStaleFile is the regression for the bug this
// function shipped with: it returned early on os.Stat, so a token file whose
// backing row was gone survived every restart. The daemon came up healthy and
// every client 401'd.
//
// Both halves are independently droppable, and the drop that matters is
// documented: setup-guide.md recommends `drop schema public cascade` for a
// migration renumber, which takes the tokens table with it while the file in
// the state dir is untouched. On main this test fails at the last assertion —
// the file is byte-identical and still dead.
func TestEnsureLocalTokenRepairsAStaleFile(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		content func(t *testing.T) string
	}{
		{
			// A well-formed token naming a row that does not exist. This is
			// the post-schema-rebuild state exactly: GetTokenByID reaches the
			// same NotFound arm whether the row was dropped or the file came
			// from a different database.
			name: "row gone",
			content: func(t *testing.T) string {
				plaintext, _, _, err := Generate()
				if err != nil {
					t.Fatal(err)
				}
				return plaintext + "\n"
			},
		},
		{
			// Truncated by a full disk or a killed write. parseToken rejects
			// it, so it can never authenticate and there is nothing to save.
			name:    "unparseable",
			content: func(*testing.T) string { return "cog_truncated\n" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "local-token")
			stale := tc.content(t)
			if err := os.WriteFile(tokenFile, []byte(stale), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) == stale {
				t.Fatal("stale token file left in place; local clients stay locked out")
			}
			if info, _ := os.Stat(tokenFile); info.Mode().Perm() != 0o600 {
				t.Fatalf("re-minted token file mode = %v, want 0600", info.Mode().Perm())
			}
			if !authenticates(t, s, strings.TrimSpace(string(b))) {
				t.Fatal("re-minted token does not authenticate")
			}
		})
	}
}

// TestEnsureLocalTokenLeavesAWorkingTokenAlone pins the other direction, which
// the repair must not cost: a live token is not rotated on restart. Rotating
// would silently invalidate the copy already pasted into every client config.
func TestEnsureLocalTokenLeavesAWorkingTokenAlone(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	tokenFile := filepath.Join(t.TempDir(), "local-token")

	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(tokenFile)
	if string(before) != string(after) {
		t.Fatal("a usable token was rotated on restart")
	}
	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %d (%v), want 1 — verification minted a duplicate", len(tokens), err)
	}
}

// TestEnsureLocalTokenRefusesToUndoRevocation — revocation is an operator
// action, not drift, so it is the one unusable state that must not be
// repaired. Re-minting here would be worse than the bug being fixed: the name
// "local" is taken, so the replacement lands under a fallback name and the
// revoked row sits there looking effective while access is restored.
func TestEnsureLocalTokenRefusesToUndoRevocation(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	tokenFile := filepath.Join(t.TempDir(), "local-token")

	if err := EnsureLocalToken(ctx, s, tokenFile); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(tokenFile)
	if err := s.RevokeToken(ctx, "local"); err != nil {
		t.Fatal(err)
	}

	err := EnsureLocalToken(ctx, s, tokenFile)
	if err == nil {
		t.Fatal("revocation silently undone")
	}
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("kind = %v, want Validation — an operator has to be able to tell this from a database failure", cogerr.KindOf(err))
	}
	if !strings.Contains(err.Error(), tokenFile) {
		t.Errorf("error does not name the file to delete: %v", err)
	}
	after, _ := os.ReadFile(tokenFile)
	if string(before) != string(after) {
		t.Error("token file rewritten despite the refusal")
	}
	tokens, err2 := s.ListTokens(ctx)
	if err2 != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %d (%v), want 1 — a replacement was minted around the revocation", len(tokens), err2)
	}
}

// TestLooksLocalWithdrawsOnProxyMarkers — the header check is the fail-safe
// half of the disclosure gate, so what matters is that it only ever *removes*
// trust. A forged marker from a loopback peer must yield less detail, never
// more; and a proxy forwarding from 127.0.0.1 must not read as local, which is
// the exact topology docs/remote.md recommends.
func TestLooksLocalWithdrawsOnProxyMarkers(t *testing.T) {
	for _, c := range []struct {
		name   string
		addr   string
		header [2]string
		want   bool
	}{
		{"direct loopback", "127.0.0.1:53201", [2]string{}, true},
		{"direct loopback v6", "[::1]:53201", [2]string{}, true},
		{"remote peer", "203.0.113.7:44321", [2]string{}, false},
		{"proxied from loopback", "127.0.0.1:53201", [2]string{"X-Forwarded-For", "203.0.113.7"}, false},
		{"proxied, Forwarded header", "127.0.0.1:53201", [2]string{"Forwarded", "for=203.0.113.7"}, false},
		{"proxied, X-Real-Ip", "127.0.0.1:53201", [2]string{"X-Real-Ip", "203.0.113.7"}, false},
		{"unparseable addr", "garbage", [2]string{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
			r.RemoteAddr = c.addr
			if c.header[0] != "" {
				r.Header.Set(c.header[0], c.header[1])
			}
			if got := looksLocal(r); got != c.want {
				t.Errorf("looksLocal(%s, %v) = %v, want %v", c.addr, c.header, got, c.want)
			}
		})
	}
}
