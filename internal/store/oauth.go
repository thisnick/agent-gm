package store

// Persistence for the four OAuth tables of migration 0006: `oauth_clients`,
// `enrollment_codes`, `authorization_requests` and `authorization_codes`
// (spec sections 4.2, 9.3, 9.4, 9.5, 9.6).
//
// Like authz.go, nothing here makes a policy decision. Which redirect URIs
// are acceptable, what a generic invalid-code message says, when a scope
// selection is a widening and how a PKCE verifier is checked all live in
// `internal/oauth`. What this file insists on is the same thing authz.go
// does: atomicity where the spec requires it. Consuming an authorization code
// and minting the tokens it produces, and revoking those tokens when the code
// is replayed, are single decisions, so they compose inside one AuthzTx.
//
// **Only hashes reach a column.** No enrollment code, no authorization code,
// no cookie handle and no form token is ever stored as its value.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Authorization request statuses. A request moves pending -> approved |
// denied, and an approved one moves to completed when the browser redeems it.
// A completed request can never mint a second code (spec section 9.5).
const (
	AuthRequestPending   = "pending"
	AuthRequestApproved  = "approved"
	AuthRequestDenied    = "denied"
	AuthRequestCompleted = "completed"
)

// Attempt kinds this slice adds, the `kind` half of `oauth_attempts`'
// primary key (spec sections 9.4, 9.8).
const (
	// AttemptKindEnrollmentSource counts failed enrollment-code submissions
	// per client source. Section 9.4 checks it BEFORE the submission is
	// examined, so loading a fresh authorization page does not reset it.
	AttemptKindEnrollmentSource = "enrollment_code_source"
	// AttemptKindEnrollmentContext counts them per signed context, which is
	// the other half of the same rule.
	AttemptKindEnrollmentContext = "enrollment_code_context"
	// AttemptKindOAuthToken counts unknown or invalid presented values at
	// /oauth/token's refresh grant and at /oauth/revoke (section 9.8).
	AttemptKindOAuthToken = "oauth_token"
)

// Errors this file returns. Each maps to exactly one refusal in
// `internal/oauth`, so the HTTP layer translates rather than decides.
var (
	// ErrClientNotFound is an unknown client_id. At /oauth/authorize it is a
	// 4xx that never redirects (RFC 6749 section 4.1.2.1).
	ErrClientNotFound = errors.New("oauth client not found")
	// ErrEnrollmentCodeNotFound is an unknown code. Section 9.4 requires
	// unknown, expired, revoked and consumed to be indistinguishable to the
	// browser, so this value never reaches a rendered page.
	ErrEnrollmentCodeNotFound = errors.New("enrollment code not found")
	// ErrAuthorizationRequestNotFound is an unknown authorization request.
	ErrAuthorizationRequestNotFound = errors.New("authorization request not found")
	// ErrAuthorizationCodeNotFound is an unknown authorization code.
	ErrAuthorizationCodeNotFound = errors.New("authorization code not found")
)

// ---------------------------------------------------------------------------
// oauth_clients
// ---------------------------------------------------------------------------

// OAuthClient is one dynamically registered public client (spec section 9.3).
// There is no client_secret column because there is no client secret: public
// native clients only, `token_endpoint_auth_method` is always `none`.
type OAuthClient struct {
	ID                      string
	Name                    string
	RedirectURIs            []string
	GrantTypes              []string
	ResponseTypes           []string
	TokenEndpointAuthMethod string
	// MetadataJSON is the registration request as accepted, so that
	// GET-ing a registration back returns what was registered rather than a
	// reconstruction.
	MetadataJSON  string
	Source        string
	ActivatedAtMS int64
	ExpiresAtMS   int64
	CreatedAtMS   int64
}

// Activated reports whether an authorization has ever used this registration.
// An activated registration no longer expires (spec section 9.3).
func (c OAuthClient) Activated() bool { return c.ActivatedAtMS != 0 }

const oauthClientColumns = `id, client_name, redirect_uris, grant_types, response_types,
	token_endpoint_auth_method, metadata_json, source, activated_at_ms, expires_at_ms, created_at_ms`

