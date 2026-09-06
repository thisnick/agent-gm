package api

import (
	"errors"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/store"
)

// The account routes of spec sections 7.5 and 4.7.

// account resolves an `{account_id}` path parameter.
//
// The prefix check runs first and answers `invalid_request` naming the
// parameter and the expected prefix; only a well-formed ID reaches the
// lookup, which is what keeps a `msg_` in an account slot from ever being
// `not_found` (section 16 Slice 2 test 13).
func (d *HandlerDeps) account(r *Request) (store.Account, *apierr.Error) {
	id, e := pathID(r, "account_id", store.PrefixAccount)
	if e != nil {
		return store.Account{}, e
	}
	row, err := d.Store.Account(r.Ctx, id)
	if errors.Is(err, store.ErrAccountNotFound) {
		return store.Account{}, apierr.NotFound("account")
	}
	if err != nil {
		return store.Account{}, apierr.From(err)
	}
	return row, nil
}

func (d *HandlerDeps) accountListDTOFor(row store.Account, health *accounts.AccountHealth) accountListDTO {
	responding := false
	if health != nil {
		responding = health.PhoneResponding
	}
	return accountListDTO{
		ID:            row.ID,
		GoogleAccount: row.GoogleAccount,
		Label:         nullable(row.Label),
		State:         string(row.State),
		StateReason:   nullable(row.StateReason),
		// A pairing in flight is held by the pairing manager, not by a
		// column: an abandoned pairing must leave nothing behind (spec
		// section 16 Slice 2 test 38), so there is no row for it to leave.
		PairingID:       nullable(d.pairings.forAccount(row.ID)),
		PhoneID:         nullable(row.PhoneID),
		PhoneResponding: responding,
		PairedAt:        rfc3339(row.PairedAtMS),
		LastEventAt:     rfc3339(row.LastEventAtMS),
	}
}

// accountsList is `GET /v1/accounts`: **every account, whatever its state**.
// The four health blocks are deliberately omitted here and served on
// `GET /v1/accounts/{account_id}`, so a many-account listing stays small.
func (d *HandlerDeps) accountsList(r *Request) (*Response, error) {
	rows, err := d.Store.Accounts(r.Ctx)
	if err != nil {
		return nil, err
	}
	health, err := d.Supervisor.Health(r.Ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]accounts.AccountHealth{}
	for _, h := range health {
		byID[h.AccountID] = h
	}
	out := make([]accountListDTO, 0, len(rows))
	for _, row := range rows {
		h := byID[row.ID]
		out = append(out, d.accountListDTOFor(row, &h))
	}
	return &Response{Data: items(out)}, nil
}

// accountsGet is `GET /v1/accounts/{account_id}`: the list block plus the
// `google`, `backfill`, `sweep` and `counters` blocks -- the same per-account
// object `GET /v1/health` embeds.
func (d *HandlerDeps) accountsGet(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	health, err := d.Supervisor.HealthFor(r.Ctx, row.ID)
	if err != nil && !errors.Is(err, accounts.ErrUnknownAccount) {
		return nil, err
	}
	return &Response{Data: accountDetailDTO{
		accountListDTO: d.accountListDTOFor(row, &health),
		// `google` is null for an account that is not connected: the values
		// are cached from that account's last config fetch, and inventing
		// them would make /v1/health block on a phone.
		Google:   health.Google,
		Backfill: health.Backfill,
		Sweep:    health.Sweep,
		Counters: health.Counters,
	}}, nil
}

type labelBody struct {
	Label *string `json:"label"`
}

// accountsLabel is `PATCH /v1/accounts/{account_id}`. A human name for a
// listing; nothing else is mutable.
func (d *HandlerDeps) accountsLabel(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	var body labelBody
	if err := r.DecodeBody(&body); err != nil {
		return nil, err
	}
	if body.Label == nil {
		return nil, apierr.MissingParameter("label")
	}
	label, warnings := apierr.NormalizeFilename(*body.Label)
	r.Warn(warnings...)
	if err := d.Store.SetAccountLabel(r.Ctx, row.ID, label); err != nil {
		return nil, err
	}
	d.writeAudit(r.Ctx, audit.Event{
		Kind: audit.KindAccountLabelChanged, AccountID: row.ID,
		AuthorizationID: r.Auth.ID, TargetType: "account", TargetID: row.ID,
		Result: audit.ResultOK, Source: r.Source,
		Payload: map[string]any{"label_length": len(label)},
	})
	row.Label = label
	return &Response{Data: d.accountListDTOFor(row, nil)}, nil
}

