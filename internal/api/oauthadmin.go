package api

import (
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/oauth"
	"github.com/thisnick/agent-gm/internal/store"
	"github.com/thisnick/agent-gm/internal/wire"
)

// The owner's OAuth routes of spec section 9.5: enrollment codes,
// authorization requests, the grants they produce and the registrations
// behind them. All four families are `admin`, and none of them is reachable
// by any token an agent can hold.
//
// **These handlers contain no OAuth policy.** The scope ceiling, the
// never-enrollable `admin`, the narrowing rule at approval and the
// generic-failure rule all live in `internal/oauth`; this file decodes,
// delegates and renders, which is what keeps the CLI, the REST surface and
// the browser screen agreeing about them.

// oauthServer is the authorization server these routes delegate to. It is nil
// in a deployment (or a narrow unit test) that has not wired one, and every
// route below then answers `internal_error` rather than pretending.
func (d *HandlerDeps) oauthServer() (*oauth.Server, *apierr.Error) {
	if d.OAuth == nil {
		return nil, apierr.New(apierr.CodeInternalError,
			"this build has no authorization server wired")
	}
	return d.OAuth, nil
}

type enrollmentCodeBody struct {
	Label       string   `json:"label"`
	ExpiresIn   any      `json:"expires_in"`
	Scopes      []string `json:"scopes"`
	AllowScopes []string `json:"allow_scopes"`
}

// adminEnrollmentCodesCreate is `POST /v1/admin/enrollment-codes`.
//
// It answers **200, not 201** (spec section 9.5). The code is not a resource
// the caller can go and fetch afterwards -- there is nothing at a URL to
// point a Location header at, because only the SHA-256 is stored -- so 201
// would promise a thing that does not exist.
func (d *HandlerDeps) adminEnrollmentCodesCreate(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	var body enrollmentCodeBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	expiresIn, e := durationField("expires_in", body.ExpiresIn)
	if e != nil {
		return nil, e
	}
	code, cerr := srv.IssueEnrollmentCode(r.Ctx, oauth.EnrollmentRequest{
		Label:       body.Label,
		ExpiresIn:   expiresIn,
		Scopes:      body.Scopes,
		AllowScopes: body.AllowScopes,
	}, r.Source)
	if cerr != nil {
		return nil, cerr
	}
	d.writeAudit(r.Ctx, audit.Event{
		Kind:   audit.KindEnrollmentCreated,
		Result: "ok",
		Payload: map[string]any{
			"enrollment_code_id": code.ID,
			"label":              code.Label,
			"scopes":             code.Scopes,
		},
	})
	return &Response{Data: code}, nil
}

// durationField accepts the two shapes section 9.5 allows for `expires_in`: a
// duration string, or a number of seconds. It is a helper rather than an
// `any` reaching the OAuth layer because a JSON number arrives as a float64
// and turning that into "3.6e+03" would be a bug nobody would find twice.
func durationField(name string, v any) (string, *apierr.Error) {
	switch value := v.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case float64:
		return (time.Duration(value) * time.Second).String(), nil
	default:
		return "", apierr.WrongTypeForField(name, "a duration string such as \"30m\" or a number of seconds")
	}
}

func (d *HandlerDeps) adminEnrollmentCodesList(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	codes, cerr := srv.ListEnrollmentCodes(r.Ctx)
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: map[string]any{"items": codes}}, nil
}

func (d *HandlerDeps) adminEnrollmentCodesGet(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	code, cerr := srv.GetEnrollmentCode(r.Ctx, r.Path["enrollment_code_id"])
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: code}, nil
}

