package auth

import (
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
)

func TestValidateTokenName(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
		why  string
	}{
		{"plain", "desktop", true, ""},
		{"hyphens and digits", "laptop-alice-2", true, ""},
		{"underscore", "ci_runner", true, ""},
		{"leading digit", "2nd-laptop", true, ""},
		{"max length", strings.Repeat("a", 32), true, ""},

		{"reserved", LocalTokenName, false, "reserved"},
		{"empty", "", false, "invalid"},
		{"too long", strings.Repeat("a", 33), false, "invalid"},
		{"uppercase", "Desktop", false, "invalid"},
		{"space", "my token", false, "invalid"},
		// '=' and '"' would break the unquoted token=<name> slog attribute.
		{"equals", "a=b", false, "invalid"},
		{"quote", `a"b`, false, "invalid"},
		{"leading hyphen", "-desktop", false, "invalid"},

		// Not reserved: only the exact name is. With the local-<8hex> fallback
		// removed there is no daemon mint to exempt, so this is a plain name.
		{"local-prefixed is allowed", "local-archive", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTokenName(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateTokenName(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateTokenName(%q) = nil, want an error", tc.in)
			}
			if !cogerr.Is(err, cogerr.Validation) {
				t.Fatalf("ValidateTokenName(%q) kind = %v, want Validation", tc.in, err)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("ValidateTokenName(%q) = %q, want it to mention %q", tc.in, err, tc.why)
			}
		})
	}
}

// TestGenerateMintsUUIDv7 — token ids carry the same contract as note ids
// (vault.NewNoteID): time-ordered, so ids sort lexically by creation.
func TestGenerateMintsUUIDv7(t *testing.T) {
	_, id, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if v := id.Version(); v != 7 {
		t.Fatalf("token id is v%d, want a UUIDv7 (time-ordered)", v)
	}
}

// TestParseTokenRejectsV4 pins the version check. A v4 id is what every token
// minted before this change carried, and accepting it would leave the
// time-ordering contract optional rather than enforced.
func TestParseTokenRejectsV4(t *testing.T) {
	// A well-formed token whose id is v4 (note the '4' opening the third group).
	v4 := "cog_1b4e28ba-2fa1-41d3-883f-1b4e28ba2fa1_" +
		"c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0cw"
	if _, _, ok := parseToken(v4); ok {
		t.Fatal("parseToken accepted a UUIDv4 token id")
	}
}