func scanOAuthClient(sc rowScanner) (OAuthClient, error) {
	var c OAuthClient
	var name, source sql.NullString
	var redirects, grants, responses string
	var activated, expires sql.NullInt64
	err := sc.Scan(&c.ID, &name, &redirects, &grants, &responses,
		&c.TokenEndpointAuthMethod, &c.MetadataJSON, &source,
		&activated, &expires, &c.CreatedAtMS)
	if err != nil {
		return c, err
	}
	c.Name = name.String
	c.Source = source.String
	c.ActivatedAtMS = activated.Int64
	c.ExpiresAtMS = expires.Int64
	if err := json.Unmarshal([]byte(redirects), &c.RedirectURIs); err != nil {
		return c, err
	}
	c.GrantTypes = splitList(grants)
	c.ResponseTypes = splitList(responses)
	return c, nil
}

// OAuthClientByID reads one registration, read-only.
func (s *Store) OAuthClientByID(ctx context.Context, id string) (OAuthClient, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+oauthClientColumns+` FROM oauth_clients WHERE id = ?`, id)
	c, err := scanOAuthClient(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrClientNotFound
	}
	return c, err
}

// CountRegistrationsSince is the durable half of section 9.3's "20 per source
// per hour". Counting rows is exact and survives a restart, which an
// in-memory bucket would not -- and a registration is a durable object, so
// forgiving it on restart would be forgiving something that is still there.
func (s *Store) CountRegistrationsSince(ctx context.Context, source string, sinceMS int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oauth_clients WHERE source = ? AND created_at_ms >= ?`,
		source, sinceMS).Scan(&n)
	return n, err
}

// CreateOAuthClient inserts one registration inside a transaction.
func (t *AuthzTx) CreateOAuthClient(c OAuthClient) error {
	redirects, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	if c.CreatedAtMS == 0 {
		c.CreatedAtMS = t.nowMS
	}
	_, err = t.tx.ExecContext(t.ctx, `
		INSERT INTO oauth_clients (`+oauthClientColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, nullString(c.Name), string(redirects), joinList(c.GrantTypes), joinList(c.ResponseTypes),
		c.TokenEndpointAuthMethod, c.MetadataJSON, nullString(c.Source),
		nullInt(c.ActivatedAtMS), nullInt(c.ExpiresAtMS), c.CreatedAtMS)
	return err
}

// OAuthClient re-reads one registration inside the transaction.
func (t *AuthzTx) OAuthClient(id string) (OAuthClient, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+oauthClientColumns+` FROM oauth_clients WHERE id = ?`, id)
	c, err := scanOAuthClient(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrClientNotFound
	}
	return c, err
}

// ActivateOAuthClient marks a registration as used by an authorization, which
// is what stops the 24-hour sweep from removing it (spec section 9.3).
func (t *AuthzTx) ActivateOAuthClient(id string) error {
	_, err := t.tx.ExecContext(t.ctx,
		`UPDATE oauth_clients SET activated_at_ms = COALESCE(activated_at_ms, ?), expires_at_ms = NULL
		 WHERE id = ?`, t.nowMS, id)
	return err
}

// ExpiredUnreferencedClients lists registrations the 60-second maintenance
// pass may remove: expired, never activated, and named by no authorization
// request. "Unreferenced" is checked in SQL rather than in Go so that a
// request created between the list and the delete cannot be orphaned.
func (t *AuthzTx) ExpiredUnreferencedClients() ([]string, error) {
	rows, err := t.tx.QueryContext(t.ctx, `
		SELECT id FROM oauth_clients
		 WHERE activated_at_ms IS NULL
		   AND expires_at_ms IS NOT NULL AND expires_at_ms <= ?
		   AND id NOT IN (SELECT client_id FROM authorization_requests)
		 ORDER BY id`, t.nowMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteOAuthClient removes one registration.
func (t *AuthzTx) DeleteOAuthClient(id string) error {
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM oauth_clients WHERE id = ?`, id)
	return err
}

// ---------------------------------------------------------------------------
// enrollment_codes
// ---------------------------------------------------------------------------

