package oauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
	"github.com/thisnick/agent-gm/internal/wire"
)

// The owner's half of spec section 9.5: issuing enrollment codes, and
// approving or denying an authorization request.
//
// **There is no self-service.** A connector cannot get a token unless the
// owner does two separate things -- issue a code, and approve the request --
// and the two are separate on purpose: a code that leaked still cannot mint
// anything without a person looking at the screen and saying yes.
//
// These functions live here rather than in `internal/api` because they are
// OAuth policy: the scope ceiling, the never-enrollable `admin`, the
// narrowing rule at approval. The API layer decodes, calls one of these, and
// renders. They return *apierr.Error so that the REST envelope, the exit code
// and the CLI's rendering all come out of the one taxonomy.

// EnrollmentTTLBounds are section 9.5's: a duration between one minute and 24
// hours, capped at 24 hours.
const (
	MinEnrollmentTTL = time.Minute
	MaxEnrollmentTTL = 24 * time.Hour
)

// DefaultEnrollmentCeiling is the scope ceiling a code carries when the owner
// names none: read and write, but never delete and never admin.
//
// Delete is left out deliberately. A code issued without thinking should not
// be able to grant the one permission whose mistakes cannot be undone; an
// owner who wants it says so.
var DefaultEnrollmentCeiling = []string{
	string(authz.ScopeMessagesRead),
	string(authz.ScopeMessagesWrite),
}

// EnrollmentRequest is `POST /v1/admin/enrollment-codes`.
type EnrollmentRequest struct {
	Label string
	// ExpiresIn is a duration string ("30m") or a number of seconds. Empty
	// means the `oauth.enrollment_default_ttl` setting.
	ExpiresIn string
	// Scopes REPLACES the default ceiling; AllowScopes EXTENDS it. They are
	// mutually exclusive, because a request carrying both is a request whose
	// author did not know which one they meant.
	Scopes      []string
	AllowScopes []string
}

// EnrollmentCode is one code as the owner sees it. `Code` is populated
// exactly once, by IssueEnrollmentCode, and is empty on every listing: only
// the SHA-256 is stored, so there is nothing to show later even if a route
// wanted to.
type EnrollmentCode struct {
	ID            string   `json:"id"`
	Code          string   `json:"code,omitempty"`
	Label         string   `json:"label"`
	Scopes        []string `json:"scopes"`
	ExpiresAt     *string  `json:"expires_at"`
	ConsumedAt    *string  `json:"consumed_at"`
	ConsumedBy    string   `json:"consumed_by,omitempty"`
	RevokedAt     *string  `json:"revoked_at"`
	RevokedReason string   `json:"revoked_reason,omitempty"`
	CreatedAt     *string  `json:"created_at"`
}

// IssueEnrollmentCode mints a code. `data.code` is the ONLY time the value is
// returned, and only its SHA-256 is stored (section 9.5).
func (s *Server) IssueEnrollmentCode(ctx context.Context, in EnrollmentRequest, source string) (*EnrollmentCode, *apierr.Error) {
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return nil, invalidRequest("label", "a label is required so the owner can tell codes apart")
	}
	if len(in.Scopes) > 0 && len(in.AllowScopes) > 0 {
		return nil, invalidRequest("scopes",
			"scopes and allow_scopes are mutually exclusive: scopes replaces the default ceiling, allow_scopes extends it")
	}

	ceiling, aerr := s.enrollmentCeiling(in)
	if aerr != nil {
		return nil, aerr
	}

	ttl, aerr := s.enrollmentTTL(ctx, in.ExpiresIn)
	if aerr != nil {
		return nil, aerr
	}

	value, err := NewEnrollmentCode()
	if err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the code could not be minted")
	}
	now := s.now()
	row := store.EnrollmentCode{
		ID:          store.EnrollmentCodeID(),
		CodeHash:    EnrollmentCodeHash(value),
		Label:       label,
		Scopes:      ceiling.String(),
		ExpiresAtMS: now.Add(ttl).UnixMilli(),
		CreatedAtMS: now.UnixMilli(),
	}
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		if err := t.CreateEnrollmentCode(row); err != nil {
			return err
		}
		return t.AppendAudit("enrollment.created", "ok", "", "", source, mustJSON(map[string]any{
			"enrollment_code_id": row.ID,
			"label":              row.Label,
			"scopes":             ceiling.Strings(),
			"expires_at_ms":      row.ExpiresAtMS,
		}))
	}); err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the code could not be recorded")
	}
	out := enrollmentDTO(row)
	out.Code = value
	return &out, nil
}

