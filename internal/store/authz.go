package store

// Persistence for the three credential tables of migration 0003:
// `authorizations`, `tokens` and `oauth_attempts` (spec sections 4.2, 9.6,
// 9.8, 12.3).
//
// Nothing in this file makes a policy decision. It stores what it is told and
// reads back what it stored; which scopes may be granted, when a refresh is a
// widening, how long a cooldown lasts and what an audit row says all live in
// `internal/authz`. The one thing this file does insist on is atomicity: a
// rotation, its audit record and the family revocation that reuse triggers
// have to commit together (spec section 9.6), so the transactional entry
// point AuthzTx exists to let the policy layer compose them without ever
// holding a half-applied rotation.
//
// Only hashes are stored. No token value, and no admin secret, is ever
// written to a column here (spec sections 9.6, 12.1).

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Authorization kinds. Slice 2 mints exactly one; Slice 3 adds a second token
// SOURCE under the same shape, not a second security model.
const (
	// AuthKindAdminBootstrap is a session minted by presenting
	// AGENT_GM_ADMIN_SECRET at POST /v1/auth/admin-session (spec section 9.7).
	AuthKindAdminBootstrap = "admin_bootstrap"
	// AuthKindOAuth is the Slice 3 grant.
	AuthKindOAuth = "oauth"
)

// Token kinds.
const (
	TokenKindAccess  = "access"
	TokenKindRefresh = "refresh"
)

// Attempt kinds, the `kind` half of oauth_attempts' primary key.
const (
	AttemptKindAdminSecret = "admin_secret"
	AttemptKindRefresh     = "refresh_token"
)

// AttemptGlobalSource is the reserved `source` value for the server-wide half
// of a per-source limiter. It is not a valid client source: a resolved source
// is an IP address and is never the empty string nor this sentinel (spec
// section 12.3).
const AttemptGlobalSource = "*global*"

// Authorization is one row of `authorizations`.
//
// Scopes is what the authorization currently carries; MintedScopes is what it
// was minted with and never changes. A refresh may only narrow relative to
// MintedScopes, never back up to it (spec section 9.6) -- without the second
// column a narrowed session would be one refresh away from full privilege.
type Authorization struct {
	ID     string
	Kind   string
	Client string
	// Scopes and MintedScopes are stored space-joined, in the caller's order.
	Scopes       string
	MintedScopes string
	// SecretGeneration identifies which AGENT_GM_ADMIN_SECRET minted this
	// authorization. It is a hash, never the secret (spec section 12.1).
	SecretGeneration string
	Source           string
	RevokedAtMS      int64
	ExpiresAtMS      int64
	CreatedAtMS      int64
	UpdatedAtMS      int64
}

// Revoked reports whether the authorization has been revoked.
func (a Authorization) Revoked() bool { return a.RevokedAtMS != 0 }

// Token is one row of `tokens`. Value is never stored: TokenHash is the
// primary key and the only representation the database ever sees.
type Token struct {
	TokenHash       string
	AuthorizationID string
	Kind            string
	FamilyID        string
	SpentAtMS       int64
	RevokedAtMS     int64
	ExpiresAtMS     int64
	CreatedAtMS     int64
}

// Spent reports whether this token has already been redeemed. Presenting a
// spent refresh token is reuse, and reuse revokes the family (spec section
// 9.6).
func (t Token) Spent() bool { return t.SpentAtMS != 0 }

// Revoked reports whether this token was revoked.
func (t Token) Revoked() bool { return t.RevokedAtMS != 0 }

// Attempt is one row of `oauth_attempts`: the durable half of a
// credential-failure limiter (spec sections 9.8, 12.3). It is durable
// precisely because it is the limit an attacker would restart-cycle to reset.
type Attempt struct {
	Kind            string
	Source          string
	WindowStartMS   int64
	Failures        int
	CooldownSteps   int
	CooldownUntilMS int64
	UpdatedAtMS     int64
}

// ErrAuthorizationNotFound is returned when no authorization has that ID.
var ErrAuthorizationNotFound = errors.New("authorization not found")

// ErrTokenNotFound is returned when no token has that hash.
var ErrTokenNotFound = errors.New("token not found")

const authorizationColumns = `id, kind, client_id, scopes, minted_scopes,
	secret_generation, source, revoked_at_ms, expires_at_ms, created_at_ms, updated_at_ms`

const tokenColumns = `token_hash, authorization_id, kind, family_id,
	spent_at_ms, revoked_at_ms, expires_at_ms, created_at_ms`

type rowScanner interface{ Scan(...any) error }

