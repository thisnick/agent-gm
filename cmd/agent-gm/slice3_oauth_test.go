package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/mcp"
	"github.com/thisnick/agent-gm/internal/oauth"
)

// Spec section 16 Slice 3, acceptance tests 1 to 13 and 18. Every one of them
// drives `buildServer`'s own handler, so a rule that `internal/oauth` obeys
// but `serve` never mounts fails here rather than in production.

// Test 1. `issuer` equals https://gm.example.test BYTE FOR BYTE and
// `resource` equals it plus /mcp with no trailing-slash drift, asserted as
// STRING EQUALITY on both discovery documents.
//
// String equality is the assertion, not parsed-URL equivalence: a client that
// fetched the metadata and compared the issuer to the one it asked for will
// reject a mismatch, and two URLs that parse the same are not the same bytes.
//
// Plant: return `strings.TrimRight(publicURL, "/") + "/"` from Issuer and
// this fails naming both documents. Planted 2026-09-06.
func TestSlice3Test1DiscoveryDocumentsAreByteExact(t *testing.T) {
	h := newOAuthHarness(t)

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		resp := h.get(path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		var doc struct {
			Resource              string   `json:"resource"`
			AuthorizationServers  []string `json:"authorization_servers"`
			ScopesSupported       []string `json:"scopes_supported"`
			BearerMethods         []string `json:"bearer_methods_supported"`
			ResourceDocumentation string   `json:"resource_documentation"`
		}
		if err := json.Unmarshal([]byte(readBody(t, resp)), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Resource != oauthTestIssuer+"/mcp" {
			t.Errorf("%s: resource is %q, want %q", path, doc.Resource, oauthTestIssuer+"/mcp")
		}
		if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != oauthTestIssuer {
			t.Errorf("%s: authorization_servers is %v, want [%q]",
				path, doc.AuthorizationServers, oauthTestIssuer)
		}
		if strings.Join(doc.ScopesSupported, " ") != "messages:read messages:write messages:delete" {
			t.Errorf("%s: scopes_supported is %v", path, doc.ScopesSupported)
		}
		if len(doc.BearerMethods) != 1 || doc.BearerMethods[0] != "header" {
			t.Errorf("%s: bearer_methods_supported is %v", path, doc.BearerMethods)
		}
	}

	resp := h.get("/.well-known/oauth-authorization-server")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorization server metadata: %d", resp.StatusCode)
	}
	var as map[string]any
	if err := json.Unmarshal([]byte(readBody(t, resp)), &as); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"issuer":                 oauthTestIssuer,
		"authorization_endpoint": oauthTestIssuer + "/oauth/authorize",
		"token_endpoint":         oauthTestIssuer + "/oauth/token",
		"registration_endpoint":  oauthTestIssuer + "/oauth/register",
		"revocation_endpoint":    oauthTestIssuer + "/oauth/revoke",
	}
	for key, expected := range want {
		if got, _ := as[key].(string); got != expected {
			t.Errorf("%s is %q, want %q", key, got, expected)
		}
	}
	if as["authorization_response_iss_parameter_supported"] != true {
		t.Error("authorization_response_iss_parameter_supported must be true (RFC 9207)")
	}
	if methods, _ := as["token_endpoint_auth_methods_supported"].([]any); len(methods) != 1 ||
		methods[0] != "none" {
		t.Errorf("token_endpoint_auth_methods_supported is %v, want [none]", as["token_endpoint_auth_methods_supported"])
	}
	if challenge, _ := as["code_challenge_methods_supported"].([]any); len(challenge) != 1 ||
		challenge[0] != "S256" {
		t.Errorf("code_challenge_methods_supported is %v, want [S256]", as["code_challenge_methods_supported"])
	}
}

// Test 2. An unauthenticated /mcp request answers 401 with the exact
// WWW-Authenticate of section 9.2; a valid token carrying no messaging scope
// answers 403 insufficient_scope with the SAME challenge.
//
// The two are deliberately distinct: a caller with no credential is told to
// get one, and a caller whose credential is too narrow is told which scope it
// would have needed. Collapsing them would send a connector back through
// authorization it has already completed.
func TestSlice3Test2MCPChallenge(t *testing.T) {
	h := newOAuthHarness(t)

	// The challenge FORMAT is the SDK's since decision D36, which is why
	// there is no `realm` parameter: `auth.RequireBearerToken` does not emit
	// one. Written out here rather than taken from mcp.Challenge, so that a
	// change to that function has to be made twice and meant twice.
	wantChallenge := `Bearer resource_metadata="` +
		oauthTestIssuer + `/.well-known/oauth-protected-resource/mcp", ` +
		`scope="messages:read messages:write"`

	resp := h.postJSON("/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}, func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream") })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated /mcp request answered %d, want 401\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != wantChallenge {
		t.Errorf("the 401 challenge is\n  %q\nwant\n  %q", got, wantChallenge)
	}
	_ = readBody(t, resp)

	// A valid token with no messaging scope. The admin bootstrap can be
	// narrowed to `admin` alone, which is exactly a credential that is valid
	// and carries no messaging scope.
	mint := h.postJSON("/v1/auth/admin-session", map[string]any{
		"secret": oauthTestAdminSecret,
		"scopes": []string{"admin"},
	})
	if mint.StatusCode != http.StatusOK {
		t.Fatalf("minting an admin-only session: %d %s", mint.StatusCode, readBody(t, mint))
	}
	token, _ := dataOf(t, mint)["access_token"].(string)

	resp = h.postJSON("/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}, bearer(token), func(r *http.Request) {
		r.Header.Set("Accept", "application/json, text/event-stream")
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a token with no messaging scope answered %d, want 403\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != wantChallenge {
		t.Errorf("the 403 challenge is\n  %q\nwant\n  %q", got, wantChallenge)
	}
	_ = readBody(t, resp)
}