// EnrollmentCode is one owner-issued code (spec section 9.5). `Scopes` is the
// ceiling it caps a grant at, never a set of accounts: enrollment codes carry
// no account dimension, consistent with D29.
type EnrollmentCode struct {
	ID            string
	CodeHash      string
	Label         string
	Scopes        string
	ExpiresAtMS   int64
	ConsumedAtMS  int64
	ConsumedBy    string
	RevokedAtMS   int64
	RevokedReason string
	CreatedAtMS   int64
	UpdatedAtMS   int64
}

// Revoked reports whether the owner revoked this code.
func (e EnrollmentCode) Revoked() bool { return e.RevokedAtMS != 0 }

// Consumed reports whether the code has already been redeemed.
func (e EnrollmentCode) Consumed() bool { return e.ConsumedAtMS != 0 }

const enrollmentCodeColumns = `id, code_hash, label, scopes, expires_at_ms,
	consumed_at_ms, consumed_by, revoked_at_ms, revoked_reason, created_at_ms, updated_at_ms`

func scanEnrollmentCode(sc rowScanner) (EnrollmentCode, error) {
	var e EnrollmentCode
	var consumedBy, reason sql.NullString
	var consumed, revoked sql.NullInt64
	err := sc.Scan(&e.ID, &e.CodeHash, &e.Label, &e.Scopes, &e.ExpiresAtMS,
		&consumed, &consumedBy, &revoked, &reason, &e.CreatedAtMS, &e.UpdatedAtMS)
	if err != nil {
		return e, err
	}
	e.ConsumedAtMS = consumed.Int64
	e.ConsumedBy = consumedBy.String
	e.RevokedAtMS = revoked.Int64
	e.RevokedReason = reason.String
	return e, nil
}

// EnrollmentCodeByHash resolves a presented code, read-only and BEFORE any
// write transaction opens (spec section 9.8's rule, which applies to every
// presented credential and not only to a refresh token).
func (s *Store) EnrollmentCodeByHash(ctx context.Context, hash string) (EnrollmentCode, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+enrollmentCodeColumns+` FROM enrollment_codes WHERE code_hash = ?`, hash)
	e, err := scanEnrollmentCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrEnrollmentCodeNotFound
	}
	return e, err
}

// EnrollmentCodeByID reads one code row by its ID, for the admin listing.
func (s *Store) EnrollmentCodeByID(ctx context.Context, id string) (EnrollmentCode, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+enrollmentCodeColumns+` FROM enrollment_codes WHERE id = ?`, id)
	e, err := scanEnrollmentCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrEnrollmentCodeNotFound
	}
	return e, err
}

// EnrollmentCodes lists every code, newest first. The value is not in any of
// them: `data.code` is returned exactly once, at creation.
func (s *Store) EnrollmentCodes(ctx context.Context) ([]EnrollmentCode, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+enrollmentCodeColumns+` FROM enrollment_codes ORDER BY created_at_ms DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EnrollmentCode
	for rows.Next() {
		e, err := scanEnrollmentCode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateEnrollmentCode inserts one code row.
func (t *AuthzTx) CreateEnrollmentCode(e EnrollmentCode) error {
	if e.CreatedAtMS == 0 {
		e.CreatedAtMS = t.nowMS
	}
	e.UpdatedAtMS = t.nowMS
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO enrollment_codes (`+enrollmentCodeColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.CodeHash, e.Label, e.Scopes, e.ExpiresAtMS,
		nullInt(e.ConsumedAtMS), nullString(e.ConsumedBy),
		nullInt(e.RevokedAtMS), nullString(e.RevokedReason), e.CreatedAtMS, e.UpdatedAtMS)
	return err
}

// EnrollmentCode re-reads one code row inside the transaction.
func (t *AuthzTx) EnrollmentCode(id string) (EnrollmentCode, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+enrollmentCodeColumns+` FROM enrollment_codes WHERE id = ?`, id)
	e, err := scanEnrollmentCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrEnrollmentCodeNotFound
	}
	return e, err
}