func scanAuthorization(sc rowScanner) (Authorization, error) {
	var a Authorization
	var client, generation, source sql.NullString
	var revoked, expires sql.NullInt64
	err := sc.Scan(&a.ID, &a.Kind, &client, &a.Scopes, &a.MintedScopes,
		&generation, &source, &revoked, &expires, &a.CreatedAtMS, &a.UpdatedAtMS)
	if err != nil {
		return a, err
	}
	a.Client = client.String
	a.SecretGeneration = generation.String
	a.Source = source.String
	a.RevokedAtMS = revoked.Int64
	a.ExpiresAtMS = expires.Int64
	return a, nil
}

func scanToken(sc rowScanner) (Token, error) {
	var t Token
	var spent, revoked sql.NullInt64
	err := sc.Scan(&t.TokenHash, &t.AuthorizationID, &t.Kind, &t.FamilyID,
		&spent, &revoked, &t.ExpiresAtMS, &t.CreatedAtMS)
	if err != nil {
		return t, err
	}
	t.SpentAtMS = spent.Int64
	t.RevokedAtMS = revoked.Int64
	return t, nil
}

// ---------------------------------------------------------------------------
// Read-only reads.
//
// Spec section 9.8: the presented token is looked up read-only BEFORE any
// write transaction opens, so a caller presenting a value that belongs to
// nobody cannot take the single writer's lock and queue every other writer
// behind itself. These four methods are that lookup; they run on the
// read-only pool and never touch the writer goroutine.
// ---------------------------------------------------------------------------

// Authorization reads one authorization by ID.
func (s *Store) Authorization(ctx context.Context, id string) (Authorization, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+authorizationColumns+` FROM authorizations WHERE id = ?`, id)
	a, err := scanAuthorization(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAuthorizationNotFound
	}
	return a, err
}

// TokenByHash reads one token by its hash, read-only.
func (s *Store) TokenByHash(ctx context.Context, hash string) (Token, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM tokens WHERE token_hash = ?`, hash)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrTokenNotFound
	}
	return t, err
}

// AuthorizationsByKind lists every authorization of one kind, newest first.
func (s *Store) AuthorizationsByKind(ctx context.Context, kind string) ([]Authorization, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+authorizationColumns+` FROM authorizations WHERE kind = ?
		  ORDER BY created_at_ms DESC, id ASC`, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Authorization
	for rows.Next() {
		a, err := scanAuthorization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttemptRow reads one oauth_attempts row. The bool reports whether it exists;
// an absent row is not an error, it is a source that has never failed.
func (s *Store) AttemptRow(ctx context.Context, kind, source string) (Attempt, bool, error) {
	a := Attempt{Kind: kind, Source: source}
	var cooldown sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT window_start_ms, failures, cooldown_steps, cooldown_until_ms, updated_at_ms
		   FROM oauth_attempts WHERE kind = ? AND source = ?`, kind, source).
		Scan(&a.WindowStartMS, &a.Failures, &a.CooldownSteps, &cooldown, &a.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{Kind: kind, Source: source}, false, nil
	}
	if err != nil {
		return a, false, err
	}
	a.CooldownUntilMS = cooldown.Int64
	return a, true, nil
}

// ---------------------------------------------------------------------------
// Writes.
// ---------------------------------------------------------------------------

// UpdateAttempt is the read-modify-write of one oauth_attempts row, inside one
// transaction on the single writer. fn receives the current row (zero-valued
// and with Kind/Source filled in if there is none) and returns the row to
// store. Returning the row unchanged still writes it, so a caller that only
// wants to read should use AttemptRow.
func (s *Store) UpdateAttempt(ctx context.Context, kind, source string, fn func(Attempt) Attempt) (Attempt, error) {
	var out Attempt
	err := s.Write(ctx, func(tx *sql.Tx) error {
		cur, err := attemptTx(ctx, tx, kind, source)
		if err != nil {
			return err
		}
		out = fn(cur)
		out.Kind, out.Source = kind, source
		out.UpdatedAtMS = s.clock.Now().UnixMilli()
		return putAttemptTx(ctx, tx, out)
	})
	return out, err
}

func attemptTx(ctx context.Context, tx *sql.Tx, kind, source string) (Attempt, error) {
	a := Attempt{Kind: kind, Source: source}
	var cooldown sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT window_start_ms, failures, cooldown_steps, cooldown_until_ms, updated_at_ms
		   FROM oauth_attempts WHERE kind = ? AND source = ?`, kind, source).
		Scan(&a.WindowStartMS, &a.Failures, &a.CooldownSteps, &cooldown, &a.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{Kind: kind, Source: source}, nil
	}
	if err != nil {
		return a, err
	}
	a.CooldownUntilMS = cooldown.Int64
	return a, nil
}

func putAttemptTx(ctx context.Context, tx *sql.Tx, a Attempt) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO oauth_attempts
		    (kind, source, window_start_ms, failures, cooldown_steps, cooldown_until_ms, updated_at_ms)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(kind, source) DO UPDATE SET
		    window_start_ms   = excluded.window_start_ms,
		    failures          = excluded.failures,
		    cooldown_steps    = excluded.cooldown_steps,
		    cooldown_until_ms = excluded.cooldown_until_ms,
		    updated_at_ms     = excluded.updated_at_ms`,
		a.Kind, a.Source, a.WindowStartMS, a.Failures, a.CooldownSteps,
		nullInt(a.CooldownUntilMS), a.UpdatedAtMS)
	return err
}

