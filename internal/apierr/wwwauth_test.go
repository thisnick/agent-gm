package apierr

import (
	"strings"
	"testing"
)

const testPublicURL = "https://gm.agent-wx.app"

// TestWWWAuthenticateFor401 pins the header spec section 7.2 requires on a
// 401: realm, an error parameter, and resource_metadata pointing at the
// protected-resource document an MCP client fetches to discover where to
// authorize. A 401 carries no scope, because the server has not established
// who the caller is and therefore does not know what they would have needed.
func TestWWWAuthenticateFor401(t *testing.T) {
	got := WWWAuthenticate(testPublicURL, CodeInvalidToken, "")
	const want = `Bearer realm="agent-gm", error="invalid_token", ` +
		`resource_metadata="https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp"`
	if got != want {
		t.Errorf("WWW-Authenticate\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "scope=") {
		t.Error("a 401 must not claim to know which scope was needed")
	}
}

// TestWWWAuthenticateFor403 pins the scope-refusal header: the same three
// parameters plus the scope the route requires, so the caller can ask for it.
func TestWWWAuthenticateFor403(t *testing.T) {
	got := WWWAuthenticate(testPublicURL, CodeInsufficientScope, "messages:write")
	const want = `Bearer realm="agent-gm", error="insufficient_scope", scope="messages:write", ` +
		`resource_metadata="https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp"`
	if got != want {
		t.Errorf("WWW-Authenticate\n got: %s\nwant: %s", got, want)
	}
}

// TestResourceMetadataURLIsBuiltFromThePublicURL proves the metadata URL
// comes from the configured public URL and tolerates a trailing slash. It is
// never built from a request Host: a hostile Host or X-Forwarded-Host would
// otherwise point a client at an attacker's authorization server (spec
// sections 7.2, 9.2, Slice 2 test 19).
func TestResourceMetadataURLIsBuiltFromThePublicURL(t *testing.T) {
	const want = "https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp"
	for _, in := range []string{
		"https://gm.agent-wx.app",
		"https://gm.agent-wx.app/",
		"https://gm.agent-wx.app///",
	} {
		if got := ResourceMetadataURL(in); got != want {
			t.Errorf("ResourceMetadataURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ResourceMetadataURL("https://other.example"); !strings.HasPrefix(got, "https://other.example/") {
		t.Errorf("the metadata URL ignored the configured public URL: %q", got)
	}
}

// TestWWWAuthenticateForOnlyChallengesOn401And403 proves a handler can call
// the builder unconditionally: an unrelated error never advertises an
// authorization challenge, which would tell a caller to go and re-authorize
// over a rate limit or a Google outage.
func TestWWWAuthenticateForOnlyChallengesOn401And403(t *testing.T) {
	if got := WWWAuthenticateFor(testPublicURL, InvalidToken("expired")); got == "" {
		t.Error("invalid_token must carry a challenge")
	}
	scoped := WWWAuthenticateFor(testPublicURL, InsufficientScope("messages:read"))
	if !strings.Contains(scoped, `scope="messages:read"`) {
		t.Errorf("insufficient_scope must carry the required scope: %s", scoped)
	}

	for _, e := range []*Error{
		NotFound("conversation"), RateLimited(0), Disconnected(), NotPaired(),
		PhoneNotResponding(), UnknownQueryParameter("_"),
	} {
		if got := WWWAuthenticateFor(testPublicURL, e); got != "" {
			t.Errorf("%q advertised a challenge: %s", e.Code, got)
		}
	}
	if got := WWWAuthenticateFor(testPublicURL, nil); got != "" {
		t.Errorf("a nil error produced %q", got)
	}
}

// TestWWWAuthenticateQuotesItsValues proves a value carrying a quote cannot
// break out of the quoted-string and inject a parameter of its own.
func TestWWWAuthenticateQuotesItsValues(t *testing.T) {
	got := WWWAuthenticate(testPublicURL, CodeInsufficientScope, `a", error="invalid_token`)
	// Only one unescaped `error="` parameter may exist; the injected one must
	// survive as escaped text inside the scope's quoted-string.
	if n := strings.Count(got, `, error="`); n != 1 {
		t.Errorf("a quote in the scope injected %d error parameters: %s", n, got)
	}
	if !strings.Contains(got, `error=\"invalid_token`) {
		t.Errorf("the injected quote was not escaped into the scope value: %s", got)
	}
}