// enrollmentCeiling applies the `scopes` / `allow_scopes` rule and the one
// that outranks it: **`admin` can never be enrolled** (section 9.5).
func (s *Server) enrollmentCeiling(in EnrollmentRequest) (authz.ScopeSet, *apierr.Error) {
	base := DefaultEnrollmentCeiling
	if len(in.Scopes) > 0 {
		base = in.Scopes
	}
	set, err := authz.ParseScopes(base)
	if err != nil {
		return authz.ScopeSet{}, invalidRequest("scopes", err.Error())
	}
	for _, extra := range in.AllowScopes {
		more, err := authz.ParseScopes([]string{extra})
		if err != nil {
			return authz.ScopeSet{}, invalidRequest("allow_scopes", err.Error())
		}
		set = authz.NewScopeSet(append(set.List(), more.List()...)...)
	}
	if set.Empty() {
		return authz.ScopeSet{}, invalidRequest("scopes", "an empty ceiling grants nothing")
	}
	if set.Has(authz.ScopeAdmin) {
		return authz.ScopeSet{}, invalidRequest("scopes",
			"admin can never be enrolled; the only way to hold it is AGENT_GM_ADMIN_SECRET")
	}
	return set, nil
}

// enrollmentTTL parses `expires_in` as a duration string or a number of
// seconds, defaulting to `oauth.enrollment_default_ttl` and capped at 24
// hours.
func (s *Server) enrollmentTTL(ctx context.Context, raw string) (time.Duration, *apierr.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		d, err := s.cfg.Authz.SettingDuration(ctx, authz.SettingOAuthEnrollmentDefaultTTL)
		if err != nil {
			return 15 * time.Minute, nil
		}
		return d, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		secs, serr := strconv.ParseInt(raw, 10, 64)
		if serr != nil {
			return 0, invalidRequest("expires_in",
				"expires_in is a duration such as `30m` or a number of seconds")
		}
		d = time.Duration(secs) * time.Second
	}
	if d < MinEnrollmentTTL {
		return 0, invalidRequest("expires_in",
			fmt.Sprintf("expires_in must be at least %s", MinEnrollmentTTL))
	}
	if d > MaxEnrollmentTTL {
		return 0, invalidRequest("expires_in",
			fmt.Sprintf("expires_in is capped at %s", MaxEnrollmentTTL))
	}
	return d, nil
}

// ListEnrollmentCodes lists every code. No value is in the answer.
func (s *Server) ListEnrollmentCodes(ctx context.Context) ([]EnrollmentCode, *apierr.Error) {
	rows, err := s.st.EnrollmentCodes(ctx)
	if err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the codes could not be read")
	}
	out := make([]EnrollmentCode, 0, len(rows))
	for _, row := range rows {
		out = append(out, enrollmentDTO(row))
	}
	return out, nil
}

// GetEnrollmentCode shows one code row.
func (s *Server) GetEnrollmentCode(ctx context.Context, id string) (*EnrollmentCode, *apierr.Error) {
	row, err := s.st.EnrollmentCodeByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrEnrollmentCodeNotFound) {
			return nil, apierr.NotFound("enrollment code")
		}
		return nil, apierr.New(apierr.CodeInternalError, "the code could not be read")
	}
	out := enrollmentDTO(row)
	return &out, nil
}

// RevokeEnrollmentCode revokes one code. Repeating it answers 200 with
// `revoked: false` (section 9.5), which is why the boolean is the result
// rather than an error: a second revocation is not a failure, it is a
// no-longer-necessary act.
func (s *Server) RevokeEnrollmentCode(ctx context.Context, id, reason, source string) (bool, *apierr.Error) {
	if _, err := s.st.EnrollmentCodeByID(ctx, id); err != nil {
		if errors.Is(err, store.ErrEnrollmentCodeNotFound) {
			return false, apierr.NotFound("enrollment code")
		}
		return false, apierr.New(apierr.CodeInternalError, "the code could not be read")
	}
	var revoked bool
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		ok, err := t.RevokeEnrollmentCode(id, reason)
		if err != nil {
			return err
		}
		revoked = ok
		if !ok {
			return nil
		}
		return t.AppendAudit("enrollment.revoked", "ok", "", "", source, mustJSON(map[string]any{
			"enrollment_code_id": id,
			"reason":             reason,
		}))
	}); err != nil {
		return false, apierr.New(apierr.CodeInternalError, "the revocation could not be recorded")
	}
	return revoked, nil
}