// Test 3. Dynamic client registration, every row of section 9.3's table.
func TestSlice3Test3DynamicClientRegistration(t *testing.T) {
	h := newOAuthHarness(t)

	refused := []struct {
		name string
		body map[string]any
	}{
		{"a token_endpoint_auth_method other than none", map[string]any{
			"redirect_uris":              []string{"https://client.example/cb"},
			"token_endpoint_auth_method": "client_secret_basic",
		}},
		{"a client-chosen client_id", map[string]any{
			"redirect_uris": []string{"https://client.example/cb"},
			"client_id":     "client_i-picked-this",
		}},
		{"a grant type outside the two", map[string]any{
			"redirect_uris": []string{"https://client.example/cb"},
			"grant_types":   []string{"authorization_code", "password"},
		}},
		{"a response type other than code", map[string]any{
			"redirect_uris":  []string{"https://client.example/cb"},
			"response_types": []string{"token"},
		}},
		{"eleven redirect URIs", map[string]any{"redirect_uris": elevenRedirects()}},
		{"a 501-character redirect URI", map[string]any{
			"redirect_uris": []string{longRedirect(501)},
		}},
		{"http on a host that merely contains localhost", map[string]any{
			"redirect_uris": []string{"http://localhost.evil.example/cb"},
		}},
		{"http://notlocalhost/cb", map[string]any{
			"redirect_uris": []string{"http://notlocalhost/cb"},
		}},
		{"http://local.host/cb", map[string]any{
			"redirect_uris": []string{"http://local.host/cb"},
		}},
		{"a userinfo that looks like localhost", map[string]any{
			"redirect_uris": []string{"http://localhost@evil.example/cb"},
		}},
		{"https with an IP literal", map[string]any{
			"redirect_uris": []string{"https://93.184.216.34/cb"},
		}},
		{"a redirect URI carrying a fragment", map[string]any{
			"redirect_uris": []string{"https://client.example/cb#done"},
		}},
	}
	for _, c := range refused {
		t.Run("refused/"+c.name, func(t *testing.T) {
			resp := h.postJSON("/oauth/register", c.body)
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("registering with %s answered %d, want a 4xx\n%s",
					c.name, resp.StatusCode, readBody(t, resp))
			}
			body := readBody(t, resp)
			if !strings.Contains(body, `"error"`) {
				t.Errorf("the refusal is not an OAuth error body: %s", body)
			}
		})
	}

	accepted := []struct {
		name, uri string
	}{
		{"a loopback literal", "http://127.0.0.1:53211/callback"},
		{"the IPv6 loopback", "http://[::1]:53211/callback"},
		{"the literal host name localhost", "http://localhost:1/cb"},
		{"https with a fully qualified host", "https://client.example/cb"},
		{"a private-use scheme with a dot", "com.example.client:/cb"},
	}
	for _, c := range accepted {
		t.Run("accepted/"+c.name, func(t *testing.T) {
			resp := h.postJSON("/oauth/register", map[string]any{
				"redirect_uris": []string{c.uri},
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("registering %s answered %d\n%s", c.uri, resp.StatusCode, readBody(t, resp))
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(readBody(t, resp)), &out); err != nil {
				t.Fatal(err)
			}
			if _, ok := out["client_secret"]; ok {
				t.Error("a registration answered with a client_secret; this server registers " +
					"public clients only and there is no secret to leak")
			}
			if out["token_endpoint_auth_method"] != "none" {
				t.Errorf("token_endpoint_auth_method is %v, want none", out["token_endpoint_auth_method"])
			}
		})
	}

	// The loopback port rule of RFC 8252 section 7.3, at authorization time.
	t.Run("a registered loopback redirect matches any port and nothing else", func(t *testing.T) {
		clientID := h.register("http://127.0.0.1/cb")
		_, challenge := pkce(t)
		cases := []struct {
			redirect string
			ok       bool
		}{
			{"http://127.0.0.1:53211/cb", true},
			{"http://127.0.0.1/cb", true},
			{"http://127.0.0.1:53211/cb2", false},
			{"http://127.0.0.2/cb", false},
		}
		for _, c := range cases {
			resp := h.get(authorizeParams{
				ClientID: clientID, RedirectURI: c.redirect, State: "s",
				Challenge: challenge,
			}.query(h))
			body := readBody(t, resp)
			if c.ok && resp.StatusCode != http.StatusOK {
				t.Errorf("%s should have matched the registration, got %d\n%s",
					c.redirect, resp.StatusCode, body)
			}
			if !c.ok && resp.StatusCode == http.StatusOK {
				t.Errorf("%s matched the registration and must not have", c.redirect)
			}
			if !c.ok && resp.Header.Get("Location") != "" {
				t.Errorf("%s was refused with a REDIRECT to %q; an unregistered redirect "+
					"must never be redirected to (RFC 6749 4.1.2.1)",
					c.redirect, resp.Header.Get("Location"))
			}
		}
	})
}

