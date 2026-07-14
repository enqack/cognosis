package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
)

// fakeTokenStore is an in-memory TokenStore — Middleware needs no database.
type fakeTokenStore struct {
	tok      store.Token
	found    bool
	touched  int
	getCalls int
}

func (f *fakeTokenStore) GetTokenByID(_ context.Context, id uuid.UUID) (store.Token, error) {
	f.getCalls++
	if !f.found || f.tok.ID != id {
		return store.Token{}, cogerr.E("fake.GetTokenByID", cogerr.NotFound, nil)
	}
	return f.tok, nil
}

func (f *fakeTokenStore) TouchToken(_ context.Context, _ uuid.UUID) { f.touched++ }

// mintInto builds a valid token and its matching store row in the fake.
func mintInto(t *testing.T, f *fakeTokenStore) (plaintext string) {
	t.Helper()
	pt, id, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.tok = store.Token{ID: id, Name: "test", Hash: hash}
	f.found = true
	return pt
}

func do(ts TokenStore, authHeader string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	Middleware(ts, next).ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareAcceptsValidTokenAndTouches(t *testing.T) {
	f := &fakeTokenStore{}
	pt := mintInto(t, f)
	rec := do(f, "Bearer "+pt)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", rec.Code)
	}
	if f.touched != 1 {
		t.Fatalf("expected TouchToken called once, got %d", f.touched)
	}
}

func TestMiddlewareRejectsMissingAndMalformed(t *testing.T) {
	f := &fakeTokenStore{}
	mintInto(t, f)
	for _, tc := range []struct {
		name, header string
	}{
		{"missing", ""},
		{"no-bearer", "Basic xyz"},
		{"malformed", "Bearer not-a-cog-token"},
	} {
		rec := do(f, tc.header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, rec.Code)
		}
	}
	// None of these should reach a token touch.
	if f.touched != 0 {
		t.Fatalf("unauthenticated requests touched the token %d times", f.touched)
	}
}

func TestMiddlewareRejectsRevokedToken(t *testing.T) {
	f := &fakeTokenStore{}
	pt := mintInto(t, f)
	now := time.Now()
	f.tok.RevokedAt = &now
	rec := do(f, "Bearer "+pt)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d, want 401", rec.Code)
	}
	if f.touched != 0 {
		t.Fatalf("revoked token should not be touched")
	}
}