// ConsumeEnrollmentCode marks a code redeemed by one authorization request.
// It is conditional on the code still being unconsumed, so two submissions
// racing cannot both redeem it: the loser sees zero rows affected.
func (t *AuthzTx) ConsumeEnrollmentCode(id, requestID string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `
		UPDATE enrollment_codes
		   SET consumed_at_ms = ?, consumed_by = ?, updated_at_ms = ?
		 WHERE id = ? AND consumed_at_ms IS NULL AND revoked_at_ms IS NULL`,
		t.nowMS, requestID, t.nowMS, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RevokeEnrollmentCode revokes a code, reporting whether this call was the
// one that did it. Repeating a revocation answers 200 with `revoked: false`
// (spec section 9.5), which is why the boolean is returned rather than an
// error.
func (t *AuthzTx) RevokeEnrollmentCode(id, reason string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `
		UPDATE enrollment_codes
		   SET revoked_at_ms = ?, revoked_reason = ?, updated_at_ms = ?
		 WHERE id = ? AND revoked_at_ms IS NULL`,
		t.nowMS, nullString(reason), t.nowMS, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---------------------------------------------------------------------------
// authorization_requests
// ---------------------------------------------------------------------------

// AuthorizationRequest is one pending owner decision (spec section 9.5).
//
// HandleHash and FormTokenHash are hashes of the two browser secrets: the
// handle carried in the signed `agm_oauth_context` cookie, and the anti-CSRF
// token the waiting page's form carries. Neither value is stored.
type AuthorizationRequest struct {
	ID                  string
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	RequestedScopes     string
	SelectedScopes      string
	GrantedScopes       string
	EnrollmentCodeID    string
	Status              string
	DenyReason          string
	HandleHash          string
	FormTokenHash       string
	Source              string
	ExpiresAtMS         int64
	DecidedAtMS         int64
	CompletedAtMS       int64
	CreatedAtMS         int64
	UpdatedAtMS         int64
}

const authorizationRequestColumns = `id, client_id, redirect_uri, state, code_challenge,
	code_challenge_method, resource, requested_scopes, selected_scopes, granted_scopes,
	enrollment_code_id, status, deny_reason, handle_hash, form_token_hash, source,
	expires_at_ms, decided_at_ms, completed_at_ms, created_at_ms, updated_at_ms`

func scanAuthorizationRequest(sc rowScanner) (AuthorizationRequest, error) {
	var a AuthorizationRequest
	var granted, enrollment, deny, source sql.NullString
	var decided, completed sql.NullInt64
	err := sc.Scan(&a.ID, &a.ClientID, &a.RedirectURI, &a.State, &a.CodeChallenge,
		&a.CodeChallengeMethod, &a.Resource, &a.RequestedScopes, &a.SelectedScopes, &granted,
		&enrollment, &a.Status, &deny, &a.HandleHash, &a.FormTokenHash, &source,
		&a.ExpiresAtMS, &decided, &completed, &a.CreatedAtMS, &a.UpdatedAtMS)
	if err != nil {
		return a, err
	}
	a.GrantedScopes = granted.String
	a.EnrollmentCodeID = enrollment.String
	a.DenyReason = deny.String
	a.Source = source.String
	a.DecidedAtMS = decided.Int64
	a.CompletedAtMS = completed.Int64
	return a, nil
}

// AuthorizationRequestByID reads one pending request, read-only.
func (s *Store) AuthorizationRequestByID(ctx context.Context, id string) (AuthorizationRequest, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+authorizationRequestColumns+` FROM authorization_requests WHERE id = ?`, id)
	a, err := scanAuthorizationRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAuthorizationRequestNotFound
	}
	return a, err
}

