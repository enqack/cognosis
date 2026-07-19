package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Token is a bearer-token registration; only the Argon2id hash is stored.
type Token struct {
	ID         uuid.UUID
	Name       string
	Hash       string
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// CreateToken registers a token hash under a unique name. The id comes from
// the caller — it is embedded in the plaintext token for O(1) verification
// lookup, so the row must carry exactly that id.
//
// Only a unique violation (a live token already holds the name, or the id is
// taken) is a Conflict; anything else — connection loss, cancellation — is
// Internal. Callers branch on the distinction: EnsureLocalToken's "revoke and
// restart" remedy is correct advice only for the former.
func (s *Store) CreateToken(ctx context.Context, id uuid.UUID, name, hash string) error {
	const op = "store.CreateToken"
	if _, err := s.pool.Exec(ctx,
		`insert into tokens (id, name, token_hash) values ($1, $2, $3)`, id, name, hash); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return cogerr.E(op, cogerr.Conflict, err)
		}
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// GetTokenByID fetches one token row — the synchronous per-request check.
// Revocation is read live here on every call, deliberately: no cache, no
// revoked-token window.
func (s *Store) GetTokenByID(ctx context.Context, id uuid.UUID) (Token, error) {
	const op = "store.GetTokenByID"
	var t Token
	err := s.pool.QueryRow(ctx, `
		select id, name, token_hash, created_at, revoked_at, last_used_at
		from tokens where id = $1`, id).Scan(
		&t.ID, &t.Name, &t.Hash, &t.CreatedAt, &t.RevokedAt, &t.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, cogerr.Ef(op, cogerr.NotFound, "unknown token")
	}
	if err != nil {
		return Token{}, cogerr.E(op, cogerr.Internal, err)
	}
	return t, nil
}

// RevokeToken marks a token revoked, effective on the next request.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	const op = "store.RevokeToken"
	tag, err := s.pool.Exec(ctx,
		`update tokens set revoked_at = now() where name = $1 and revoked_at is null`, name)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if tag.RowsAffected() == 0 {
		return cogerr.Ef(op, cogerr.NotFound, "no live token named %q", name)
	}
	return nil
}

// ListTokens returns every token's metadata — names and timestamps, never
// secrets (only hashes are stored anyway).
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	const op = "store.ListTokens"
	rows, err := s.pool.Query(ctx, `
		select id, name, token_hash, created_at, revoked_at, last_used_at
		from tokens order by created_at`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &t.CreatedAt, &t.RevokedAt, &t.LastUsedAt); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// prunableTokens is the single definition of "safe to delete": revoked, and no
// audit row points at it. Shared by PrunableTokens and PruneRevokedTokens so a
// dry-run can never preview a different set than the delete performs — a
// preview that can drift from its action is worse than no preview.
//
// Referenced tokens are kept deliberately. audit_log joins to tokens.name at
// read time and its FK is NO ACTION, so deleting a referenced row would error
// rather than dangle — this predicate means the error is never reached, and the
// FK stays as the backstop that would turn a bug here into a failure rather
// than a silently broken join.
const prunableTokens = `revoked_at is not null
	and not exists (select 1 from audit_log a where a.token_id = tokens.id)`

// PrunableTokens names the tokens PruneRevokedTokens would delete.
func (s *Store) PrunableTokens(ctx context.Context) ([]string, error) {
	const op = "store.PrunableTokens"
	rows, err := s.pool.Query(ctx,
		`select name from tokens where `+prunableTokens+` order by name`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	return scanNames(op, rows)
}

// PruneRevokedTokens deletes revoked tokens nothing in audit_log references and
// returns their names. Live tokens are never touched.
func (s *Store) PruneRevokedTokens(ctx context.Context) ([]string, error) {
	const op = "store.PruneRevokedTokens"
	rows, err := s.pool.Query(ctx,
		`delete from tokens where `+prunableTokens+` returning name`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	names, err := scanNames(op, rows)
	if err != nil {
		return nil, err
	}
	// DELETE ... RETURNING admits no ORDER BY; sort here so output is
	// deterministic and comparable against PrunableTokens.
	sort.Strings(names)
	return names, nil
}

// CountRevokedTokens counts revoked rows, so prune can report how many it kept
// — which is the answer to "why is my revoked token still listed".
func (s *Store) CountRevokedTokens(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from tokens where revoked_at is not null`).Scan(&n); err != nil {
		return 0, cogerr.E("store.CountRevokedTokens", cogerr.Internal, err)
	}
	return n, nil
}

func scanNames(op string, rows pgx.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, name)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// CountLiveTokens backs the zero-config auto-token decision at startup.
func (s *Store) CountLiveTokens(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from tokens where revoked_at is null`).Scan(&n); err != nil {
		return 0, cogerr.E("store.CountLiveTokens", cogerr.Internal, err)
	}
	return n, nil
}

// TouchToken bumps last_used_at; best-effort and debounced. The auth path calls
// this on every authorized request, so the UPDATE is conditional — it only
// writes when the recorded time is stale (older than ~5 minutes) — turning a
// read-heavy hot path from a write-per-request into an occasional write. The
// metric stays "roughly last used", which is all it is for.
func (s *Store) TouchToken(ctx context.Context, id uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `
		update tokens set last_used_at = now()
		where id = $1
		  and (last_used_at is null or last_used_at < now() - interval '5 minutes')`, id)
}

// AppendAudit records one tool call. args_summary must already be redacted by
// the caller — this layer never sees full note content.
func (s *Store) AppendAudit(ctx context.Context, tokenID *uuid.UUID, tool, project, argsSummary string, success bool, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		insert into audit_log (token_id, tool_name, project, args_summary, success, error)
		values ($1,$2,$3,$4,$5,$6)`, tokenID, tool, project, argsSummary, success, errMsg)
	if err != nil {
		return cogerr.E("store.AppendAudit", cogerr.Internal, err)
	}
	return nil
}
