package cli

import (
	"context"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
)

// daemonOwnership is what a probe could establish about whether a daemon owns
// this vault's database.
type daemonOwnership int

const (
	// daemonUnknown — the question could not be answered: no DSN configured, or
	// Postgres unreachable. Not the same as "no daemon", and callers must not
	// treat it as one.
	daemonUnknown daemonOwnership = iota
	// daemonAbsent — nothing holds the instance lock, so no daemon owns this
	// database anywhere. A direct write is safe by construction.
	daemonAbsent
	// daemonPresent — some daemon holds the instance lock. It may be on another
	// host.
	daemonPresent
)

// probeDaemon reports whether a daemon owns this vault's database.
//
// It asks Postgres, not the PID lock file. `LockInstance` is an advisory lock
// held by whichever daemon owns the database, so it answers the question across
// hosts; the lock file only ever describes this machine, and store/tx.go calls
// the advisory lock "the cross-machine arbiter the local PID lockfile can never
// be". A CLI that branched on the lock file would take the direct-write path
// against a daemon running elsewhere — precisely the race it was branching to
// avoid.
//
// The probe reads `pg_locks` rather than acquiring the lock. Acquire-and-release
// looks equivalent and is not: between the two calls the CLI *is* the
// single-daemon arbiter, so a daemon booting in that window exits with "another
// cognosis daemon already owns this database" naming a process that has already
// finished. A read cannot displace anyone.
//
// It is still only a snapshot — a daemon may start between the probe and the
// caller's write. This is not a mutual-exclusion primitive, it is a "should I be
// doing this at all" check.
func probeDaemon(ctx context.Context, cfg *config.Config) daemonOwnership {
	dsn, err := store.ResolveDSN(cfg.DSN)
	if err != nil {
		return daemonUnknown
	}
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		return daemonUnknown
	}
	defer s.Close()
	return daemonOwns(ctx, s)
}

// daemonOwns is probeDaemon for a caller that already holds a store.
func daemonOwns(ctx context.Context, s *store.Store) daemonOwnership {
	held, err := s.AdvisoryHeld(ctx, store.LockInstance)
	switch {
	case err != nil:
		return daemonUnknown
	case held:
		return daemonPresent
	default:
		return daemonAbsent
	}
}

// refuseIfDaemonOwns blocks a command that mutates state a running daemon owns.
//
// These commands write Postgres rows, vault files, and in one case rewrite git
// history, all without the per-path lock the daemon's own writers share. There
// is no MCP equivalent to route them through — hard deletion and migration
// control are deliberately not on the agent-facing surface — so refusing is
// the available correctness, and it is better than the alternative of mutating
// underneath a live daemon and letting the watcher discover it afterwards.
//
// daemonUnknown is permitted rather than refused: if Postgres cannot be reached
// then the command is about to fail on its own, and turning an unreachable
// database into a confusing "a daemon owns this" message would be worse than
// the error the caller is going to get anyway.
func refuseIfDaemonOwns(ctx context.Context, s *store.Store, what string) error {
	const op = "cli"
	if daemonOwns(ctx, s) != daemonPresent {
		return nil
	}
	return cogerr.Ef(op, cogerr.Conflict,
		"a running daemon owns this database, and %s would change state underneath it. "+
			"Stop it first: cognosis stop", what)
}