func enrollmentDTO(row store.EnrollmentCode) EnrollmentCode {
	return EnrollmentCode{
		ID:            row.ID,
		Label:         row.Label,
		Scopes:        store.SplitScopes(row.Scopes),
		ExpiresAt:     msTime(row.ExpiresAtMS),
		ConsumedAt:    msTime(row.ConsumedAtMS),
		ConsumedBy:    row.ConsumedBy,
		RevokedAt:     msTime(row.RevokedAtMS),
		RevokedReason: row.RevokedReason,
		CreatedAt:     msTime(row.CreatedAtMS),
	}
}

// msTime renders a stored millisecond stamp as the section 4.4 instant every
// surface uses: RFC 3339 in UTC with exactly three fractional digits. It goes
// through internal/wire rather than marshalling a time.Time, because Go's own
// RFC3339Nano strips trailing zeros and the width would then depend on the
// value (see internal/wire's guard).
func msTime(ms int64) *string { return wire.InstantMS(ms) }

// ---------------------------------------------------------------------------
// Authorization requests: the owner's approval.
// ---------------------------------------------------------------------------

// AuthorizationRequest is one pending decision as the owner sees it. It
// carries no secret: not the handle, not the form token, not a code.
type AuthorizationRequest struct {
	ID          string `json:"id"`
	ClientID    string `json:"client_id"`
	ClientName  string `json:"client_name,omitempty"`
	RedirectURI string `json:"redirect_uri"`
	// Scopes is the effective set: what was granted if the owner has
	// decided, else what the browser selected. It is served alongside the
	// three underlying sets rather than instead of them, because "what does
	// this request currently amount to" is the question an owner asks, and
	// answering it by making them compare three arrays answers a different
	// one.
	Scopes          []string `json:"scopes"`
	RequestedScopes []string `json:"requested_scopes"`
	SelectedScopes  []string `json:"selected_scopes"`
	GrantedScopes   []string `json:"granted_scopes,omitempty"`
	// AuthorizationID is the grant this request eventually minted. It is
	// null until the browser completes and the token endpoint runs -- the
	// owner approves BEFORE anything is minted -- and it is what
	// `agm admin authorizations revoke` is given to undo the whole thing.
	AuthorizationID *string `json:"authorization_id"`
	Status          string  `json:"status"`
	DenyReason      string  `json:"deny_reason,omitempty"`
	Source          string  `json:"source,omitempty"`
	ExpiresAt       *string `json:"expires_at"`
	DecidedAt       *string `json:"decided_at"`
	CompletedAt     *string `json:"completed_at"`
	CreatedAt       *string `json:"created_at"`
}

// ListAuthorizationRequests lists requests, optionally by status.
func (s *Server) ListAuthorizationRequests(ctx context.Context, status string) ([]AuthorizationRequest, *apierr.Error) {
	switch status {
	case "", store.AuthRequestPending, store.AuthRequestApproved,
		store.AuthRequestDenied, store.AuthRequestCompleted:
	default:
		return nil, invalidRequest("status",
			"status is one of pending, approved, denied, completed")
	}
	rows, err := s.st.AuthorizationRequests(ctx, status)
	if err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the requests could not be read")
	}
	out := make([]AuthorizationRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.requestDTO(ctx, row))
	}
	return out, nil
}

// GetAuthorizationRequest shows one request.
func (s *Server) GetAuthorizationRequest(ctx context.Context, id string) (*AuthorizationRequest, *apierr.Error) {
	row, err := s.st.AuthorizationRequestByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAuthorizationRequestNotFound) {
			return nil, apierr.NotFound("authorization request")
		}
		return nil, apierr.New(apierr.CodeInternalError, "the request could not be read")
	}
	out := s.requestDTO(ctx, row)
	return &out, nil
}