// AuthzTx runs fn inside one transaction on the single writer, handing it a
// credential-scoped view of that transaction. Everything fn does commits
// together or not at all: this is how a rotation, its audit record and a
// family revocation become one atomic decision (spec section 9.6).
func (s *Store) AuthzTx(ctx context.Context, fn func(*AuthzTx) error) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		return fn(&AuthzTx{ctx: ctx, tx: tx, nowMS: s.clock.Now().UnixMilli()})
	})
}

// AuthzTx is the credential surface of one open write transaction.
type AuthzTx struct {
	ctx   context.Context
	tx    *sql.Tx
	nowMS int64
}

// NowMS is the injected clock's reading when the transaction opened. Every
// timeout in the credential path is measured against it, never against
// time.Now (spec section 13.1).
func (t *AuthzTx) NowMS() int64 { return t.nowMS }

// CreateAuthorization inserts one authorization row.
func (t *AuthzTx) CreateAuthorization(a Authorization) error {
	if a.CreatedAtMS == 0 {
		a.CreatedAtMS = t.nowMS
	}
	a.UpdatedAtMS = t.nowMS
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO authorizations (`+authorizationColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Kind, nullString(a.Client), a.Scopes, a.MintedScopes,
		nullString(a.SecretGeneration), nullString(a.Source),
		nullInt(a.RevokedAtMS), nullInt(a.ExpiresAtMS), a.CreatedAtMS, a.UpdatedAtMS)
	return err
}

// Authorization re-reads one authorization inside the transaction. Spec
// section 9.8: when the token does exist, the transaction re-reads the row
// before cascading, so revocation stays a single atomic decision.
func (t *AuthzTx) Authorization(id string) (Authorization, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+authorizationColumns+` FROM authorizations WHERE id = ?`, id)
	a, err := scanAuthorization(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAuthorizationNotFound
	}
	return a, err
}

// SetAuthorizationScopes narrows (or otherwise rewrites) the current scopes.
// minted_scopes is deliberately not writable: it is what the session was
// minted with and a refresh is measured against it forever.
func (t *AuthzTx) SetAuthorizationScopes(id, scopes string) error {
	res, err := t.tx.ExecContext(t.ctx,
		`UPDATE authorizations SET scopes = ?, updated_at_ms = ? WHERE id = ?`,
		scopes, t.nowMS, id)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// RevokeAuthorization marks the authorization revoked. It is idempotent: an
// already-revoked row keeps its first revocation time.
func (t *AuthzTx) RevokeAuthorization(id string) error {
	_, err := t.tx.ExecContext(t.ctx,
		`UPDATE authorizations
		    SET revoked_at_ms = COALESCE(revoked_at_ms, ?), updated_at_ms = ?
		  WHERE id = ?`, t.nowMS, t.nowMS, id)
	return err
}

// InsertToken stores one token by hash. The value never reaches this layer.
func (t *AuthzTx) InsertToken(tok Token) error {
	if tok.CreatedAtMS == 0 {
		tok.CreatedAtMS = t.nowMS
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO tokens (`+tokenColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		tok.TokenHash, tok.AuthorizationID, tok.Kind, tok.FamilyID,
		nullInt(tok.SpentAtMS), nullInt(tok.RevokedAtMS), tok.ExpiresAtMS, tok.CreatedAtMS)
	return err
}

// Token re-reads one token by hash inside the transaction.
func (t *AuthzTx) Token(hash string) (Token, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+tokenColumns+` FROM tokens WHERE token_hash = ?`, hash)
	tok, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return tok, ErrTokenNotFound
	}
	return tok, err
}

// SpendToken marks a refresh token redeemed. It fails with ErrTokenNotFound if
// the row is gone, so a caller cannot believe it rotated something that no
// longer exists.
func (t *AuthzTx) SpendToken(hash string) error {
	res, err := t.tx.ExecContext(t.ctx,
		`UPDATE tokens SET spent_at_ms = COALESCE(spent_at_ms, ?) WHERE token_hash = ?`,
		t.nowMS, hash)
	if err != nil {
		return err
	}
	if err := requireOneRow(res); err != nil {
		return ErrTokenNotFound
	}
	return nil
}