func elevenRedirects() []string {
	out := make([]string, 11)
	for i := range out {
		out[i] = "https://client.example/cb" + string(rune('a'+i))
	}
	return out
}

func longRedirect(n int) string {
	const prefix = "https://client.example/"
	return prefix + strings.Repeat("a", n-len(prefix))
}

// Test 4. The authorization endpoint's refusal table.
//
// An unknown client and an unregistered redirect answer 4xx and DO NOT
// redirect; every later failure redirects with `error`, `state` and a
// byte-exact `iss`.
func TestSlice3Test4Authorize(t *testing.T) {
	h := newOAuthHarness(t)
	const redirect = "http://127.0.0.1:53211/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(t)

	t.Run("an unknown client does not redirect", func(t *testing.T) {
		resp := h.get(authorizeParams{
			ClientID: "client_nobody", RedirectURI: redirect, State: "s", Challenge: challenge,
		}.query(h))
		defer func() { _ = readBody(t, resp) }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("an unknown client answered %d, want 400", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("an unknown client was redirected to %q", loc)
		}
	})

	t.Run("an unregistered redirect does not redirect", func(t *testing.T) {
		resp := h.get(authorizeParams{
			ClientID: clientID, RedirectURI: "https://evil.example/cb", State: "s",
			Challenge: challenge,
		}.query(h))
		defer func() { _ = readBody(t, resp) }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("an unregistered redirect answered %d, want 400", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("an unregistered redirect was redirected to %q", loc)
		}
	})

	redirecting := []struct {
		name   string
		params authorizeParams
		want   string
	}{
		{"a response_type other than code", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
		}, "unsupported_response_type"},
		{"a missing state", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, Challenge: challenge,
		}, "invalid_request"},
		{"an omitted code_challenge_method", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
			NoMethod: true,
		}, "invalid_request"},
		{"a plain code_challenge_method", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
			Method: "plain",
		}, "invalid_request"},
		{"a malformed challenge", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: "short",
		}, "invalid_request"},
		{"a resource other than the canonical one", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
			Resource: "https://gm.example.test/mcp/",
		}, "invalid_target"},
		{"an unknown scope", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
			Scope: "messages:reed",
		}, "invalid_scope"},
		{"scope=admin", authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
			Scope: "admin",
		}, "invalid_scope"},
	}
	for _, c := range redirecting {
		t.Run(c.name, func(t *testing.T) {
			query := c.params.query(h)
			if c.name == "a response_type other than code" {
				query = strings.Replace(query, "response_type=code", "response_type=token", 1)
			}
			resp := h.get(query)
			defer func() { _ = readBody(t, resp) }()
			loc := resp.Header.Get("Location")
			if loc == "" {
				t.Fatalf("%s answered %d with no redirect; every failure after the "+
					"callback is verified must redirect", c.name, resp.StatusCode)
			}
			u, err := url.Parse(loc)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("error"); got != c.want {
				t.Errorf("%s redirected with error=%q, want %q", c.name, got, c.want)
			}
			if iss := u.Query().Get("iss"); iss != oauthTestIssuer {
				t.Errorf("%s redirected with iss=%q, want the byte-exact issuer %q",
					c.name, iss, oauthTestIssuer)
			}
			if c.params.State != "" && u.Query().Get("state") != c.params.State {
				t.Errorf("%s dropped state", c.name)
			}
		})
	}
}

