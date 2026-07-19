package auth

import (
	"regexp"

	"github.com/enqack/cognosis/internal/cogerr"
)

// LocalTokenName is the daemon's auto-minted local token, reserved from
// operator creation. EnsureLocalToken mints under exactly this name; letting an
// operator take it first made the daemon mint under a mangled one, after which
// `cognosis token revoke local` revoked the operator's token rather than the
// daemon's -- the documented remedy operating on the wrong row.
const LocalTokenName = "local"

// tokenNamePattern bounds what a token may be called.
//
// Lowercase alphanumerics plus '-' and '_', leading character alphanumeric,
// 1-32 characters. Two constraints are load-bearing rather than cosmetic:
//
//   - The name is emitted as an unquoted `token=<name>` slog attribute (see
//     NewIdentityHandler), so whitespace, '=' or quotes would break parsing of
//     the very attribute per-client attribution depends on.
//   - Lowercase-only prevents two names differing solely in case. The live-name
//     index treats `Desktop` and `desktop` as distinct; a human reading
//     `cognosis token list` does not.
var tokenNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateTokenName rejects names an operator must not use. Enforced at the CLI
// boundary rather than in store.CreateToken, because the store should not own
// auth policy and EnsureLocalToken legitimately creates the reserved name.
func ValidateTokenName(name string) error {
	const op = "auth.ValidateTokenName"
	if name == LocalTokenName {
		return cogerr.Ef(op, cogerr.Validation,
			"%q is reserved for the daemon's auto-minted local token", name)
	}
	if !tokenNamePattern.MatchString(name) {
		return cogerr.Ef(op, cogerr.Validation,
			"invalid token name %q: use 1-32 characters of a-z, 0-9, '-' or '_', "+
				"starting with a letter or digit", name)
	}
	return nil
}
