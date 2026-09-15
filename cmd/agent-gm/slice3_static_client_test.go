package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Owner-declared OAuth clients, spec section 9.3 and decision D20.
//
// The case they exist for is a connector that does not implement RFC 7591: it
// asks its operator to paste a `client_id` and then calls /oauth/authorize
// with it, which answers `invalid_client` until the owner declares one. These
// tests drive the whole path the way that connector does.

// declareClient creates one through the owner's route and returns its data.
func declareClient(t *testing.T, h *oauthHarness, admin string, body map[string]any) map[string]any {
	t.Helper()
	resp := h.postJSON("/v1/admin/clients", body, bearer(admin))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("declaring a client: %d %s", resp.StatusCode, readBody(t, resp))
	}
	return dataOf(t, resp)
}

// The whole flow for a client that was declared rather than registered, and
// that omits `resource` throughout -- which is exactly what the connectors
// this feature exists for do.
//
// Plant: drop the `p.Resource == "" && client.DefaultResource` branch from
// validateAuthorize and the authorization screen answers invalid_target
// instead of rendering.
func TestSlice3AnOwnerDeclaredClientCompletesTheFlowWithoutResource(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	const redirect = "https://connector.example.test/api/oauth/callback"

	declared := declareClient(t, h, admin, map[string]any{
		"client_id":        "muse",
		"name":             "a declared connector",
		"redirect_uris":    []string{redirect},
		"default_resource": true,
	})
	if declared["id"] != "muse" {
		t.Fatalf("the declared client came back as %v, want muse", declared["id"])
	}
	if declared["static"] != true {
		t.Errorf("the declared client does not report static: %v", declared)
	}
	// It is created activated and with no expiry, so nothing sweeps it.
	if declared["expires_at"] != nil {
		t.Errorf("a declared client carries an expiry: %v", declared["expires_at"])
	}
	if declared["activated_at"] == nil {
		t.Error("a declared client is not activated, so the 24-hour sweep could take it")
	}

	// The authorization screen renders with NO `resource` at all.
	_, codeValue := h.enrollmentCode(admin, map[string]any{
		"label":  "a declared connector",
		"scopes": []string{"messages:read", "messages:write"},
	})
	verifier, challenge := pkce(t)
	page := h.authorizePage(authorizeParams{
		ClientID: "muse", RedirectURI: redirect, State: "a-state",
		Challenge: challenge, NoResource: true,
	})
	form := hiddenFields(t, page)

	// The hidden field carries the CANONICAL resource, not an empty string:
	// the default is applied before the context is signed, so the form echo,
	// the code and the token are all bound to the same audience a client-sent
	// value would have produced.
	if got := form.Get("resource"); got != oauthTestIssuer+"/mcp" {
		t.Fatalf("the approval form echoes resource=%q, want %q",
			got, oauthTestIssuer+"/mcp")
	}

	form.Add("scope_selected", "messages:read")
	form.Add("scope_selected", "messages:write")
	form.Set("enrollment_code", codeValue)

	resp := h.postForm("/oauth/authorize", form, h.withCookie)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting the approval form: %d\n%s", resp.StatusCode, readBody(t, resp))
	}
	requestID := strings.TrimPrefix(resp.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(t, resp)

	approve := h.postJSON("/v1/admin/authorization-requests/"+requestID+"/approve",
		map[string]any{}, bearer(admin))
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approving: %d %s", approve.StatusCode, readBody(t, approve))
	}
	_ = readBody(t, approve)

	complete := h.postForm("/oauth/requests/"+requestID+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	if complete.StatusCode != http.StatusSeeOther {
		t.Fatalf("completing: %d\n%s", complete.StatusCode, readBody(t, complete))
	}
	location, err := url.Parse(complete.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, complete)
	if !strings.HasPrefix(location.String(), redirect) {
		t.Fatalf("the callback went to %s, not the declared redirect", location)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("the callback carries no code: %s", location)
	}

	// The token exchange, also with no `resource`.
	tokenResp := h.exchange(flowResult{
		ClientID: "muse", Code: code, Verifier: verifier,
		RedirectURI: redirect,
	}, nil)
	if tokenResp.StatusCode != http.StatusOK {
		t.Fatalf("the token exchange failed: %d\n%s",
			tokenResp.StatusCode, readBody(t, tokenResp))
	}
	tokens := tokenValues(t, tokenResp)
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("the exchange returned no access token: %v", tokens)
	}

	// The token works, which is what makes this a connector and not a screen.
	if who := h.get("/v1/auth/whoami", bearer(access)); who.StatusCode != http.StatusOK {
		t.Fatalf("the declared client's access token answered %d at whoami", who.StatusCode)
	}

	// Declaring one is an administrative act and leaves an audit row.
	audit := h.get("/v1/admin/audit?kind=client.created", bearer(admin))
	if body := readBody(t, audit); !strings.Contains(body, "muse") {
		t.Errorf("no client.created audit row for the declared client:\n%s", body)
	}
}