// Test 5. An invalid enrollment code re-renders the form with ONE generic
// message, byte-identical for unknown, expired, revoked and consumed; creates
// no pending request; and consumes nothing.
//
// Byte-identical is the assertion. Four messages that merely "look similar"
// are four ways to tell an attacker which of their guesses was once a real
// code.
func TestSlice3Test5InvalidEnrollmentCodeIsOneGenericMessage(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	const redirect = "http://127.0.0.1:53211/callback"
	clientID := h.register(redirect)

	// A revoked code, and a consumed one.
	revokedID, revokedValue := h.enrollmentCode(admin, nil)
	resp := h.do(mustRequest(t, http.MethodDelete,
		h.http.URL+"/v1/admin/enrollment-codes/"+revokedID+"?reason=test", bearer(admin)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoking: %d %s", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	consumed := h.fullFlow("http://127.0.0.1:53212/callback")
	_ = consumed

	// A code that expires immediately is not constructible through the
	// public route (the floor is one minute), so `expired` is covered by the
	// unknown and revoked cases plus the single code path they share:
	// enrollmentUsable folds all four into one boolean at one place.
	cases := map[string]string{
		"unknown": "ZZZZ-ZZZZ-ZZZZ-ZZZZ",
		"revoked": revokedValue,
	}

	var messages []string
	for name, value := range cases {
		_, challenge := pkce(t)
		page := h.authorizePage(authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
		})
		form := hiddenFields(t, page)
		form.Add("scope_selected", "messages:read")
		form.Set("enrollment_code", value)

		before := h.pendingRequests(admin)
		submit := h.postForm("/oauth/authorize", form, h.withCookie)
		if submit.StatusCode != http.StatusOK {
			t.Fatalf("%s: an invalid code answered %d, want a 200 re-render",
				name, submit.StatusCode)
		}
		body := readBody(t, submit)
		if !strings.Contains(body, oauth.GenericEnrollmentFailure) {
			t.Fatalf("%s: the re-rendered page does not carry the generic failure line", name)
		}
		messages = append(messages, extractAlert(t, body))
		if after := h.pendingRequests(admin); after != before {
			t.Errorf("%s: an invalid code created a pending request (%d -> %d)",
				name, before, after)
		}
	}
	for i := 1; i < len(messages); i++ {
		if messages[i] != messages[0] {
			t.Errorf("the refusal messages differ:\n  %q\n  %q", messages[0], messages[i])
		}
	}

	// Nothing was consumed: the revoked code's row is still unconsumed.
	show := h.get("/v1/admin/enrollment-codes/"+revokedID, bearer(admin))
	data := dataOf(t, show)
	if data["consumed_at"] != nil {
		t.Errorf("a refused submission consumed the code: consumed_at is %v", data["consumed_at"])
	}
}

var alertLine = regexp.MustCompile(`<p role="alert">([^<]*)</p>`)

func extractAlert(t *testing.T, page string) string {
	t.Helper()
	m := alertLine.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page carries no alert line:\n%s", page)
	}
	return m[1]
}

func (h *oauthHarness) pendingRequests(admin string) int {
	h.t.Helper()
	resp := h.get("/v1/admin/authorization-requests?status=pending", bearer(admin))
	data := dataOf(h.t, resp)
	items, _ := data["items"].([]any)
	return len(items)
}

func mustRequest(t *testing.T, method, url string, opts ...func(*http.Request)) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	return req
}

// Test 6. The eleventh failed code attempt within 15 minutes is 429 with
// Retry-After, per signed context AND per source -- and loading a fresh
// authorization page does not reset the source bucket.
//
// The last clause is the one that matters. A limiter keyed only on the signed
// context is a limiter an attacker resets by reloading the page.
func TestSlice3Test6EnrollmentAttemptsAreBudgeted(t *testing.T) {
	h := newOAuthHarness(t)
	const redirect = "http://127.0.0.1:53211/callback"
	clientID := h.register(redirect)

	submit := func() *http.Response {
		_, challenge := pkce(t)
		page := h.authorizePage(authorizeParams{
			ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
		})
		form := hiddenFields(t, page)
		form.Add("scope_selected", "messages:read")
		form.Set("enrollment_code", "ZZZZ-ZZZZ-ZZZZ-ZZZZ")
		return h.postForm("/oauth/authorize", form, h.withCookie)
	}

	// Ten failures, each on a FRESH authorization page, so only the
	// per-source bucket can be counting them.
	for i := 1; i <= 10; i++ {
		resp := submit()
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d answered %d, want a 200 re-render\n%s", i, resp.StatusCode, body)
		}
	}
	resp := submit()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the eleventh failed attempt answered %d, want 429. A fresh "+
			"authorization page must not reset the per-source bucket\n%s",
			resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After; a 429 without one tells a client to guess")
	}
}

// Test 7. /oauth/requests/{id} and /status answer 404 WITHOUT the context
// cookie. The request ID alone conveys no authority.
func TestSlice3Test7RequestRoutesNeedTheCookie(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	_, codeValue := h.enrollmentCode(admin, nil)
	const redirect = "http://127.0.0.1:53211/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(t)

	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
	})
	form := hiddenFields(t, page)
	form.Add("scope_selected", "messages:read")
	form.Set("enrollment_code", codeValue)
	resp := h.postForm("/oauth/authorize", form, h.withCookie)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting: %d\n%s", resp.StatusCode, readBody(t, resp))
	}
	id := strings.TrimPrefix(resp.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(t, resp)

	for _, path := range []string{"/oauth/requests/" + id, "/oauth/requests/" + id + "/status"} {
		withCookie := h.get(path, h.withCookie)
		if withCookie.StatusCode != http.StatusOK {
			t.Errorf("%s with the cookie answered %d, want 200", path, withCookie.StatusCode)
		}
		_ = readBody(t, withCookie)

		without := h.get(path)
		if without.StatusCode != http.StatusNotFound {
			t.Errorf("%s WITHOUT the cookie answered %d, want 404. The request ID alone "+
				"conveys no authority, and a 403 would confirm that it exists",
				path, without.StatusCode)
		}
		_ = readBody(t, without)
	}
}

