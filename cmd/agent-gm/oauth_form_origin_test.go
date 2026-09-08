package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The bug these tests were written for: a person opened
// `GET /oauth/authorize` in a browser, filled the enrollment form in, and the
// submission came back 403 `this form may only be submitted from ...`. The
// page was served with `Referrer-Policy: no-referrer`; a browser applies the
// page's policy to the form navigation, and the Fetch standard serialises
// `Origin` as `null` when that policy is `no-referrer`. Every earlier
// exercise of this form set `Origin` by hand and so never consulted the page.
// Nothing in the suite had ever posted it as a browser would.
//
// Two tests, because the fix has two halves that fail differently: the pages
// say `same-origin` (so the browser sends its real origin), and the refusal
// says which origin it got (so the next person does not have to guess).

// TestOAuthPagesCarrySameOriginReferrerPolicy pins the split of section 9.9:
// the HTML a browser renders, and the script that HTML loads, carry
// `Referrer-Policy: same-origin`; everything else -- the JSON answers, the
// discovery documents, and above all the redirect that carries the
// authorization code -- keeps `no-referrer`.
//
// Plant: put `no-referrer` back in setPageSecurityHeaders and this fails at
// "the authorization screen carries Referrer-Policy".
func TestOAuthPagesCarrySameOriginReferrerPolicy(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	id, _ := h.pendingRequest(admin, "http://127.0.0.1:53213/callback")

	clientID := h.register("http://127.0.0.1:53214/callback")
	_, challenge := pkce(t)

	pages := map[string]*http.Response{
		"the authorization screen": h.get(authorizeParams{
			ClientID: clientID, RedirectURI: "http://127.0.0.1:53214/callback",
			State: "s", Challenge: challenge}.query(h)),
		"the waiting page":   h.get("/oauth/requests/"+id, h.withCookie),
		"the polling script": h.get("/oauth/poll.js"),
	}
	others := map[string]*http.Response{
		"the metadata document": h.get("/.well-known/oauth-authorization-server"),
		"an unknown path":       h.get("/oauth/nonsense"),
		"an OAuth error body": h.postForm("/oauth/authorize",
			url.Values{"context": {"nonsense"}}),
	}

	// The other four headers of section 9.9 are unchanged everywhere: this
	// fix moves one value and must not quietly drop the rest.
	rest := map[string]string{
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; " +
			"base-uri 'none'; form-action 'self'; script-src 'self'; connect-src 'self'; style-src 'self'",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	}
	check := func(name string, resp *http.Response, want string) {
		t.Helper()
		h.captureCookie(resp)
		_ = readBody(t, resp)
		if got := resp.Header.Get("Referrer-Policy"); got != want {
			t.Errorf("%s carries Referrer-Policy: %q, want %q", name, got, want)
		}
		for header, value := range rest {
			if header == "Content-Security-Policy" {
				if name == "the authorization screen" {
					value = strings.Replace(value, "form-action 'self'", "form-action 'self' http://127.0.0.1:53214", 1)
				}
				if name == "the waiting page" {
					value = strings.Replace(value, "form-action 'self'", "form-action 'self' http://127.0.0.1:53213", 1)
				}
			}
			if got := resp.Header.Get(header); got != value {
				t.Errorf("%s carries %s: %q, want %q", name, header, got, value)
			}
		}
	}
	for name, resp := range pages {
		check(name, resp, "same-origin")
	}
	for name, resp := range others {
		check(name, resp, "no-referrer")
	}
}

// TestOriginRefusalNamesTheOriginItReceived is the diagnosis half. The check
// itself stays exact -- `null` is refused, not exempted -- but a refusal that
// named no value could not be told apart from an attack, which is how a
// header bug survived to production.
//
// Plant: drop the `; received Origin ...` clause from checkOrigin and this
// fails at "the 403 says".
func TestOriginRefusalNamesTheOriginItReceived(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	_, codeValue := h.enrollmentCode(admin, nil)
	const redirect = "http://127.0.0.1:53215/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(t)

	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
	})
	form := hiddenFields(t, page)
	form.Add("scope_selected", "messages:read")
	form.Set("enrollment_code", codeValue)

	origin := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Origin", v) }
	}

	for _, sent := range []string{"null", "https://evil.example"} {
		refused := h.postForm("/oauth/authorize", form, h.withCookie, origin(sent))
		body := readBody(t, refused)
		if refused.StatusCode != http.StatusForbidden {
			t.Fatalf("posting the form with Origin %q answered %d, want 403",
				sent, refused.StatusCode)
		}
		var oe map[string]string
		if err := json.Unmarshal([]byte(body), &oe); err != nil {
			t.Fatalf("the 403 body is %q, want the OAuth error shape", body)
		}
		if oe["error"] != "invalid_request" {
			t.Errorf("the 403 error is %q, want invalid_request", oe["error"])
		}
		want := `received Origin "` + sent + `"`
		if !strings.Contains(oe["error_description"], want) {
			t.Errorf("the 403 says %q; it must contain %q, or the owner cannot tell "+
				"a header bug from an attack", oe["error_description"], want)
		}
		if !strings.Contains(oe["error_description"], h.issuer) {
			t.Errorf("the 403 says %q; it must still name the origin it wants",
				oe["error_description"])
		}
		if refused.Header.Get("X-Request-Id") == "" {
			t.Error("the 403 carries no X-Request-Id, so it cannot be matched to " +
				"the warn line the server logged")
		}
	}

	// `null` is refused, never accepted: the point of the fix is the page,
	// not a hole in the check. And the matching origin still passes, so the
	// diagnosis did not cost the flow.
	ok := h.postForm("/oauth/authorize", form, h.withCookie, origin(h.issuer))
	_ = readBody(t, ok)
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("posting the form with the matching Origin answered %d, want 303",
			ok.StatusCode)
	}
}

// TestCompletionOriginRefusalNamesWhatItReceived is the same requirement on
// `POST /oauth/requests/{id}/complete`, which is stricter: the header must be
// present. An absent one is reported as absent rather than as an empty value.
func TestCompletionOriginRefusalNamesWhatItReceived(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	id, form := h.pendingRequest(admin, "http://127.0.0.1:53216/callback")
	body := url.Values{"form_token": {form.Get("form_token")}}

	refused := h.postForm("/oauth/requests/"+id+"/complete", body, h.withCookie,
		func(r *http.Request) { r.Header.Set("Origin", "null") })
	text := readBody(t, refused)
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("completing with Origin null answered %d, want 403", refused.StatusCode)
	}
	if !strings.Contains(text, `received Origin \"null\"`) &&
		!strings.Contains(text, `received Origin "null"`) {
		t.Errorf("the 403 says %q; it must name the Origin it received", text)
	}

	absent := h.postForm("/oauth/requests/"+id+"/complete", body, h.withCookie)
	text = readBody(t, absent)
	if absent.StatusCode != http.StatusForbidden {
		t.Fatalf("completing with no Origin answered %d, want 403", absent.StatusCode)
	}
	if !strings.Contains(text, "carries no Origin") {
		t.Errorf("the 403 says %q; it must say the header was absent", text)
	}
}