// accountsReconnect is `POST /v1/accounts/{account_id}/reconnect`.
func (d *HandlerDeps) accountsReconnect(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	a, err := d.Supervisor.Get(row.ID)
	if err != nil {
		return nil, apierr.NotFound("account")
	}
	d.Supervisor.Stop(a)
	if err := d.Supervisor.Start(r.Ctx, a); err != nil {
		return nil, err
	}
	return &Response{Data: map[string]any{"account_id": row.ID, "reconnected": true}}, nil
}

type confirmBody struct {
	Confirm bool `json:"confirm"`
}

// requireConfirm enforces the `{"confirm": true}` of spec section 4.7 on the
// two routes that have lasting consequences.
//
// It runs before anything else happens, and its refusal is `invalid_request`
// naming the field. **Without it nothing is deleted and nothing is signed
// out** -- which is the half of section 16 Slice 2 test 33 that is easy to
// get wrong, because a handler that signed out first and then checked would
// pass the "deletes nothing" assertion while having done something.
func requireConfirm(r *Request) *apierr.Error {
	var body confirmBody
	if e := r.DecodeBody(&body); e != nil {
		return e
	}
	if !body.Confirm {
		e := apierr.New(apierr.CodeInvalidRequest,
			"this is irreversible, so it requires {\"confirm\": true} in the body; nothing was done")
		e.Details = map[string]any{"field": "confirm"}
		return e
	}
	return nil
}

type signOutDTO struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"`
	Effect    string `json:"effect"`
}

// SignOutEffect is what signing out does, in one sentence, so no surface can
// claim more than another (spec section 4.7).
const SignOutEffect = "shreds this account's Google session file and stops its connection; " +
	"every conversation, message and attachment Agent GM has indexed is kept and stays readable"

// accountsSignOut is `POST /v1/accounts/{account_id}/sign-out`: **shreds the
// session file and keeps every row** (spec section 4.7). It is not a
// deletion, and `DELETE /v1/accounts/{id}` is the only route that is.
func (d *HandlerDeps) accountsSignOut(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	if e := requireConfirm(r); e != nil {
		return nil, e
	}
	a, err := d.Supervisor.Get(row.ID)
	if err == nil {
		if err := d.Supervisor.SignOut(r.Ctx, a); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, accounts.ErrUnknownAccount) {
		return nil, err
	}
	return &Response{Data: signOutDTO{
		AccountID: row.ID,
		State:     string(store.StateSignedOut),
		Effect:    SignOutEffect,
	}}, nil
}

// deletedCountsDTO is what a removal deleted, per table.
type deletedCountsDTO struct {
	Conversations     int64 `json:"conversations"`
	Participants      int64 `json:"participants"`
	Messages          int64 `json:"messages"`
	Attachments       int64 `json:"attachments"`
	Reactions         int64 `json:"reactions"`
	BackfillState     int64 `json:"backfill_state"`
	Operations        int64 `json:"operations"`
	Contacts          int64 `json:"contacts"`
	MediaCacheEntries int64 `json:"media_cache_entries"`
}

type accountRemovedDTO struct {
	AccountID     string           `json:"account_id"`
	Removed       bool             `json:"removed"`
	DeletedCounts deletedCountsDTO `json:"deleted_counts"`
	MediaFiles    int              `json:"media_files_unlinked"`
	Effect        string           `json:"effect"`
}