// Test 8. A replayed authorization code is invalid_grant AND revokes the
// tokens the first exchange produced. A wrong PKCE verifier is invalid_grant
// AND consumes the code.
//
// Both halves matter and both are easy to half-implement. A replay that only
// refused the second attempt would leave the tokens the leaked code already
// bought; a wrong verifier that did not consume the code would let the
// challenge be brute-forced by retrying.
func TestSlice3Test8CodeReplayRevokesAndWrongVerifierConsumes(t *testing.T) {
	t.Run("a replayed code revokes what the first exchange produced", func(t *testing.T) {
		h := newOAuthHarness(t)
		f := h.fullFlow("http://127.0.0.1:53211/callback")

		first := h.exchange(f, nil)
		if first.StatusCode != http.StatusOK {
			t.Fatalf("the first exchange answered %d\n%s", first.StatusCode, readBody(t, first))
		}
		tokens := tokenValues(t, first)
		access, _ := tokens["access_token"].(string)
		if access == "" {
			t.Fatal("the exchange returned no access token")
		}
		// The token works.
		if resp := h.get("/v1/auth/whoami", bearer(access)); resp.StatusCode != http.StatusOK {
			t.Fatalf("the freshly minted token answered %d at whoami\n%s",
				resp.StatusCode, readBody(t, resp))
		}

		replay := h.exchange(f, nil)
		body := readBody(t, replay)
		if replay.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
			t.Fatalf("the replay answered %d %s, want 400 invalid_grant", replay.StatusCode, body)
		}

		after := h.get("/v1/auth/whoami", bearer(access))
		if after.StatusCode != http.StatusUnauthorized {
			t.Errorf("after the replay the FIRST exchange's token still answers %d at "+
				"whoami; section 9.6 requires the replay to revoke it", after.StatusCode)
		}
		_ = readBody(t, after)
	})

	t.Run("a wrong verifier consumes the code", func(t *testing.T) {
		h := newOAuthHarness(t)
		f := h.fullFlow("http://127.0.0.1:53211/callback")

		wrong := h.exchange(f, url.Values{"code_verifier": {"a-verifier-that-does-not-hash-to-the-challenge"}})
		body := readBody(t, wrong)
		if wrong.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
			t.Fatalf("a wrong verifier answered %d %s, want 400 invalid_grant",
				wrong.StatusCode, body)
		}

		// The code is now spent: the RIGHT verifier no longer works.
		retry := h.exchange(f, nil)
		retryBody := readBody(t, retry)
		if retry.StatusCode == http.StatusOK {
			t.Fatal("after a wrong verifier the correct one still exchanged the code; " +
				"section 9.6 requires the failure to consume it, so a verifier cannot " +
				"be guessed by retrying")
		}
		if !strings.Contains(retryBody, "invalid_grant") {
			t.Errorf("the retry answered %s, want invalid_grant", retryBody)
		}
	})
}

// Test 9. Refresh rotates; reusing a spent refresh token revokes the family
// and the authorization in one transaction, with its audit row; a widening
// `scope` is invalid_scope and DOES NOT SPEND the presented token.
func TestSlice3Test9RefreshRotatesAndReuseRevokes(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	f := h.fullFlow("http://127.0.0.1:53211/callback",
		"messages:read", "messages:write", "messages:delete")

	exchanged := tokenValues(t, h.exchange(f, nil))
	refresh, _ := exchanged["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("the exchange returned no refresh token")
	}

	// A widening scope is refused and does not spend the token.
	widen := h.postForm("/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {f.ClientID},
		"scope":         {"messages:read messages:write messages:delete admin"},
	})
	widenBody := readBody(t, widen)
	if widen.StatusCode != http.StatusBadRequest || !strings.Contains(widenBody, "invalid_scope") {
		t.Fatalf("a widening scope answered %d %s, want 400 invalid_scope",
			widen.StatusCode, widenBody)
	}

	// The token was NOT spent: the ordinary refresh still works.
	rotated := h.postForm("/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {f.ClientID},
	})
	if rotated.StatusCode != http.StatusOK {
		t.Fatalf("after a refused widening the presented token no longer works (%d); "+
			"section 9.6 says a widening does not spend it\n%s",
			rotated.StatusCode, readBody(t, rotated))
	}
	next := tokenValues(t, rotated)
	rotatedRefresh, _ := next["refresh_token"].(string)
	if rotatedRefresh == "" || rotatedRefresh == refresh {
		t.Fatal("the refresh token did not rotate")
	}
	rotatedAccess, _ := next["access_token"].(string)

	// Reuse of the spent token revokes the whole family and the
	// authorization: the access token minted from the rotation stops working.
	reuse := h.postForm("/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {f.ClientID},
	})
	reuseBody := readBody(t, reuse)
	if reuse.StatusCode != http.StatusBadRequest || !strings.Contains(reuseBody, "invalid_grant") {
		t.Fatalf("reusing a spent refresh token answered %d %s, want 400 invalid_grant",
			reuse.StatusCode, reuseBody)
	}
	after := h.get("/v1/auth/whoami", bearer(rotatedAccess))
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("after refresh-token reuse the rotated access token still answers %d; "+
			"reuse must revoke the whole family and its authorization", after.StatusCode)
	}
	_ = readBody(t, after)

	// And the audit row is there.
	audit := h.get("/v1/admin/audit?kind_prefix=auth.", bearer(admin))
	auditBody := readBody(t, audit)
	if !strings.Contains(auditBody, "refresh_token_reuse_detected") {
		t.Errorf("no auth.refresh_token_reuse_detected audit row was written:\n%s", auditBody)
	}
}