func (d *HandlerDeps) adminEnrollmentCodesRevoke(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	revoked, cerr := srv.RevokeEnrollmentCode(r.Ctx, r.Path["enrollment_code_id"], r.Query["reason"], r.Source)
	if cerr != nil {
		return nil, cerr
	}
	if revoked {
		d.writeAudit(r.Ctx, audit.Event{
			Kind:   audit.KindEnrollmentRevoked,
			Result: "ok",
			Payload: map[string]any{
				"enrollment_code_id": r.Path["enrollment_code_id"],
				"reason":             r.Query["reason"],
			},
		})
	}
	// `revoked: false` on a repeat, with a 200. A second revocation is not a
	// failure; it is a no-longer-necessary act, and answering 404 or 409
	// would make an idempotent script wrong (spec section 9.5).
	//
	// `changed` says which of the two happened, `revoked_at` says when the
	// code actually stopped being redeemable -- on a repeat that is the
	// FIRST revocation's stamp, not this call's -- and `effect` is the same
	// sentence `agm` shows before it asks.
	code, cerr := srv.GetEnrollmentCode(r.Ctx, r.Path["enrollment_code_id"])
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: map[string]any{
		"id":         code.ID,
		"revoked":    true,
		"changed":    revoked,
		"revoked_at": code.RevokedAt,
		"effect":     apierr.EffectEnrollmentCodeRevoke,
	}}, nil
}

func (d *HandlerDeps) adminAuthorizationRequestsList(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	requests, cerr := srv.ListAuthorizationRequests(r.Ctx, r.Query["status"])
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: map[string]any{"items": requests}}, nil
}

func (d *HandlerDeps) adminAuthorizationRequestsGet(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	req, cerr := srv.GetAuthorizationRequest(r.Ctx, r.Path["authorization_request_id"])
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: req}, nil
}

type approveBody struct {
	Scopes []string `json:"scopes"`
}

func (d *HandlerDeps) adminAuthorizationRequestsApprove(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	var body approveBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	req, cerr := srv.Approve(r.Ctx, r.Path["authorization_request_id"], body.Scopes, r.Source)
	if cerr != nil {
		return nil, cerr
	}
	d.writeAudit(r.Ctx, audit.Event{
		Kind:   audit.KindAuthorizationApproved,
		Result: "ok",
		Payload: map[string]any{
			"request_id":     req.ID,
			"client_id":      req.ClientID,
			"granted_scopes": req.GrantedScopes,
		},
	})
	return &Response{Data: req}, nil
}

type denyBody struct {
	Reason string `json:"reason"`
}

func (d *HandlerDeps) adminAuthorizationRequestsDeny(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	var body denyBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	req, cerr := srv.Deny(r.Ctx, r.Path["authorization_request_id"], body.Reason, r.Source)
	if cerr != nil {
		return nil, cerr
	}
	d.writeAudit(r.Ctx, audit.Event{
		Kind:    audit.KindAuthorizationDenied,
		Result:  "ok",
		Payload: map[string]any{"request_id": req.ID, "client_id": req.ClientID},
	})
	return &Response{Data: req}, nil
}

// authorizationDTO is one grant as the owner sees it. It carries no token and
// no hash: the question this route answers is "what is allowed to talk to my
// server, and can I stop it?".
type authorizationDTO struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	ClientID string `json:"client_id,omitempty"`
	// ClientName is the name the client gave at registration. Without it an
	// owner auditing their grants reads a list of UUIDs and has to go and
	// look each one up before they can decide what to revoke, which is
	// exactly the moment they are least inclined to.
	ClientName   string   `json:"client_name,omitempty"`
	Scopes       []string `json:"scopes"`
	MintedScopes []string `json:"minted_scopes"`
	Source       string   `json:"source,omitempty"`
	Revoked      bool     `json:"revoked"`
	RevokedAt    *string  `json:"revoked_at"`
	ExpiresAt    *string  `json:"expires_at"`
	CreatedAt    *string  `json:"created_at"`
}

func authorizationFrom(a store.Authorization) authorizationDTO {
	return authorizationDTO{
		ID:           a.ID,
		Kind:         a.Kind,
		ClientID:     a.Client,
		Scopes:       store.SplitScopes(a.Scopes),
		MintedScopes: store.SplitScopes(a.MintedScopes),
		Source:       a.Source,
		Revoked:      a.Revoked(),
		RevokedAt:    msTimePtr(a.RevokedAtMS),
		ExpiresAt:    msTimePtr(a.ExpiresAtMS),
		CreatedAt:    msTimePtr(a.CreatedAtMS),
	}
}

