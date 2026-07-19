// Package cogerr is the domain error type every package boundary wraps into.
// Raw pgx/HTTP/YAML errors never cross a package boundary; the MCP layer maps
// Kind to tool-error responses and slog logs op/kind as structured fields.
package cogerr

import (
	"errors"
	"fmt"
)

// Kind classifies an error at a package boundary.
type Kind int

const (
	Internal Kind = iota
	NotFound
	Conflict
	Validation
	Unavailable
)

func (k Kind) String() string {
	switch k {
	case Internal:
		return "internal"
	case NotFound:
		return "not_found"
	case Conflict:
		return "conflict"
	case Validation:
		return "validation"
	case Unavailable:
		return "unavailable"
	default:
		return "internal"
	}
}

// Error carries the operation that failed, its classification, and the cause.
type Error struct {
	Op   string // e.g. "store.UpsertNote"
	Kind Kind
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// E wraps err into an *Error. A nil err yields a message-less Error, which is
// valid for signalling a Kind alone (e.g. NotFound with no underlying cause).
func E(op string, kind Kind, err error) *Error {
	return &Error{Op: op, Kind: kind, Err: err}
}

// Ef is E with a formatted cause.
func Ef(op string, kind Kind, format string, args ...any) *Error {
	return &Error{Op: op, Kind: kind, Err: fmt.Errorf(format, args...)}
}

// KindOf reports the Kind of err if it is (or wraps) an *Error, else Internal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Internal
}

// Message returns err's text with the op and Kind prefixes stripped -- the
// sentence a human or an agent can act on, without the internal identifiers
// that belong in a log line.
//
// It does not decide whether the cause is safe to show. `cognosis status` runs
// locally for an operator and wants the detail; the MCP layer serves a remote
// caller and withholds Internal causes, which carry DSNs and socket paths.
// That policy stays with each caller rather than being guessed at here.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	if e.Err == nil {
		return e.Kind.String()
	}
	return e.Err.Error()
}

// Is reports whether err carries the given Kind.
func Is(err error, kind Kind) bool { return err != nil && KindOf(err) == kind }