// Test 10. /oauth/revoke ALWAYS answers 200, including for a token belonging
// to nobody, and never confirms existence.
func TestSlice3Test10RevokeAlwaysAnswers200(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)

	cases := []struct {
		name  string
		form  url.Values
		alive bool
	}{
		{"a token belonging to nobody", url.Values{
			"token": {"a-value-that-was-never-issued"}, "client_id": {f.ClientID},
		}, true},
		{"another client's client_id", url.Values{
			"token": {access}, "client_id": {"client_somebody-else"},
		}, true},
		{"the real token and the real client", url.Values{
			"token": {access}, "client_id": {f.ClientID},
		}, false},
	}
	for _, c := range cases {
		resp := h.postForm("/oauth/revoke", c.form)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s answered %d, want 200 always\n%s", c.name, resp.StatusCode, body)
		}
		if strings.TrimSpace(body) != "" {
			t.Errorf("%s answered with a body (%q); an answer that says anything is an "+
				"oracle for whether the value exists", c.name, body)
		}
		whoami := h.get("/v1/auth/whoami", bearer(access))
		alive := whoami.StatusCode == http.StatusOK
		_ = readBody(t, whoami)
		if alive != c.alive {
			t.Errorf("after %s the token alive=%v, want %v", c.name, alive, c.alive)
		}
	}
}

// Test 11, the HTTP half. Guessing at /oauth/token is refused as
// `invalid_grant`, and the budgets of section 9.8 refuse the caller with a
// 429 that carries Retry-After long before thirty guesses are possible.
//
// Section 9.8 puts TWO budgets in front of this endpoint: 60 requests a
// minute with a burst of 20, in memory, and a durable limit of 30 invalid
// presented tokens per 15 minutes. The in-memory one bites first by design --
// it is the cheap one -- so an HTTP-level test can only reach the durable one
// by waiting out a minute, which is not a thing a test may do (section 13.1).
// The durable half, including that it survives a restart and that a
// successful presentation does not clear it, is
// TestDurableTokenBudgetSurvivesARestart in internal/authz, which runs on the
// injected clock over a real store that is closed and reopened.
//
// What this test proves is the part only the wired server can: that both
// budgets are actually in front of the endpoint, that the refusal is
// indistinguishable from an ordinary invalid grant until the budget trips,
// and that the 429 carries Retry-After.
func TestSlice3Test11GuessingAtTheTokenEndpointIsBudgeted(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:53211/callback")

	guess := func(n int) *http.Response {
		return h.postForm("/oauth/token", url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {"a-guess-" + strings.Repeat("x", n)},
			"client_id":     {f.ClientID},
		})
	}

	var limited *http.Response
	for i := 0; i < 40; i++ {
		resp := guess(i)
		body := readBody(t, resp)
		switch resp.StatusCode {
		case http.StatusBadRequest:
			if !strings.Contains(body, "invalid_grant") {
				t.Fatalf("guess %d answered %s, want invalid_grant", i, body)
			}
		case http.StatusTooManyRequests:
			limited = resp
		default:
			t.Fatalf("guess %d answered %d\n%s", i, resp.StatusCode, body)
		}
		if limited != nil {
			break
		}
	}
	if limited == nil {
		t.Fatal("forty guesses at /oauth/token were never rate limited; section 9.8 " +
			"budgets this endpoint precisely because the presented token IS the " +
			"credential and an attacker can guess at it")
	}
	if limited.Header.Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After; a 429 without one tells a client to guess, " +
			"and a guessing client retries too soon")
	}
}

// Test 12. The enrollment code's plaintext appears NOWHERE in the database
// file, the WAL, the shm, or a log; and the stored hash equals SHA-256 of the
// canonical form COMPUTED OUTSIDE THE CODEBASE.
//
// "Outside the codebase" is why this test hashes the value itself with
// crypto/sha256 over the canonical form rather than calling
// oauth.EnrollmentCodeHash: a test that called the implementation would agree
// with it however wrong it was.
func TestSlice3Test12EnrollmentCodePlaintextIsNowhere(t *testing.T) {
	dir := t.TempDir()
	h := newOAuthHarnessIn(t, dir)
	admin := h.adminToken()
	id, value := h.enrollmentCode(admin, nil)

	canonical := strings.ToUpper(strings.ReplaceAll(value, "-", ""))
	sum := sha256.Sum256([]byte(canonical))
	expected := hex.EncodeToString(sum[:])

	// The stored hash, read out of the database file itself.
	h.stop()
	stored := readStoredCodeHash(t, dir, id)
	if stored != expected {
		t.Errorf("enrollment_codes.code_hash is\n  %s\nand SHA-256 of the canonical form "+
			"%q computed here is\n  %s", stored, canonical, expected)
	}

	// And the plaintext is in none of the three files.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(dir, "agent-gm.sqlite3"+suffix)
		raw, err := os.ReadFile(path) //nolint:gosec // a test reading its own temporary directory
		if err != nil {
			continue // -wal and -shm need not exist
		}
		for _, form := range []string{value, canonical, strings.ToLower(canonical)} {
			if strings.Contains(string(raw), form) {
				t.Errorf("%s contains the enrollment code's plaintext (%s form)",
					filepath.Base(path), form)
			}
		}
	}
}

