package main

import (
	"net/http"
	"net/url"
	"testing"
)

// `resource` at POST /oauth/token, spec section 9.6.
//
// The rule has two halves and the second one is the one that is easy to get
// wrong: a `resource` that is not this server's is `invalid_target`, and the
// refusal must NOT spend the authorization code. A wrong audience is an
// honest client's misconfiguration -- it is not a guess at anything -- so
// burning the code would send a working connector back through an
// authorization the owner already approved. Only a failed BINDING check
// (PKCE, redirect URI, client) consumes the code, because only that one is
// something an attacker could retry.
//
// The third case is the one RFC 8707 section 2.2 permits for a
// single-audience server: `resource` is mandatory at /oauth/authorize and
// OPTIONAL here, so an omitted value is read as the canonical resource.
//
// Plant: drop the `resource != "" && resource != s.Resource()` guard from
// grantAuthorizationCode and the wrong-audience exchange succeeds instead of
// answering invalid_target.
func TestSlice3TokenResourceIsCheckedWithoutSpendingTheCode(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:41999/callback")

	// A wrong audience is invalid_target.
	for _, wrong := range []string{
		"https://gm.example.com/mcp", // another server entirely
		oauthTestIssuer,              // the issuer, which is not the resource
		oauthTestIssuer + "/mcp/",    // a trailing slash is a different audience
		oauthTestIssuer + "/mcp?x=1", // and so is a query
	} {
		resp := h.exchange(f, url.Values{"resource": {wrong}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("resource=%q: %d, want 400\n%s", wrong, resp.StatusCode, readBody(t, resp))
		}
		body := tokenValues(t, resp)
		if body["error"] != "invalid_target" {
			t.Errorf("resource=%q: error is %v, want invalid_target", wrong, body["error"])
		}
		if body["access_token"] != nil {
			t.Errorf("resource=%q: a refused exchange returned a token", wrong)
		}
	}

	// The SAME code, with the right audience, still works. This is the whole
	// point: a wrong `resource` is refused without spending anything.
	resp := h.exchange(f, url.Values{"resource": {oauthTestIssuer + "/mcp"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the code was spent by the refusals: %d\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	tokens := tokenValues(t, resp)
	if access, _ := tokens["access_token"].(string); access == "" {
		t.Fatalf("the exchange returned no access token: %v", tokens)
	}
}

// `resource` omitted at the token endpoint succeeds: section 9.6 makes it
// optional there, and a single-audience server reads its absence as the
// canonical resource.
//
// Plant: make `resource` required in grantAuthorizationCode -- add it to the
// "code, client_id, redirect_uri and code_verifier are all required" list --
// and this fails with invalid_request.
func TestSlice3TokenResourceIsOptional(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:41998/callback")

	resp := h.exchange(f, nil) // no `resource` at all
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an omitted resource was refused: %d\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	tokens := tokenValues(t, resp)
	if access, _ := tokens["access_token"].(string); access == "" {
		t.Fatalf("the exchange returned no access token: %v", tokens)
	}
	if refresh, _ := tokens["refresh_token"].(string); refresh == "" {
		t.Fatalf("the exchange returned no refresh token: %v", tokens)
	}
}
