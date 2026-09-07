package cli

// Refreshing a STORED PROFILE (spec section 11.5).
//
// `agm` used to refresh exactly one kind of credential: the automation pair
// AGENT_GM_REFRESH_TOKEN_FILE + AGENT_GM_CLIENT_ID. A profile written by
// `agm auth login` was presented until the server refused it and then
// reported as "log in again", with a perfectly good refresh token sitting
// beside it in the same file. `admin.access_token_ttl` defaults to 15
// minutes, so an admin session was dead a quarter of an hour after it was
// minted and the next command looked like a server fault.
//
// Two triggers, whichever comes first:
//
//   - PROACTIVE. The profile records `expires_at`. A token within
//     refreshSkew of that instant is exchanged BEFORE the request, and a
//     token past it is never presented at all -- a round trip spent proving
//     that a token this process already knew was dead is a round trip that
//     buys nothing and, on a mutation, has to be reasoned about.
//   - ON REFUSAL. `invalid_token` from the server refreshes once and sends
//     the same request again, with the same Idempotency-Key. This is the
//     path for a token revoked early, and for a profile written by an older
//     build that recorded no expiry.
//
// The write-back safety rule of section 11.5 is unchanged and is the part
// most easily got wrong: the destination is proved writable BEFORE the token
// is spent, so an unwritable directory is exit 9 with the token still good.
// A refresh token the server has already refused is exit 3 and is never
// retried -- rotation makes it single-use, so a retry cannot succeed.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// refreshSkew is how long before its stated expiry a stored access token is
// exchanged. It covers the round trip and a modest clock difference between
// this machine and the server; a token that expires mid-flight is a failure
// the owner cannot do anything about.
const refreshSkew = 60 * time.Second

// loginAgainMessage is the sentence exit 3 carries once a refresh has been
// attempted and refused. Run's second line adds the admin-session caveat.
const loginAgainMessage = "log in again -- `agm auth login`, or " +
	"`agm auth login --admin` if this profile is an admin session"

// profileExpiry parses a profile's `expires_at` DEFENSIVELY.
//
// The value is a string the server wrote, and a profile from an older build
// may carry none at all or one this build cannot parse. Neither is a reason
// to refuse to run and neither is a reason to refresh on every invocation:
// an unknown expiry simply falls back to the on-refusal path.
func profileExpiry(p Profile) (time.Time, bool) {
	raw := strings.TrimSpace(p.ExpiresAt)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// storedProfile re-reads the profile this invocation is using.
func (r *runner) storedProfile() (Profile, bool, error) {
	creds, err := r.store.Load()
	if err != nil {
		return Profile{}, false, err
	}
	p, ok := creds.Profiles[r.cred.Profile]
	return p, ok, nil
}

// refreshProfileIfExpiring is the proactive half, run by connect before the
// command sends anything.
func (r *runner) refreshProfileIfExpiring() error {
	p, ok, err := r.storedProfile()
	if err != nil || !ok {
		return err
	}
	expiry, known := profileExpiry(p)
	if !known {
		// An older build's profile. The on-refusal path covers it.
		return nil
	}
	now := r.env.Now()
	if now.Add(refreshSkew).Before(expiry) {
		return nil
	}
	if p.RefreshToken == "" {
		if now.Before(expiry) {
			// Close to expiry but still alive, and nothing to exchange:
			// spend it rather than refuse a command that would have worked.
			return nil
		}
		return &apierr.Error{Code: apierr.CodeInvalidToken, Message: "the profile " +
			r.cred.Profile + "'s access token expired at " + p.ExpiresAt +
			" and the profile holds no refresh token; " + loginAgainMessage}
	}
	r.out.Verbosef("the profile %s expires at %s; refreshing before the request",
		r.cred.Profile, p.ExpiresAt)
	return r.refreshProfile()
}

// refreshProfile exchanges the profile's refresh token and stores the
// rotation. It is also the client's OnInvalidToken hook.
//
// Both an admin session and an OAuth authorization refresh at
// `/v1/auth/refresh`. An admin refresh token is NEVER sent to `/oauth/token`
// (spec section 9.6): that grant is the OAuth client's, it would answer
// `invalid_grant`, and the two paths must not cross.
func (r *runner) refreshProfile() error {
	p, ok, err := r.storedProfile()
	if err != nil {
		return err
	}
	if !ok || p.RefreshToken == "" {
		return &apierr.Error{Code: apierr.CodeInvalidToken, Message: "the bearer token was " +
			"refused and the profile " + r.cred.Profile + " holds no refresh token; " +
			loginAgainMessage}
	}

	// Step 1. Prove the destination writable. Nothing has been spent yet, so
	// an unwritable directory is exit 9 with the token still good.
	pending, err := r.store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()

	// Step 2. Exchange, with NO bearer of our own: the refresh token is the
	// credential here, and presenting a dead access token alongside it only
	// gives the server a second thing to refuse.
	// The hook is cleared for the duration, because this call can reach
	// refreshProfile a second time otherwise -- the refusal of a refresh
	// token is itself `invalid_token` -- and the second attempt would
	// deadlock on the advisory lock the first one is holding.
	presented, hook := r.client.Token, r.client.OnInvalidToken
	r.client.Token, r.client.OnInvalidToken = "", nil
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost,
		Path:   "/v1/auth/refresh",
		Body:   map[string]any{"refresh_token": p.RefreshToken},
	})
	r.client.Token, r.client.OnInvalidToken = presented, hook
	if err != nil {
		var apiErr *apierr.Error
		if errors.As(err, &apiErr) && apiErr.Code == apierr.CodeInvalidToken {
			return &apierr.Error{
				Code: apierr.CodeInvalidToken,
				Message: "the profile " + r.cred.Profile + "'s refresh token was refused; it " +
					"is single-use and is not retried. " + loginAgainMessage,
				Details: apiErr.Details,
			}
		}
		return err
	}

	var body struct {
		AccessToken     string   `json:"access_token"`
		RefreshToken    string   `json:"refresh_token"`
		ExpiresAt       string   `json:"access_token_expires_at"`
		Scopes          []string `json:"scopes"`
		AuthorizationID string   `json:"authorization_id"`
	}
	if err := json.Unmarshal(resp.Data, &body); err != nil || body.AccessToken == "" {
		return &ContractError{Msg: "POST /v1/auth/refresh answered without an access_token"}
	}

	// Step 3. Store the rotation atomically, at 0600, without changing which
	// profile is active.
	p.AccessToken = body.AccessToken
	if body.RefreshToken != "" {
		p.RefreshToken = body.RefreshToken
	}
	p.ExpiresAt = body.ExpiresAt
	if len(body.Scopes) > 0 {
		p.Scopes = body.Scopes
	}
	if body.AuthorizationID != "" {
		p.AuthorizationID = body.AuthorizationID
	}
	if err := r.store.UpdateProfile(pending, r.cred.Profile, p); err != nil {
		return err
	}

	r.client.Token = body.AccessToken
	r.cred.Token = body.AccessToken
	r.out.Verbosef("the profile %s was refreshed", r.cred.Profile)
	return nil
}