// Approve is `POST /v1/admin/authorization-requests/{id}/approve`.
//
// The optional `scopes` may only NARROW the browser-selected set. Widening or
// an empty list is `invalid_request`; a no-longer-pending request is
// `idempotency_conflict`; an expired one is `invalid_request` (section 9.5).
//
// Narrowing-only matters because the browser-selected set is what the owner
// saw on the screen next to the disclosure line. An approval that could widen
// it would grant access the screen never described.
func (s *Server) Approve(ctx context.Context, id string, scopes []string, source string) (*AuthorizationRequest, *apierr.Error) {
	row, err := s.st.AuthorizationRequestByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAuthorizationRequestNotFound) {
			return nil, apierr.NotFound("authorization request")
		}
		return nil, apierr.New(apierr.CodeInternalError, "the request could not be read")
	}
	if row.Status != store.AuthRequestPending {
		return nil, apierr.New(apierr.CodeIdempotencyConflict,
			"this request is "+row.Status+" and can no longer be decided")
	}
	if s.now().UnixMilli() >= row.ExpiresAtMS {
		return nil, invalidRequest("id",
			"this request expired before it was approved; the client must start again")
	}

	selected, perr := authz.ParseScopeString(row.SelectedScopes)
	if perr != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the request's scopes are unreadable")
	}
	granted := selected
	if len(scopes) > 0 {
		requested, perr := authz.ParseScopes(scopes)
		if perr != nil {
			return nil, invalidRequest("scopes", perr.Error())
		}
		if requested.Empty() {
			return nil, invalidRequest("scopes",
				"an empty scope list grants nothing; deny the request instead")
		}
		if requested.Widens(selected) {
			return nil, invalidRequest("scopes",
				"scopes may only narrow what the browser selected ("+selected.String()+")")
		}
		granted = requested
	}

	var decided bool
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		ok, err := t.DecideAuthorizationRequest(id, store.AuthRequestApproved, granted.String(), "")
		if err != nil {
			return err
		}
		decided = ok
		if !ok {
			return nil
		}
		return t.AppendAudit("authorization.approved", "ok", "", "", source, mustJSON(map[string]any{
			"request_id":      id,
			"client_id":       row.ClientID,
			"selected_scopes": selected.Strings(),
			"granted_scopes":  granted.Strings(),
			"stage":           "owner_approved",
		}))
	}); err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the approval could not be recorded")
	}
	if !decided {
		return nil, apierr.New(apierr.CodeIdempotencyConflict,
			"this request was decided by somebody else first")
	}
	return s.GetAuthorizationRequest(ctx, id)
}

// Deny is `POST /v1/admin/authorization-requests/{id}/deny`. A denial is a
// decision the client is entitled to hear, so the waiting page's completion
// redirects with `error=access_denied` rather than leaving it hanging.
func (s *Server) Deny(ctx context.Context, id, reason, source string) (*AuthorizationRequest, *apierr.Error) {
	row, err := s.st.AuthorizationRequestByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAuthorizationRequestNotFound) {
			return nil, apierr.NotFound("authorization request")
		}
		return nil, apierr.New(apierr.CodeInternalError, "the request could not be read")
	}
	if row.Status != store.AuthRequestPending {
		return nil, apierr.New(apierr.CodeIdempotencyConflict,
			"this request is "+row.Status+" and can no longer be decided")
	}
	var decided bool
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		ok, err := t.DecideAuthorizationRequest(id, store.AuthRequestDenied, "", reason)
		if err != nil {
			return err
		}
		decided = ok
		if !ok {
			return nil
		}
		return t.AppendAudit("authorization.denied", "ok", "", "", source, mustJSON(map[string]any{
			"request_id": id,
			"client_id":  row.ClientID,
			"reason":     reason,
		}))
	}); err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the denial could not be recorded")
	}
	if !decided {
		return nil, apierr.New(apierr.CodeIdempotencyConflict,
			"this request was decided by somebody else first")
	}
	return s.GetAuthorizationRequest(ctx, id)
}