// AuthorizationRequests lists requests, optionally filtered by status,
// newest first.
func (s *Store) AuthorizationRequests(ctx context.Context, status string) ([]AuthorizationRequest, error) {
	query := `SELECT ` + authorizationRequestColumns + ` FROM authorization_requests`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at_ms DESC, id DESC`
	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuthorizationRequest
	for rows.Next() {
		a, err := scanAuthorizationRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAuthorizationRequest inserts one pending request.
func (t *AuthzTx) CreateAuthorizationRequest(a AuthorizationRequest) error {
	if a.CreatedAtMS == 0 {
		a.CreatedAtMS = t.nowMS
	}
	a.UpdatedAtMS = t.nowMS
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO authorization_requests (`+authorizationRequestColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ClientID, a.RedirectURI, a.State, a.CodeChallenge,
		a.CodeChallengeMethod, a.Resource, a.RequestedScopes, a.SelectedScopes,
		nullString(a.GrantedScopes), nullString(a.EnrollmentCodeID), a.Status,
		nullString(a.DenyReason), a.HandleHash, a.FormTokenHash, nullString(a.Source),
		a.ExpiresAtMS, nullInt(a.DecidedAtMS), nullInt(a.CompletedAtMS),
		a.CreatedAtMS, a.UpdatedAtMS)
	return err
}

// AuthorizationRequest re-reads one request inside the transaction, which is
// what makes an approval and a completion single atomic decisions rather than
// read-then-write races.
func (t *AuthzTx) AuthorizationRequest(id string) (AuthorizationRequest, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+authorizationRequestColumns+` FROM authorization_requests WHERE id = ?`, id)
	a, err := scanAuthorizationRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAuthorizationRequestNotFound
	}
	return a, err
}

// DecideAuthorizationRequest moves a request from pending to approved or
// denied, conditional on it still being pending. A no-longer-pending request
// is `idempotency_conflict` at the admin route, and this is where that is
// decided: zero rows affected means somebody got there first.
func (t *AuthzTx) DecideAuthorizationRequest(id, status, grantedScopes, denyReason string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `
		UPDATE authorization_requests
		   SET status = ?, granted_scopes = ?, deny_reason = ?, decided_at_ms = ?, updated_at_ms = ?
		 WHERE id = ? AND status = ?`,
		status, nullString(grantedScopes), nullString(denyReason), t.nowMS, t.nowMS,
		id, AuthRequestPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CompleteAuthorizationRequest marks an approved request completed,
// conditional on it still being approved. **A completed request can never
// mint a second code** (spec section 9.5), and this condition is the whole
// reason for that guarantee.
func (t *AuthzTx) CompleteAuthorizationRequest(id string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `
		UPDATE authorization_requests
		   SET status = ?, completed_at_ms = ?, updated_at_ms = ?
		 WHERE id = ? AND status = ?`,
		AuthRequestCompleted, t.nowMS, t.nowMS, id, AuthRequestApproved)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---------------------------------------------------------------------------
// authorization_codes
// ---------------------------------------------------------------------------

// AuthorizationCode is one issued code (spec section 9.6). Only its hash is
// stored. AuthorizationID is filled in when the code is exchanged, so that a
// replay can revoke the tokens the first exchange produced.
type AuthorizationCode struct {
	CodeHash        string
	RequestID       string
	ClientID        string
	RedirectURI     string
	CodeChallenge   string
	Scopes          string
	Resource        string
	AuthorizationID string
	ConsumedAtMS    int64
	ExpiresAtMS     int64
	CreatedAtMS     int64
}

// Consumed reports whether the code has been exchanged (or burned by a wrong
// PKCE verifier, which consumes it so a verifier cannot be guessed by
// retrying).
func (c AuthorizationCode) Consumed() bool { return c.ConsumedAtMS != 0 }

const authorizationCodeColumns = `code_hash, request_id, client_id, redirect_uri,
	code_challenge, scopes, resource, authorization_id, consumed_at_ms, expires_at_ms, created_at_ms`

func scanAuthorizationCode(sc rowScanner) (AuthorizationCode, error) {
	var c AuthorizationCode
	var authID sql.NullString
	var consumed sql.NullInt64
	err := sc.Scan(&c.CodeHash, &c.RequestID, &c.ClientID, &c.RedirectURI,
		&c.CodeChallenge, &c.Scopes, &c.Resource, &authID, &consumed,
		&c.ExpiresAtMS, &c.CreatedAtMS)
	if err != nil {
		return c, err
	}
	c.AuthorizationID = authID.String
	c.ConsumedAtMS = consumed.Int64
	return c, nil
}

// AuthorizationCodeByHash resolves a presented code read-only, before any
// write transaction opens (spec section 9.8).
func (s *Store) AuthorizationCodeByHash(ctx context.Context, hash string) (AuthorizationCode, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+authorizationCodeColumns+` FROM authorization_codes WHERE code_hash = ?`, hash)
	c, err := scanAuthorizationCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrAuthorizationCodeNotFound
	}
	return c, err
}