// readStoredCodeHash reads one column straight out of the database file with
// no Agent GM code in the way.
func readStoredCodeHash(t *testing.T, dir, id string) string {
	t.Helper()
	db := openRawDB(t, filepath.Join(dir, "agent-gm.sqlite3"))
	defer func() { _ = db.Close() }()
	var hash string
	if err := db.QueryRow(`SELECT code_hash FROM enrollment_codes WHERE id = ?`, id).
		Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// Test 13. An OAuth authorization can never hold `admin`, and an admin
// session can never be minted through /oauth/token; the two credential paths
// do not cross, and neither attempt revokes anything.
func TestSlice3Test13TheTwoCredentialPathsDoNotCross(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	t.Run("admin can never be enrolled", func(t *testing.T) {
		resp := h.postJSON("/v1/admin/enrollment-codes", map[string]any{
			"label": "an admin code", "scopes": []string{"admin"},
		}, bearer(admin))
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_request") {
			t.Fatalf("enrolling admin answered %d %s, want 400 invalid_request",
				resp.StatusCode, body)
		}
	})

	t.Run("an OAuth grant never carries admin", func(t *testing.T) {
		f := h.fullFlow("http://127.0.0.1:53211/callback")
		tokens := tokenValues(t, h.exchange(f, nil))
		access, _ := tokens["access_token"].(string)
		whoami := h.get("/v1/auth/whoami", bearer(access))
		data := dataOf(t, whoami)
		scopes, _ := data["scopes"].([]any)
		for _, s := range scopes {
			if s == "admin" {
				t.Fatalf("an OAuth authorization holds admin: %v", scopes)
			}
		}
		// And it cannot reach an admin route.
		adminRoute := h.get("/v1/admin/settings", bearer(access))
		if adminRoute.StatusCode != http.StatusForbidden {
			t.Errorf("an OAuth token reached /v1/admin/settings with %d, want 403",
				adminRoute.StatusCode)
		}
		_ = readBody(t, adminRoute)
	})

	t.Run("an admin refresh token is invalid_grant at /oauth/token", func(t *testing.T) {
		mint := h.postJSON("/v1/auth/admin-session", map[string]any{"secret": oauthTestAdminSecret})
		session := dataOf(t, mint)
		adminRefresh, _ := session["refresh_token"].(string)
		adminAccess, _ := session["access_token"].(string)

		resp := h.postForm("/oauth/token", url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {adminRefresh},
			"client_id":     {"client_anything"},
		})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
			t.Fatalf("an admin refresh token at /oauth/token answered %d %s, "+
				"want 400 invalid_grant", resp.StatusCode, body)
		}
		// And it revoked nothing: the admin session still works.
		whoami := h.get("/v1/auth/whoami", bearer(adminAccess))
		if whoami.StatusCode != http.StatusOK {
			t.Errorf("presenting an admin refresh token at /oauth/token revoked the "+
				"admin session (whoami answered %d); section 9.6 says neither attempt "+
				"revokes anything", whoami.StatusCode)
		}
		_ = readBody(t, whoami)
	})

	t.Run("an OAuth refresh token is refused at /v1/auth/refresh", func(t *testing.T) {
		f := h.fullFlow("http://127.0.0.1:53213/callback")
		tokens := tokenValues(t, h.exchange(f, nil))
		oauthRefresh, _ := tokens["refresh_token"].(string)
		access, _ := tokens["access_token"].(string)

		resp := h.postJSON("/v1/auth/refresh", map[string]any{"refresh_token": oauthRefresh})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an OAuth refresh token at /v1/auth/refresh answered %d %s, want 401",
				resp.StatusCode, body)
		}
		whoami := h.get("/v1/auth/whoami", bearer(access))
		if whoami.StatusCode != http.StatusOK {
			t.Errorf("presenting an OAuth refresh token at /v1/auth/refresh revoked the "+
				"OAuth grant (whoami answered %d)", whoami.StatusCode)
		}
		_ = readBody(t, whoami)
	})
}