// accountsRemove is `DELETE /v1/accounts/{account_id}` -- **the only route in
// Agent GM that deletes an account's data** (spec sections 4.7, 7.5, D30).
//
// The order is the contract, and it is the erasure order of spec section 10.3:
//
//  1. require `{"confirm": true}`; without it, refuse and delete nothing;
//  2. sign the account out, so its cookies are gone from the process before
//     its rows are;
//  3. delete the rows in ONE transaction, **collecting the cached media paths
//     inside it**;
//  4. commit;
//  5. only then unlink the files.
//
// Steps 3 to 5 are in that order because the alternative is unrecoverable.
// The eviction sweep finds every file it deletes through `media_cache_entries`,
// so deleting the files first and then failing to delete the rows would leave
// the index claiming bytes that are not there, and deleting the rows without
// collecting the paths first would leave files nothing references and no way
// to find them. A crash between the commit and the unlink leaves an orphan
// file, which an operator can delete, and **never an orphan row** -- which is
// exactly what section 16 Slice 2 test 20 injects a fault to prove.
//
// The `account.removed` audit row is written after the erasure and
// **survives it**, carrying its `account_id`: that is how "what happened to
// this account" stays answerable after the account is gone (spec section 12.4).
func (d *HandlerDeps) accountsRemove(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	if e := requireConfirm(r); e != nil {
		return nil, e
	}

	if a, err := d.Supervisor.Get(row.ID); err == nil {
		if err := d.Supervisor.SignOut(r.Ctx, a); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, accounts.ErrUnknownAccount) {
		return nil, err
	}

	result, err := d.Store.EraseAccount(r.Ctx, row.ID)
	if err != nil {
		return nil, err
	}

	// The window spec section 10.3 names: the transaction has committed and
	// the files are still on disk. In production nothing runs here.
	if d.AfterErase != nil {
		if err := d.AfterErase(row.ID, result.MediaPaths); err != nil {
			return nil, err
		}
	}

	unlinked := 0
	if d.Cache != nil && len(result.MediaPaths) > 0 {
		for _, unlinkErr := range d.Cache.UnlinkAll(result.MediaPaths) {
			if d.Log != nil {
				d.Log.Warn("unlinking cached media after erasure failed",
					"account_id", row.ID, "error", unlinkErr.Error())
			}
		}
		unlinked = len(result.MediaPaths)
	}

	counts := deletedCountsDTO(result.Counts)
	d.writeAudit(r.Ctx, audit.Event{
		Kind: audit.KindAccountRemoved, AccountID: row.ID,
		AuthorizationID: r.Auth.ID, TargetType: "account", TargetID: row.ID,
		Result: audit.ResultOK, Source: r.Source,
		Payload: map[string]any{
			"conversations": counts.Conversations,
			"participants":  counts.Participants,
			"messages":      counts.Messages,
			"attachments":   counts.Attachments,
			"reactions":     counts.Reactions,
			"operations":    counts.Operations,
			"contacts":      counts.Contacts,
			"media_files":   unlinked,
		},
	})

	return &Response{Data: accountRemovedDTO{
		AccountID:     row.ID,
		Removed:       true,
		DeletedCounts: counts,
		MediaFiles:    unlinked,
		Effect:        store.EffectSentence,
	}}, nil
}

type cookiesBody struct {
	Cookies map[string]string `json:"cookies"`
}

// accountsRefreshCookies is `POST /v1/accounts/{account_id}/refresh-cookies`.
// A capture from a different Google address is `pairing_wrong_account` and
// changes nothing (spec section 3.2).
func (d *HandlerDeps) accountsRefreshCookies(r *Request) (*Response, error) {
	row, e := d.account(r)
	if e != nil {
		return nil, e
	}
	var body cookiesBody
	if err := r.DecodeBody(&body); err != nil {
		return nil, err
	}
	if len(body.Cookies) == 0 {
		return nil, apierr.New(apierr.CodePairingNoCookies,
			"a cookie refresh needs the Google cookies; nothing was changed")
	}
	a, err := d.Supervisor.Get(row.ID)
	if err != nil {
		return nil, apierr.NotFound("account")
	}
	if err := a.Backend.RefreshGoogleCookies(r.Ctx, body.Cookies); err != nil {
		return nil, err
	}
	// The address is re-derived and compared rather than trusted: a refresh
	// that signed in as somebody else must change nothing.
	if addr := backendAddress(a.Backend); addr != "" && store.AccountID(addr) != row.ID {
		return nil, apierr.New(apierr.CodePairingWrongAccount,
			"those cookies belong to a different Google account than this one; nothing was changed")
	}
	if err := a.PersistSession(r.Ctx); err != nil {
		return nil, err
	}
	return &Response{Data: map[string]any{"account_id": row.ID, "refreshed": true}}, nil
}
