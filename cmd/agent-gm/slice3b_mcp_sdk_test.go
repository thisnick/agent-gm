package main

import (
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// The MCP-over-the-wired-binary properties that only exist because the
// protocol layer is the official Go SDK (decision D36).

// TestSlice3bRevocationEndsALiveMCPSession is section 9.6's "revocation is
// effective on the next request", asserted against a session that is already
// open.
//
// It is new work because the risk is new. Before decision D36 there was no
// MCP session at all: every POST was independent, so "the next request"
// could not mean anything else. The SDK's transport holds a session across
// requests, and the obvious way to write the authorization down -- once, when
// the session is established -- would make a revoked token keep working until
// the client happened to reconnect. It is read from
// `RequestExtra.TokenInfo` on every single call instead, and that is what
// this test is for: the same open client session, the same open transport,
// and the call after the revocation is refused.
func TestSlice3bRevocationEndsALiveMCPSession(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)

	client := sdk.NewClient(&sdk.Implementation{Name: "agent-gm-tests", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:   h.http.URL + mcp.Path,
		HTTPClient: &http.Client{Transport: bearerTransport{token: access, next: http.DefaultTransport}},
	}
	cs, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("the SDK client could not connect with a freshly minted token: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// The session works before the revocation, so that a failure afterwards
	// is the revocation and not a broken setup.
	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("tools/list on a live authorization failed: %v", err)
	}

	whoami := dataOf(t, h.get("/v1/auth/whoami", bearer(access)))
	id, _ := whoami["authorization_id"].(string)
	if id == "" {
		t.Fatal("/v1/auth/whoami reports no authorization_id")
	}
	revoked := h.do(mustRequest(t, http.MethodDelete,
		h.http.URL+"/v1/admin/authorizations/"+id+"?reason=test", bearer(admin)))
	if data := dataOf(t, revoked); data["revoked"] != true {
		t.Fatalf("the revocation reports %v", data)
	}

	// The very next call on the SAME session. Not a new client, not a
	// reconnect: the one the connector already has open.
	if _, err := cs.ListTools(t.Context(), nil); err == nil {
		t.Fatal("a revoked authorization kept an open MCP session working; section 9.6 " +
			"makes revocation effective on the NEXT REQUEST, and the request a " +
			"connector makes next is on the session it already holds")
	}
}

// TestSlice3bTheProtectedResourceDocumentIsTheSDKsHandler asserts what
// decision D36 says about RFC 9728: the document is served by
// `auth.ProtectedResourceMetadataHandler`, so it carries the CORS headers
// RFC 9728 section 3.1 asks for -- a browser-based client has to be able to
// read public discovery data cross-origin -- while section 9.9's headers
// survive underneath and section 9.2's values stay byte-exact.
func TestSlice3bTheProtectedResourceDocumentIsTheSDKsHandler(t *testing.T) {
	h := newOAuthHarness(t)

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		resp := h.get(path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin is %q, want * -- this document is "+
				"public discovery data and a browser client must be able to read it", path, got)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
			"X-Frame-Options":        "DENY",
			"Cache-Control":          "no-store",
		} {
			if got := resp.Header.Get(header); got != want {
				t.Errorf("%s: %s is %q, want %q -- section 9.9's headers must survive "+
					"the SDK's handler", path, header, got, want)
			}
		}

		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			ScopesSupported      []string `json:"scopes_supported"`
		}
		if err := json.Unmarshal([]byte(readBody(t, resp)), &doc); err != nil {
			t.Fatalf("%s: decoding the document: %v", path, err)
		}
		// Section 9.2's values are ours and stay byte-exact whoever serves them.
		if doc.Resource != oauthTestIssuer+mcp.Path {
			t.Errorf("%s: resource is %q, want %q", path, doc.Resource, oauthTestIssuer+mcp.Path)
		}
		if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != oauthTestIssuer {
			t.Errorf("%s: authorization_servers is %v, want [%q]",
				path, doc.AuthorizationServers, oauthTestIssuer)
		}
		if len(doc.ScopesSupported) != 3 {
			t.Errorf("%s: scopes_supported is %v, want the three messaging scopes",
				path, doc.ScopesSupported)
		}
	}

	// An OPTIONS preflight is answered, which is the other half of being
	// readable from a browser.
	preflight := h.do(mustRequest(t, http.MethodOptions,
		h.http.URL+"/.well-known/oauth-protected-resource/mcp"))
	if preflight.StatusCode != http.StatusNoContent && preflight.StatusCode != http.StatusOK {
		t.Errorf("a CORS preflight answered %d, want 204", preflight.StatusCode)
	}
	_ = readBody(t, preflight)
}