// Test 18. The authorization screen renders the global-scope disclosure line
// VERBATIM, above the scope checkboxes, followed by the current accounts.
//
// Asserted as a string, which is what makes section 9.7's honesty claim
// testable rather than aspirational: scopes are global across accounts (D29),
// and the owner has to be told so before they tick a box.
func TestSlice3Test18TheScreenDisclosesGlobalScope(t *testing.T) {
	h := newOAuthHarness(t)
	const redirect = "http://127.0.0.1:53211/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(t)

	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
	})

	const want = "This will let the client read and send as any Google account on this " +
		"server, including accounts added later."
	if oauth.GlobalScopeDisclosure != want {
		t.Fatalf("the disclosure constant is\n  %q\nwant\n  %q", oauth.GlobalScopeDisclosure, want)
	}
	disclosure := strings.Index(page, want)
	if disclosure < 0 {
		t.Fatalf("the authorization screen does not carry the disclosure line verbatim:\n%s", page)
	}
	checkbox := strings.Index(page, `name="scope_selected"`)
	if checkbox < 0 {
		t.Fatal("the authorization screen renders no scope checkboxes")
	}
	if disclosure > checkbox {
		t.Error("the disclosure line is rendered BELOW the scope checkboxes; section 9.4 " +
			"puts it above them, because an owner who has already ticked a box has " +
			"already decided")
	}
}

// Test 28. GET /v1/health and MCP `serverInfo` report a `commit` equal to the
// built commit and a `source_url` that resolves -- the AGPL section 13
// obligation of section 1.4.
//
// The half that only the wired binary can prove is that the TWO SURFACES
// AGREE. A reviewer's run against the real binary found them disagreeing:
// `/v1/health` computed `<repo>/tree/<commit>` and `serverInfo` served the
// bare repository link, so the obligation was half kept and both surfaces'
// own tests were green. There is now one accessor and this test walks both.
func TestSlice3Test28BothSurfacesReportTheSameSource(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	health := dataOf(t, h.get("/v1/health", bearer(admin)))
	restCommit, _ := health["commit"].(string)
	restSource, _ := health["source_url"].(string)
	if restCommit == "" {
		t.Fatal("/v1/health reports no commit")
	}

	// The MCP half is read the way a connector reads it: with the official
	// SDK client, against the wired binary's handler. That matters here more
	// than anywhere -- the facts are on `serverInfo`, and `serverInfo` is
	// exactly what the SDK's client reconstructs for itself from
	// `server/discover` (decision D36). Reading the wire instead would assert
	// a licence obligation no client actually receives.
	info := h.mcpServerInfo(t, admin)
	if info.Name != "agent-gm" {
		t.Errorf("serverInfo.name is %v, want agent-gm", info.Name)
	}
	restVersion, _ := health["version"].(string)
	if want := mcp.ServerVersion(restVersion, restCommit); info.Version != want {
		t.Errorf("serverInfo.version is %q and /v1/health reports version %q at commit %q, "+
			"so the two surfaces disagree about what is running", info.Version, restVersion, restCommit)
	}
	source := info.WebsiteURL
	if source != restSource {
		t.Errorf("serverInfo reports websiteUrl\n  %q\nand /v1/health reports source_url\n  %q\n"+
			"Section 1.4's AGPL obligation is not kept by two surfaces answering "+
			"differently", source, restSource)
	}
	// Two rows, and the second is the one a reviewer found broken. A STAMPED
	// build names its commit; an UNSTAMPED one -- a test binary, a plain `go
	// build` with no VCS metadata -- falls back to the repository ROOT rather
	// than offering `<repo>/tree/unknown`, which is a 404. The AGPL section 13
	// obligation is to offer the source, and a dead link offers nothing.
	if restCommit == api.UnknownCommit {
		if restSource != "https://github.com/thisnick/agent-gm" {
			t.Errorf("an unstamped build reports source_url %q; it must be the repository "+
				"root, never .../tree/unknown", restSource)
		}
	} else if !strings.Contains(restSource, restCommit) {
		t.Errorf("source_url %q does not name the built commit %q; a link to `main` "+
			"is a link to code that is not what answered you", restSource, restCommit)
	}
}

// TestSlice3AnUnstampedBuildDoesNotOfferADeadLink pins both rows of
// api.SourceURL directly, because the test binary can only ever exercise one
// of them and the other is the one that ships.
//
// Plant: restore `return d.SourceURLBase + "/tree/" + d.Commit` unconditionally
// and this fails naming the dead link. Planted 2026-09-07 after reviewer
// finding R-9.
func TestSlice3AnUnstampedBuildDoesNotOfferADeadLink(t *testing.T) {
	const repo = "https://github.com/thisnick/agent-gm"
	cases := []struct {
		name, commit, want string
	}{
		{"a stamped build names its commit", "abc123", repo + "/tree/abc123"},
		{"an unstamped build falls back to the root", api.UnknownCommit, repo},
		{"an empty commit falls back to the root", "", repo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := &api.HandlerDeps{SourceURLBase: repo, Commit: c.commit}
			if got := deps.SourceURL(); got != c.want {
				t.Errorf("SourceURL() is %q, want %q", got, c.want)
			}
		})
	}
	// With no repository configured there is no honest answer, so the field
	// is empty rather than invented.
	empty := &api.HandlerDeps{Commit: "abc123"}
	if got := empty.SourceURL(); got != "" {
		t.Errorf("with no repository SourceURL() is %q, want empty", got)
	}
}