// CreateAuthorizationCode inserts one issued code.
func (t *AuthzTx) CreateAuthorizationCode(c AuthorizationCode) error {
	if c.CreatedAtMS == 0 {
		c.CreatedAtMS = t.nowMS
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO authorization_codes (`+authorizationCodeColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.CodeHash, c.RequestID, c.ClientID, c.RedirectURI, c.CodeChallenge,
		c.Scopes, c.Resource, nullString(c.AuthorizationID),
		nullInt(c.ConsumedAtMS), c.ExpiresAtMS, c.CreatedAtMS)
	return err
}

// AuthorizationCode re-reads one code inside the transaction.
func (t *AuthzTx) AuthorizationCode(hash string) (AuthorizationCode, error) {
	row := t.tx.QueryRowContext(t.ctx,
		`SELECT `+authorizationCodeColumns+` FROM authorization_codes WHERE code_hash = ?`, hash)
	c, err := scanAuthorizationCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrAuthorizationCodeNotFound
	}
	return c, err
}

// ConsumeAuthorizationCode marks a code spent, conditional on it being
// unspent. The boolean is the race answer: false means somebody exchanged it
// first, which is the replay section 9.6 punishes.
func (t *AuthzTx) ConsumeAuthorizationCode(hash, authorizationID string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `
		UPDATE authorization_codes
		   SET consumed_at_ms = ?, authorization_id = COALESCE(?, authorization_id)
		 WHERE code_hash = ? AND consumed_at_ms IS NULL`,
		t.nowMS, nullString(authorizationID), hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---------------------------------------------------------------------------
// Authorization listing, for GET /v1/admin/authorizations.
// ---------------------------------------------------------------------------

// Authorizations lists every authorization, newest first, optionally only the
// live ones. It carries no token and no hash: an owner auditing their grants
// sees IDs, kinds, clients, scopes and times.
func (s *Store) Authorizations(ctx context.Context, includeRevoked bool) ([]Authorization, error) {
	query := `SELECT ` + authorizationColumns + ` FROM authorizations`
	if !includeRevoked {
		query += ` WHERE revoked_at_ms IS NULL`
	}
	query += ` ORDER BY created_at_ms DESC, id DESC`
	rows, err := s.read.QueryContext(ctx, query)
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

func joinList(v []string) string { return strings.Join(v, " ") }

func splitList(s string) []string { return strings.Fields(s) }

// OAuthClients lists every registration, newest first.
func (s *Store) OAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+oauthClientColumns+` FROM oauth_clients ORDER BY created_at_ms DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OAuthClient
	for rows.Next() {
		c, err := scanOAuthClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AuthorizationsForClient lists the live authorizations one registration
// holds, which is what a client revocation has to take with it.
func (s *Store) AuthorizationsForClient(ctx context.Context, clientID string) ([]Authorization, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+authorizationColumns+` FROM authorizations
		  WHERE client_id = ? AND revoked_at_ms IS NULL
		  ORDER BY created_at_ms DESC, id DESC`, clientID)
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

// AuthorizationIDForRequest returns the authorization one approved request
// eventually minted, or "" if the code has not been exchanged yet.
//
// It is a join through `authorization_codes` rather than a column on
// `authorization_requests`, because the authorization does not exist when the
// request is approved: the owner approves, the browser completes, and only
// then does the token endpoint mint anything. A column would have to be
// written by the exchange anyway, and a nullable column that is null for a
// perfectly ordinary reason invites a reader to think something went wrong.
func (s *Store) AuthorizationIDForRequest(ctx context.Context, requestID string) (string, error) {
	var id sql.NullString
	err := s.read.QueryRowContext(ctx,
		`SELECT authorization_id FROM authorization_codes
		  WHERE request_id = ? AND authorization_id IS NOT NULL
		  ORDER BY created_at_ms DESC LIMIT 1`, requestID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.String, err
}