// A DYNAMICALLY registered client may not omit `resource`. The relaxation is
// per client and never a default, so the ordinary path is unchanged.
//
// Plant: default the resource for every client rather than only a declared
// one, and this stops redirecting with invalid_target.
func TestSlice3ADynamicClientStillMustSendResource(t *testing.T) {
	h := newOAuthHarness(t)
	const redirect = "http://127.0.0.1:41997/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(t)

	resp := h.get(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "a-state",
		Challenge: challenge, NoResource: true,
	}.query(h))
	// An authorization error redirects with 302 (RFC 6749 section 4.1.2.1),
	// unlike the approved flow's own 303s.
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a dynamic client omitting resource got %d, want a 302 carrying "+
			"invalid_target\n%s", resp.StatusCode, readBody(t, resp))
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	if got := location.Query().Get("error"); got != "invalid_target" {
		t.Errorf("error=%q, want invalid_target", got)
	}
}

// A declared client WITHOUT default_resource is treated exactly like a
// dynamic one: declaring a client is not itself the relaxation.
func TestSlice3ADeclaredClientWithoutTheFlagStillMustSendResource(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	const redirect = "https://strict.example.test/cb"
	declareClient(t, h, admin, map[string]any{
		"client_id":     "strict",
		"redirect_uris": []string{redirect},
	})
	_, challenge := pkce(t)

	resp := h.get(authorizeParams{
		ClientID: "strict", RedirectURI: redirect, State: "a-state",
		Challenge: challenge, NoResource: true,
	}.query(h))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("got %d, want a 302 carrying invalid_target\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	location, _ := url.Parse(resp.Header.Get("Location"))
	_ = readBody(t, resp)
	if got := location.Query().Get("error"); got != "invalid_target" {
		t.Errorf("error=%q, want invalid_target", got)
	}
}

// The refusals at POST /v1/admin/clients.
func TestSlice3DeclaringAClientIsValidated(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	// The client_ prefix is reserved for what this server mints.
	for _, body := range []map[string]any{
		{"client_id": "client_01ab", "redirect_uris": []string{"https://a.example.test/cb"}},
		{"client_id": "", "redirect_uris": []string{"https://a.example.test/cb"}},
		{"client_id": "has space", "redirect_uris": []string{"https://a.example.test/cb"}},
		{"client_id": "ok", "redirect_uris": []string{}},
		{"client_id": "ok", "redirect_uris": []string{"http://evil.example.test/cb"}},
		{"client_id": "ok", "redirect_uris": []string{"https://a.example.test/cb#frag"}},
	} {
		resp := h.postJSON("/v1/admin/clients", body, bearer(admin))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v answered %d, want 400\n%s", body, resp.StatusCode, readBody(t, resp))
			continue
		}
		_ = readBody(t, resp)
	}

	// A duplicate is refused rather than silently updated.
	ok := map[string]any{
		"client_id":     "muse",
		"redirect_uris": []string{"https://connector.example.test/cb"},
	}
	declareClient(t, h, admin, ok)
	again := h.postJSON("/v1/admin/clients", ok, bearer(admin))
	if again.StatusCode != http.StatusBadRequest {
		t.Errorf("re-declaring answered %d, want 400\n%s", again.StatusCode, readBody(t, again))
	} else {
		_ = readBody(t, again)
	}

	// It is admin-scoped: there is no self-service here.
	f := h.fullFlow("http://127.0.0.1:41996/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatal("the fixture flow returned no access token")
	}
	denied := h.postJSON("/v1/admin/clients", map[string]any{
		"client_id": "sneaky", "redirect_uris": []string{"https://b.example.test/cb"},
	}, bearer(access))
	if denied.StatusCode == http.StatusOK {
		t.Error("an OAuth access token declared a client; the route is admin-only")
	}
	_ = readBody(t, denied)
}
