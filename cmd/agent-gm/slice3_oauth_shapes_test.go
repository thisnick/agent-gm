package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// expireRegistration backdates one registration's expiry.
func expireRegistration(t *testing.T, h *oauthHarness, clientID string) {
	t.Helper()
	if err := h.b.Store.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE oauth_clients SET expires_at_ms = 1 WHERE id = ?`, clientID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// The shapes spec section 9.1 fixes for the answers an HTTP framework would
// otherwise give itself, section 9.9's browser headers, and section 9.3's
// registration sweep.
//
// None of these is an acceptance test in its own right, and every one of them
// is a thing that quietly stops being true. A 405 that arrives as the Go
// framework's plain-text default still says 405; a page that loses its
// Content-Security-Policy still renders. The failure mode is silence, which is
// why they are asserted rather than assumed.

func TestSlice3OAuthAnswersHaveTheirOwnShapes(t *testing.T) {
	h := newOAuthHarness(t)

	t.Run("the root path is 404", func(t *testing.T) {
		resp := h.get("/")
		defer func() { _ = readBody(t, resp) }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/ answered %d, want 404", resp.StatusCode)
		}
	})

	t.Run("an unknown path under /oauth is the REST not_found envelope", func(t *testing.T) {
		resp := h.get("/oauth/nonsense")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("/oauth/nonsense answered %d, want 404", resp.StatusCode)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("the body is not JSON: %s", body)
		}
		if env.Error.Code != "not_found" {
			t.Errorf("the body is %s; an unknown path is not an OAuth protocol failure, so "+
				"it takes the REST envelope rather than {error, error_description}", body)
		}
	})

	t.Run("a wrong method is 405 with Allow and the OAuth shape", func(t *testing.T) {
		resp := h.postJSON("/oauth/authorize", map[string]any{})
		// POST /oauth/authorize exists, so use one that does not: the token
		// endpoint is POST-only.
		_ = readBody(t, resp)

		wrong := h.get("/oauth/token")
		body := readBody(t, wrong)
		if wrong.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /oauth/token answered %d, want 405", wrong.StatusCode)
		}
		if allow := wrong.Header.Get("Allow"); allow != "POST" {
			t.Errorf("the 405 carries Allow: %q, want POST", allow)
		}
		var oe map[string]string
		if err := json.Unmarshal([]byte(body), &oe); err != nil || oe["error"] == "" {
			t.Errorf("the 405 body is %q; section 9.1 gives it the {error, "+
				"error_description} shape rather than the framework's default", body)
		}
	})

	t.Run("a body over 1 MiB is 413 with the OAuth shape", func(t *testing.T) {
		big := strings.Repeat("a", 1<<20+1)
		resp := h.postForm("/oauth/token", url.Values{"token": {big}})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("a 1 MiB + 1 body answered %d, want 413\n%s", resp.StatusCode, body)
		}
		var oe map[string]string
		if err := json.Unmarshal([]byte(body), &oe); err != nil || oe["error"] == "" {
			t.Errorf("the 413 body is %q, want the OAuth error shape", body)
		}
	})

	t.Run("every OAuth answer carries section 9.9's headers", func(t *testing.T) {
		clientID := h.register("http://127.0.0.1:53211/callback")
		_, challenge := pkce(t)
		cases := map[string]*http.Response{
			"the metadata document":    h.get("/.well-known/oauth-authorization-server"),
			"the authorization screen": h.get(authorizeParams{ClientID: clientID, RedirectURI: "http://127.0.0.1:53211/callback", State: "s", Challenge: challenge}.query(h)),
			"an unknown path":          h.get("/oauth/nonsense"),
			"the polling script":       h.get("/oauth/poll.js"),
		}
		// The referrer policy is the one header of section 9.9 that is not
		// the same everywhere: a rendered page and its script say
		// `same-origin`, everything else says `no-referrer`.
		// TestOAuthPagesCarrySameOriginReferrerPolicy owns that split.
		referrer := map[string]string{
			"the metadata document":    "no-referrer",
			"the authorization screen": "same-origin",
			"an unknown path":          "no-referrer",
			"the polling script":       "same-origin",
		}
		for name, resp := range cases {
			h.captureCookie(resp)
			_ = readBody(t, resp)
			want := map[string]string{
				"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; " +
					"base-uri 'none'; form-action 'self'; script-src 'self'; connect-src 'self'; style-src 'self'",
				"Cache-Control":          "no-store",
				"Referrer-Policy":        referrer[name],
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
			}
			for header, value := range want {
				if header == "Content-Security-Policy" && name == "the authorization screen" {
					value = strings.Replace(value, "form-action 'self'", "form-action 'self' http://127.0.0.1:53211", 1)
				}
				if got := resp.Header.Get(header); got != value {
					t.Errorf("%s carries %s: %q, want %q", name, header, got, value)
				}
			}
		}
	})

	t.Run("the polling script is served rather than inlined", func(t *testing.T) {
		resp := h.get("/oauth/poll.js")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/oauth/poll.js answered %d", resp.StatusCode)
		}
		if !strings.Contains(body, "poll_interval_seconds") {
			t.Error("the polling script does not read poll_interval_seconds")
		}
		// The clamp of section 9.5: the server decides the interval and the
		// page refuses to believe a value that would hammer it or appear to
		// hang.
		if !strings.Contains(body, "Math.min(60") || !strings.Contains(body, "Math.max(1") {
			t.Error("the polling script does not clamp the interval to 1-60 seconds")
		}
	})

	t.Run("the context cookie is HttpOnly, Secure, SameSite=Lax and scoped to /oauth", func(t *testing.T) {
		clientID := h.register("http://127.0.0.1:53212/callback")
		_, challenge := pkce(t)
		resp := h.get(authorizeParams{
			ClientID: clientID, RedirectURI: "http://127.0.0.1:53212/callback",
			State: "s", Challenge: challenge,
		}.query(h))
		defer func() { _ = readBody(t, resp) }()
		var found *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "agm_oauth_context" {
				found = c
			}
		}
		if found == nil {
			t.Fatal("the authorization screen set no context cookie")
		}
		if !found.HttpOnly || !found.Secure ||
			found.SameSite != http.SameSiteLaxMode || found.Path != "/oauth" {
			t.Errorf("the cookie is %+v; section 9.4 fixes Path=/oauth, HttpOnly, Secure, "+
				"SameSite=Lax", found)
		}
	})
}

// TestSlice3ExpiredRegistrationsAreSwept is section 9.3's maintenance pass: a
// registration expires 24 hours after creation unless an authorization
// activates it, and the sweep removes the expired unreferenced ones and audits
// each removal.
//
// The sweep is called directly rather than waited for. A test that slept 60
// seconds to watch a ticker would be a minute of nothing, and the ticker is
// not the thing under test.
func TestSlice3ExpiredRegistrationsAreSwept(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	unused := h.register("https://unused.example/cb")
	used := h.register("http://127.0.0.1:53211/callback")

	// Activate the second one by starting an authorization with it.
	_, codeValue := h.enrollmentCode(admin, nil)
	_, challenge := pkce(t)
	page := h.authorizePage(authorizeParams{
		ClientID: used, RedirectURI: "http://127.0.0.1:53211/callback",
		State: "s", Challenge: challenge,
	})
	form := hiddenFields(t, page)
	form.Add("scope_selected", "messages:read")
	form.Set("enrollment_code", codeValue)
	submitted := h.postForm("/oauth/authorize", form, h.withCookie)
	if submitted.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting: %d\n%s", submitted.StatusCode, readBody(t, submitted))
	}
	_ = readBody(t, submitted)

	// Put the unused registration's expiry in the past. `agent-gm serve`
	// runs on the real clock, so twenty-five hours cannot be waited out and
	// must not be slept through; moving the stamp is exactly what those
	// hours would have done to it.
	expireRegistration(t, h, unused)

	removed, err := h.b.OAuth.SweepExpiredRegistrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != unused {
		t.Fatalf("the sweep removed %v; it should remove exactly the expired, "+
			"never-activated, unreferenced registration %s", removed, unused)
	}

	if resp := h.get("/v1/admin/clients/"+unused, bearer(admin)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the swept registration still answers %d", resp.StatusCode)
	}
	if resp := h.get("/v1/admin/clients/"+used, bearer(admin)); resp.StatusCode != http.StatusOK {
		t.Errorf("the ACTIVATED registration answered %d; an authorization activates a "+
			"registration and the sweep must leave it alone", resp.StatusCode)
	}

	audit := h.get("/v1/admin/audit?kind=client.revoked", bearer(admin))
	body := readBody(t, audit)
	if !strings.Contains(body, unused) {
		t.Errorf("the sweep wrote no client.revoked audit row for %s:\n%s", unused, body)
	}
}

// The owner-facing DTOs of spec section 9.5, as the live gate found them
// wanting.
//
// Live gate 25 passed on 40387c4 with Codex CLI as the real MCP client, and
// left three findings that no unit test had any reason to notice, because
// every one of them is a field that is ABSENT rather than wrong: an OAuth
// grant listed with no client name, an approval that did not say what it had
// approved, and a revocation that did not say when. A missing field reads as
// `null`, and `null` reads as "something went wrong" to the owner who is
// trying to decide what to revoke at the moment they most need to be sure.
func TestSlice3TheOwnerFacingDTOsSayEnoughToActOn(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)

	t.Run("an authorization names the client that registered it", func(t *testing.T) {
		list := dataOf(t, h.get("/v1/admin/authorizations", bearer(admin)))
		items, _ := list["items"].([]any)
		var oauthGrant map[string]any
		for _, raw := range items {
			row, _ := raw.(map[string]any)
			if row["kind"] == "oauth" {
				oauthGrant = row
			}
		}
		if oauthGrant == nil {
			t.Fatalf("no OAuth grant in %v", items)
		}
		if name, _ := oauthGrant["client_name"].(string); name != "a test client" {
			t.Errorf("the OAuth grant's client_name is %v; an owner auditing their grants "+
				"should not have to look up a UUID before they can decide what to revoke",
				oauthGrant["client_name"])
		}
		// The admin bootstrap has no client, and says so by omission rather
		// than by inventing a name.
		for _, raw := range items {
			row, _ := raw.(map[string]any)
			if row["kind"] == "admin_bootstrap" {
				if _, ok := row["client_name"]; ok {
					t.Errorf("the admin bootstrap session carries a client_name: %v", row)
				}
			}
		}
	})

	t.Run("a request says what it amounts to and what it minted", func(t *testing.T) {
		req := dataOf(t, h.get("/v1/admin/authorization-requests/"+f.RequestID, bearer(admin)))
		if req["status"] != "completed" {
			t.Errorf("status is %v, want completed", req["status"])
		}
		scopes, _ := req["scopes"].([]any)
		if len(scopes) == 0 {
			t.Errorf("scopes is %v; a request that carries scopes must say so without "+
				"making the reader compare three arrays", req["scopes"])
		}
		if id, _ := req["authorization_id"].(string); id == "" {
			t.Error("authorization_id is empty on a COMPLETED request; it is what " +
				"`agm admin authorizations revoke` is given to undo the whole thing")
		}
	})

	t.Run("a pending request already carries its scopes", func(t *testing.T) {
		// A second flow, stopped at the pending step.
		_, codeValue := h.enrollmentCode(admin, nil)
		clientID := h.register("http://127.0.0.1:53299/callback")
		_, challenge := pkce(t)
		page := h.authorizePage(authorizeParams{
			ClientID: clientID, RedirectURI: "http://127.0.0.1:53299/callback",
			State: "s", Challenge: challenge,
		})
		form := hiddenFields(t, page)
		form.Add("scope_selected", "messages:read")
		form.Set("enrollment_code", codeValue)
		submitted := h.postForm("/oauth/authorize", form, h.withCookie)
		if submitted.StatusCode != http.StatusSeeOther {
			t.Fatalf("submitting: %d", submitted.StatusCode)
		}
		id := strings.TrimPrefix(submitted.Header.Get("Location"), "/oauth/requests/")
		_ = readBody(t, submitted)

		list := dataOf(t, h.get("/v1/admin/authorization-requests?status=pending", bearer(admin)))
		items, _ := list["items"].([]any)
		var pending map[string]any
		for _, raw := range items {
			row, _ := raw.(map[string]any)
			if row["id"] == id {
				pending = row
			}
		}
		if pending == nil {
			t.Fatalf("the pending request %s is not in the listing", id)
		}
		scopes, _ := pending["scopes"].([]any)
		if len(scopes) != 1 || scopes[0] != "messages:read" {
			t.Errorf("a PENDING request's scopes are %v; before a decision the effective "+
				"set is what the browser selected", pending["scopes"])
		}
		if pending["authorization_id"] != nil {
			t.Errorf("a pending request reports authorization_id %v; nothing is minted "+
				"until the browser completes, and null says that where \"\" would say "+
				"there is one and it is blank", pending["authorization_id"])
		}
	})

	t.Run("a revocation says when, and in the words agm confirmed", func(t *testing.T) {
		auth := dataOf(t, h.get("/v1/auth/whoami", bearer(access)))
		id, _ := auth["authorization_id"].(string)

		first := h.do(mustRequest(t, http.MethodDelete,
			h.http.URL+"/v1/admin/authorizations/"+id+"?reason=live+gate", bearer(admin)))
		data := dataOf(t, first)
		if data["revoked"] != true || data["changed"] != true {
			t.Errorf("the first revocation reports %v", data)
		}
		if at, _ := data["revoked_at"].(string); at == "" {
			t.Error("the revocation does not say WHEN the authorization stopped working")
		}
		if data["effect"] != apierr.EffectAuthorizationRevoke {
			t.Errorf("the effect sentence is %q; it must be the one `agm` shows before it "+
				"asks, or a human confirms different words from the ones the server acted on",
				data["effect"])
		}

		// Repeating it is not a failure, and it does not move the stamp.
		second := h.do(mustRequest(t, http.MethodDelete,
			h.http.URL+"/v1/admin/authorizations/"+id, bearer(admin)))
		repeat := dataOf(t, second)
		if repeat["revoked"] != true || repeat["changed"] != false {
			t.Errorf("repeating the revocation reports %v, want revoked:true changed:false", repeat)
		}
		if repeat["revoked_at"] != data["revoked_at"] {
			t.Errorf("the repeat moved revoked_at from %v to %v",
				data["revoked_at"], repeat["revoked_at"])
		}

		// And the token really is dead.
		if resp := h.get("/v1/auth/whoami", bearer(access)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("after revocation the token answered %d at whoami", resp.StatusCode)
		}
	})

	t.Run("an enrollment revocation says when and in the same words", func(t *testing.T) {
		id, _ := h.enrollmentCode(admin, nil)
		first := dataOf(t, h.do(mustRequest(t, http.MethodDelete,
			h.http.URL+"/v1/admin/enrollment-codes/"+id+"?reason=test", bearer(admin))))
		if first["changed"] != true {
			t.Errorf("the first revocation reports changed=%v", first["changed"])
		}
		if at, _ := first["revoked_at"].(string); at == "" {
			t.Error("the enrollment revocation does not say when")
		}
		if first["effect"] != apierr.EffectEnrollmentCodeRevoke {
			t.Errorf("the effect sentence is %q", first["effect"])
		}
		second := dataOf(t, h.do(mustRequest(t, http.MethodDelete,
			h.http.URL+"/v1/admin/enrollment-codes/"+id, bearer(admin))))
		if second["changed"] != false || second["revoked"] != true {
			t.Errorf("repeating reports %v, want revoked:true changed:false", second)
		}
		if second["revoked_at"] != first["revoked_at"] {
			t.Errorf("the repeat moved revoked_at")
		}
	})
}