// RevokeToken revokes one token.
func (t *AuthzTx) RevokeToken(hash string) error {
	_, err := t.tx.ExecContext(t.ctx,
		`UPDATE tokens SET revoked_at_ms = COALESCE(revoked_at_ms, ?) WHERE token_hash = ?`,
		t.nowMS, hash)
	return err
}

// RevokeTokenFamily revokes every token of one rotation family and reports how
// many rows it touched. Reuse of a spent refresh token revokes the whole
// family and its authorization (spec section 9.6).
func (t *AuthzTx) RevokeTokenFamily(familyID string) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx,
		`UPDATE tokens SET revoked_at_ms = COALESCE(revoked_at_ms, ?)
		  WHERE family_id = ? AND revoked_at_ms IS NULL`, t.nowMS, familyID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeTokensForAuthorization revokes every token of one authorization.
func (t *AuthzTx) RevokeTokensForAuthorization(authorizationID string) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx,
		`UPDATE tokens SET revoked_at_ms = COALESCE(revoked_at_ms, ?)
		  WHERE authorization_id = ? AND revoked_at_ms IS NULL`, t.nowMS, authorizationID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Attempt reads one oauth_attempts row inside the transaction.
func (t *AuthzTx) Attempt(kind, source string) (Attempt, error) {
	return attemptTx(t.ctx, t.tx, kind, source)
}

// PutAttempt writes one oauth_attempts row inside the transaction.
func (t *AuthzTx) PutAttempt(a Attempt) error {
	a.UpdatedAtMS = t.nowMS
	return putAttemptTx(t.ctx, t.tx, a)
}

// AppendAudit writes one audit_events row inside the transaction, so an audit
// record commits with the change it describes (spec sections 9.6, 12.4).
//
// payloadJSON is rendered by the caller: this layer has no opinion about what
// belongs in a payload beyond the schema's requirement that it be present.
// The caller is responsible for section 12.4's rule that a payload carries IDs,
// counts, codes and outcomes and never a token, a secret or a phone number.
func (t *AuthzTx) AppendAudit(kind, result, accountID, authorizationID, source, payloadJSON string) error {
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO audit_events
		    (id, kind, account_id, authorization_id, target_type, target_id,
		     result, source, payload_json, created_at_ms)
		VALUES (?,?,?,?,NULL,NULL,?,?,?,?)`,
		"aud_"+uuid.NewString(), kind, nullString(accountID), nullString(authorizationID),
		result, nullString(source), payloadJSON, t.nowMS)
	return err
}

// ---------------------------------------------------------------------------
// Startup.
// ---------------------------------------------------------------------------

// RevokeAdminAuthorizationsFromOtherSecrets revokes every unrevoked admin
// bootstrap authorization whose secret_generation is not `generation`, and
// revokes their tokens with them. It returns the IDs it revoked.
//
// This is spec section 12.1's guarantee in one call: changing
// AGENT_GM_ADMIN_SECRET revokes every previous admin bootstrap authorization
// on the next start. It runs before any listener binds, so there is no window
// in which a session minted by the retired secret answers a request.
func (s *Store) RevokeAdminAuthorizationsFromOtherSecrets(ctx context.Context, generation string) ([]string, error) {
	var revoked []string
	err := s.AuthzTx(ctx, func(t *AuthzTx) error {
		rows, err := t.tx.QueryContext(t.ctx,
			`SELECT id FROM authorizations
			  WHERE kind = ? AND revoked_at_ms IS NULL
			    AND COALESCE(secret_generation, '') <> ?`,
			AuthKindAdminBootstrap, generation)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := t.RevokeAuthorization(id); err != nil {
				return err
			}
			if _, err := t.RevokeTokensForAuthorization(id); err != nil {
				return err
			}
			if err := t.AppendAudit("auth.admin_authorization_revoked", "ok", "", id, "",
				`{"reason":"admin_secret_changed"}`); err != nil {
				return err
			}
		}
		revoked = ids
		return nil
	})
	return revoked, err
}

// AuditKinds lists the kinds of every audit row, newest first. It exists for
// tests and for GET /v1/admin/audit's callers to assert against without
// reaching for the payload.
func (s *Store) AuditKinds(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT kind FROM audit_events ORDER BY created_at_ms DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AuditPayloads returns the raw payload JSON of every audit row of one kind.
// The sentinel scan of spec section 12.2 reads this to prove no presented
// value ever reached a payload.
func (s *Store) AuditPayloads(ctx context.Context, kind string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT payload_json FROM audit_events WHERE kind = ?
		  ORDER BY created_at_ms ASC, id ASC`, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// JoinScopes renders a scope list the way the two scope columns store it.
func JoinScopes(scopes []string) string { return strings.Join(scopes, " ") }

// SplitScopes parses a stored scope column back into a list.
func SplitScopes(s string) []string { return strings.Fields(s) }

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAuthorizationNotFound
	}
	return nil
}