// msTimePtr renders a stored millisecond stamp through internal/wire, for the
// reason its guard states: Go marshals a time.Time as RFC3339Nano, which
// strips trailing zeros, so the width would depend on the value.
func msTimePtr(ms int64) *string { return wire.InstantMS(ms) }

func (d *HandlerDeps) adminAuthorizationsList(r *Request) (*Response, error) {
	includeRevoked := r.Query["include_revoked"] == "true"
	rows, err := d.Store.Authorizations(r.Ctx, includeRevoked)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	items := make([]authorizationDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, d.authorizationWithClientName(r, row))
	}
	return &Response{Data: map[string]any{"items": items}}, nil
}

func (d *HandlerDeps) adminAuthorizationsGet(r *Request) (*Response, error) {
	row, err := d.Store.Authorization(r.Ctx, r.Path["authorization_id"])
	if err != nil {
		return nil, apierr.NotFound("authorization")
	}
	return &Response{Data: d.authorizationWithClientName(r, row)}, nil
}

// authorizationWithClientName fills in the registration's name for an OAuth
// grant. An admin bootstrap session has no client and keeps the field empty,
// which is the honest answer rather than a placeholder.
func (d *HandlerDeps) authorizationWithClientName(r *Request, row store.Authorization) authorizationDTO {
	out := authorizationFrom(row)
	if row.Client == "" {
		return out
	}
	if client, err := d.Store.OAuthClientByID(r.Ctx, row.Client); err == nil {
		out.ClientName = client.Name
	}
	return out
}

// adminAuthorizationsRevoke is `DELETE /v1/admin/authorizations/{id}`, the
// route section 16 Slice 3 test 29 uses to cut a connector off. Revoking the
// authorization revokes every token of it, so the next call the connector
// makes is a 401 rather than a slow degradation.
func (d *HandlerDeps) adminAuthorizationsRevoke(r *Request) (*Response, error) {
	id := r.Path["authorization_id"]
	before, err := d.Store.Authorization(r.Ctx, id)
	if err != nil {
		return nil, apierr.NotFound("authorization")
	}
	if !before.Revoked() {
		if err := d.Authz.RevokeAuthorization(r.Ctx, id, r.Query["reason"], r.Source); err != nil {
			return nil, apierr.NotFound("authorization")
		}
	}
	after, err := d.Store.Authorization(r.Ctx, id)
	if err != nil {
		return nil, apierr.NotFound("authorization")
	}
	out := d.authorizationWithClientName(r, after)
	return &Response{Data: map[string]any{
		"id":         id,
		"revoked":    true,
		"changed":    !before.Revoked(),
		"revoked_at": out.RevokedAt,
		"status":     "revoked",
		"effect":     apierr.EffectAuthorizationRevoke,
	}}, nil
}

func (d *HandlerDeps) adminClientsList(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	clients, cerr := srv.ListClients(r.Ctx)
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: map[string]any{"items": clients}}, nil
}

func (d *HandlerDeps) adminClientsGet(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	client, cerr := srv.GetClient(r.Ctx, r.Path["client_id"])
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: client}, nil
}

func (d *HandlerDeps) adminClientsRevoke(r *Request) (*Response, error) {
	srv, aerr := d.oauthServer()
	if aerr != nil {
		return nil, aerr
	}
	revoked, cerr := srv.RevokeClient(r.Ctx, r.Path["client_id"], r.Query["reason"], r.Source)
	if cerr != nil {
		return nil, cerr
	}
	return &Response{Data: map[string]any{
		"id":                     r.Path["client_id"],
		"revoked":                true,
		"changed":                true,
		"revoked_at":             wire.InstantPtr(d.now()),
		"authorizations_revoked": revoked,
		"effect":                 apierr.EffectClientRevoke,
	}}, nil
}