func (s *Server) requestDTO(ctx context.Context, row store.AuthorizationRequest) AuthorizationRequest {
	effective := row.GrantedScopes
	if effective == "" {
		effective = row.SelectedScopes
	}
	out := AuthorizationRequest{
		ID:              row.ID,
		ClientID:        row.ClientID,
		RedirectURI:     row.RedirectURI,
		Scopes:          store.SplitScopes(effective),
		RequestedScopes: store.SplitScopes(row.RequestedScopes),
		SelectedScopes:  store.SplitScopes(row.SelectedScopes),
		GrantedScopes:   store.SplitScopes(row.GrantedScopes),
		Status:          s.effectiveStatus(row),
		DenyReason:      row.DenyReason,
		Source:          row.Source,
		ExpiresAt:       msTime(row.ExpiresAtMS),
		DecidedAt:       msTime(row.DecidedAtMS),
		CompletedAt:     msTime(row.CompletedAtMS),
		CreatedAt:       msTime(row.CreatedAtMS),
	}
	if client, err := s.st.OAuthClientByID(ctx, row.ClientID); err == nil {
		out.ClientName = client.Name
	}
	// null, not "", until the browser completes and the token endpoint runs.
	// An empty string reads as "there is one and it is blank"; null reads as
	// "there is not one yet", which is the true thing.
	if id, err := s.st.AuthorizationIDForRequest(ctx, row.ID); err == nil && id != "" {
		out.AuthorizationID = &id
	}
	return out
}

// invalidRequest is `invalid_request` naming the parameter that was wrong, in
// `details.parameter` -- the key section 7.1 fixes for a query parameter and
// for a body field alike on these routes, because an owner correcting a call
// wants the name, not a sentence to read.
func invalidRequest(parameter, message string) *apierr.Error {
	e := apierr.New(apierr.CodeInvalidRequest, message)
	e.Details = map[string]any{"parameter": parameter}
	return e
}

// ---------------------------------------------------------------------------
// Registered clients: listing and revocation.
// ---------------------------------------------------------------------------

// Client is one dynamic registration as the owner sees it.
type Client struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	GrantTypes   []string `json:"grant_types"`
	Source       string   `json:"source,omitempty"`
	ActivatedAt  *string  `json:"activated_at"`
	ExpiresAt    *string  `json:"expires_at"`
	CreatedAt    *string  `json:"created_at"`
}

func clientDTO(c store.OAuthClient) Client {
	return Client{
		ID:           c.ID,
		Name:         c.Name,
		RedirectURIs: c.RedirectURIs,
		GrantTypes:   c.GrantTypes,
		Source:       c.Source,
		ActivatedAt:  msTime(c.ActivatedAtMS),
		ExpiresAt:    msTime(c.ExpiresAtMS),
		CreatedAt:    msTime(c.CreatedAtMS),
	}
}

// ListClients lists every registration.
func (s *Server) ListClients(ctx context.Context) ([]Client, *apierr.Error) {
	rows, err := s.st.OAuthClients(ctx)
	if err != nil {
		return nil, apierr.New(apierr.CodeInternalError, "the registrations could not be read")
	}
	out := make([]Client, 0, len(rows))
	for _, row := range rows {
		out = append(out, clientDTO(row))
	}
	return out, nil
}

// GetClient shows one registration.
func (s *Server) GetClient(ctx context.Context, id string) (*Client, *apierr.Error) {
	row, err := s.st.OAuthClientByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			return nil, apierr.NotFound("client")
		}
		return nil, apierr.New(apierr.CodeInternalError, "the registration could not be read")
	}
	out := clientDTO(row)
	return &out, nil
}

// RevokeClient removes a registration and revokes every authorization it
// holds, returning how many. Removing the registration alone would leave live
// tokens behind: the grant outlives the client row, so both have to go.
func (s *Server) RevokeClient(ctx context.Context, id, reason, source string) (int, *apierr.Error) {
	if _, err := s.st.OAuthClientByID(ctx, id); err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			return 0, apierr.NotFound("client")
		}
		return 0, apierr.New(apierr.CodeInternalError, "the registration could not be read")
	}
	grants, err := s.st.AuthorizationsForClient(ctx, id)
	if err != nil {
		return 0, apierr.New(apierr.CodeInternalError, "the registration could not be read")
	}
	for _, g := range grants {
		if rerr := s.cfg.Authz.RevokeAuthorization(ctx, g.ID, "client_revoked", source); rerr != nil {
			return 0, apierr.New(apierr.CodeInternalError, "an authorization could not be revoked")
		}
	}
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		if err := t.DeleteOAuthClient(id); err != nil {
			return err
		}
		return t.AppendAudit("client.revoked", "ok", "", "", source, mustJSON(map[string]any{
			"client_id":              id,
			"reason":                 reason,
			"authorizations_revoked": len(grants),
		}))
	}); err != nil {
		return 0, apierr.New(apierr.CodeInternalError, "the revocation could not be recorded")
	}
	return len(grants), nil
}
