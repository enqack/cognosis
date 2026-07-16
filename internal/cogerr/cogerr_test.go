package cogerr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
)

// TestKindStringNamesEveryKind pins the wire-facing spelling of each Kind: the
// MCP layer maps Kind to tool-error responses and slog logs it as a structured
// field, so these strings are an external contract, not cosmetics.
func TestKindStringNamesEveryKind(t *testing.T) {
	cases := []struct {
		kind cogerr.Kind
		want string
	}{
		{cogerr.Internal, "internal"},
		{cogerr.NotFound, "not_found"},
		{cogerr.Conflict, "conflict"},
		{cogerr.Validation, "validation"},
		{cogerr.Unavailable, "unavailable"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.want {
			t.Errorf("Kind(%d).String() = %q, want %q", c.kind, got, c.want)
		}
	}
}

// TestUnknownKindReadsAsInternal covers String's default arm — the one branch
// the exhaustive linter cannot police, since it fires on a value outside the
// declared set rather than a missing case.
func TestUnknownKindReadsAsInternal(t *testing.T) {
	if got := cogerr.Kind(99).String(); got != "internal" {
		t.Fatalf("unknown Kind reads as %q, want the safe default %q", got, "internal")
	}
}

// TestKindAloneNeedsNoCause is the documented nil-cause case: E(op, kind, nil)
// signals a Kind with no underlying error, and must not print a bare "<nil>".
func TestKindAloneNeedsNoCause(t *testing.T) {
	err := cogerr.E("store.GetNote", cogerr.NotFound, nil)
	if got, want := err.Error(), "store.GetNote: not_found"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if err.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", err.Unwrap())
	}
}

// TestErrorCarriesOpKindAndCause — the message names the failing operation, its
// classification, and the cause, in that order.
func TestErrorCarriesOpKindAndCause(t *testing.T) {
	err := cogerr.E("store.UpsertNote", cogerr.Conflict, errors.New("duplicate id"))
	if got, want := err.Error(), "store.UpsertNote: conflict: duplicate id"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestEfFormatsItsCause — Ef is E with a formatted cause.
func TestEfFormatsItsCause(t *testing.T) {
	err := cogerr.Ef("mcpserver.requireLoopback", cogerr.Validation, "bind_address %q is not loopback", "0.0.0.0:7433")
	want := `mcpserver.requireLoopback: validation: bind_address "0.0.0.0:7433" is not loopback`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestKindOfDefaultsToInternal — an error from outside the domain carries no
// Kind, so it classifies as Internal rather than guessing.
func TestKindOfDefaultsToInternal(t *testing.T) {
	if got := cogerr.KindOf(errors.New("raw pgx failure")); got != cogerr.Internal {
		t.Fatalf("KindOf(plain error) = %v, want Internal", got)
	}
}

// TestKindSurvivesWrapping is the behavior every caller depends on: KindOf uses
// errors.As, so a Kind stays readable after a package boundary wraps the error
// again with %w. If this regressed, callers would silently see Internal and the
// MCP layer would return the wrong tool error.
func TestKindSurvivesWrapping(t *testing.T) {
	base := cogerr.E("store.GetNote", cogerr.NotFound, nil)
	wrapped := fmt.Errorf("write.Pipeline: %w", base)
	if got := cogerr.KindOf(wrapped); got != cogerr.NotFound {
		t.Fatalf("KindOf through a %%w wrap = %v, want NotFound", got)
	}
	if !cogerr.Is(wrapped, cogerr.NotFound) {
		t.Error("Is through a %w wrap = false, want true")
	}
}

// TestIsRejectsNil guards the reason Is checks err != nil before comparing:
// KindOf(nil) is Internal, so without that guard Is(nil, Internal) would report
// true and turn "no error" into an internal error.
func TestIsRejectsNil(t *testing.T) {
	if cogerr.Is(nil, cogerr.Internal) {
		t.Error("Is(nil, Internal) = true, want false — a nil error is not an error")
	}
}

// TestIsMatchesOnlyItsKind — Is is a Kind equality check, not a catch-all.
func TestIsMatchesOnlyItsKind(t *testing.T) {
	err := cogerr.E("config.Load", cogerr.Validation, errors.New("bad yaml"))
	if !cogerr.Is(err, cogerr.Validation) {
		t.Error("Is(err, Validation) = false, want true")
	}
	if cogerr.Is(err, cogerr.NotFound) {
		t.Error("Is(err, NotFound) = true, want false")
	}
}

// TestUnwrapChainReachesSentinel — Unwrap is chained, so errors.Is still finds
// a sentinel cause after it has been wrapped into the domain type.
func TestUnwrapChainReachesSentinel(t *testing.T) {
	sentinel := errors.New("connection refused")
	err := cogerr.E("embed.Embed", cogerr.Unavailable, sentinel)
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is could not reach the sentinel cause through cogerr.E")
	}

	var domain *cogerr.Error
	if !errors.As(err, &domain) || domain.Op != "embed.Embed" {
		t.Errorf("errors.As did not recover the domain error with its Op; got %+v", domain)
	}
}
