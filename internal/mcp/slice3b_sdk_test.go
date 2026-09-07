package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// The properties that exist because the protocol layer is the official MCP Go
// SDK (decision D36), and that would go unnoticed if nothing named them.
//
// Each of these is a place where taking the SDK changed something, or where
// taking the SDK would have changed something and we declined. A test that
// only asserted the old behaviour would pass against a layer that had quietly
// stopped being the SDK's; these assert the seam itself.

// TestTheChallengeIsTheSDKsFormat pins section 9.2's `WWW-Authenticate` to
// what the SDK actually emits, by running a real `auth.RequireBearerToken`
// and reading the header off it.
//
// The SDK is the authority for the challenge FORMAT from decision D36, and
// this is what that sentence means operationally. Our own `Challenge()`
// builds the string, because the options that would make the SDK emit
// section 9.2's `scope` parameter -- `RequireBearerTokenOptions.Scopes` --
// are also enforced as an AND over the whole list, and section 8.1's rule is
// an OR over three scopes, so setting them would refuse a legitimate
// read-only token. Building it ourselves is therefore forced; agreeing with
// the SDK byte for byte is not, and this is what keeps it true. An SDK bump
// that changes the format fails here rather than at a connector.
func TestTheChallengeIsTheSDKsFormat(t *testing.T) {
	refused := sdkauth.RequireBearerToken(
		func(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
			return nil, fmt.Errorf("no: %w", sdkauth.ErrInvalidToken)
		},
		&sdkauth.RequireBearerTokenOptions{
			ResourceMetadataURL: publicURL + "/.well-known/oauth-protected-resource/mcp",
			Scopes:              mcp.ChallengeScopes,
		},
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the SDK's middleware admitted a request it should have refused")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, mcp.Path, nil)
	req.Header.Set("Authorization", "Bearer nonsense")
	refused.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the SDK's middleware answered %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	if got != mcp.Challenge(publicURL) {
		t.Fatalf("this server's challenge has drifted from the SDK's format.\n"+
			"  the SDK writes: %q\n  this server writes: %q", got, mcp.Challenge(publicURL))
	}
	if strings.Contains(got, "realm=") {
		t.Error("the SDK emits no realm parameter, so section 9.2 must not promise one")
	}
}

// TestTwoAuthorizationHeadersAreRefusedBeforeTheSDKSeesThem is section 8.1's
// "exactly one Authorization header is parsed", and it is a test about the
// SDK rather than about us.
//
// `auth.verify` reads `req.Header.Get("Authorization")`, which returns the
// FIRST value and ignores the rest -- so with the SDK's middleware alone, a
// request carrying a valid token and a second header is served from the valid
// one. Section 8.1 refuses that rather than resolving it, because resolving
// means choosing which of two credentials a caller meant and a proxy that
// appended its own header would silently decide it. The check that makes this
// pass is ours, in front of the SDK's.
func TestTwoAuthorizationHeadersAreRefusedBeforeTheSDKSeesThem(t *testing.T) {
	h := newHarness(t)

	for _, second := range []string{
		"Bearer somebody-elses-token",
		// The second being valid too is the case a "first one wins" server
		// gets right by luck; it must still be refused.
		"Bearer " + h.Token,
	} {
		req, err := http.NewRequest(http.MethodPost, h.HTTP.URL+mcp.Path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Add("Authorization", "Bearer "+h.Token)
		req.Header.Add("Authorization", second)

		resp, err := h.HTTP.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a valid Authorization header followed by %q answered %d, want 401 -- "+
				"the first must not win\n%s", second, resp.StatusCode, body)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != mcp.Challenge(publicURL) {
			t.Errorf("the refusal carries challenge %q, want %q", got, mcp.Challenge(publicURL))
		}
	}
}

// TestTheSDKsOwnRefusalCarriesSection92sChallenge covers the path where the
// challenge really is written by the SDK's middleware rather than by this
// package: a well-formed `Bearer <token>` whose token is not valid.
//
// It matters because the SDK writes a challenge of its own there, and it is
// the wrong one -- no `scope` parameter, for the reason `Challenge` gives.
// Driven with the SDK's own OAuth client, a challenge with no `scope` makes
// the client fall back to the metadata document's `scopes_supported` and ask
// the owner to approve `messages:delete` as well, which is exactly what
// ChallengeScopes exists to prevent. The header is substituted, not added,
// so there is one.
func TestTheSDKsOwnRefusalCarriesSection92sChallenge(t *testing.T) {
	h := newHarness(t)

	answer := h.rawCall("a-well-formed-token-that-is-not-valid", "ping", nil)
	if answer.Status != http.StatusUnauthorized {
		t.Fatalf("an invalid token answered %d, want 401", answer.Status)
	}
	got := answer.Headers.Values("WWW-Authenticate")
	if len(got) != 1 {
		t.Fatalf("the refusal carries %d WWW-Authenticate headers, want exactly one: %v",
			len(got), got)
	}
	if got[0] != mcp.Challenge(publicURL) {
		t.Fatalf("the challenge is %q, want %q", got[0], mcp.Challenge(publicURL))
	}
	if !strings.Contains(got[0], `scope="`) {
		t.Error("the challenge names no scope, so a client falls back to the metadata " +
			"document's scopes_supported and asks the owner for messages:delete too")
	}
}

// TestAJSONRPCErrorDoesNotEndTheSession is the property the SEP-2575 HTTP
// status mapping would have cost us.
//
// From protocol 2026-07-28 the SDK gives some JSON-RPC errors an HTTP status
// of their own, and its own client treats any non-2xx as a connection
// failure. So a model that names a tool that does not exist -- an everyday
// thing for a model to do -- would disconnect the connector, and the next
// call would fail with `client is closing` rather than with an answer. This
// server answers a JSON-RPC error with HTTP 200 for exactly that reason, and
// this test is the reason written down: it makes the failing call FIRST and
// then requires the session to keep working.
func TestAJSONRPCErrorDoesNotEndTheSession(t *testing.T) {
	h := newHarness(t)
	h.addAccount(addressA)

	for _, failing := range []struct {
		name   string
		method string
		params any
	}{
		{"an unknown tool name", "tools/call", map[string]any{
			"name": "delete_everything", "arguments": map[string]any{},
		}},
		{"a resource that does not exist", "resources/read", map[string]any{
			"uri": mcp.AttachmentURIPrefix + "att_nope",
		}},
		{"a URI this server does not serve at all", "resources/read", map[string]any{
			"uri": "file:///etc/passwd",
		}},
	} {
		t.Run(failing.name, func(t *testing.T) {
			answer := h.callWith(h.Token, failing.method, failing.params, nil)
			if answer.Error == nil {
				t.Fatalf("%s answered a result rather than a JSON-RPC error: %s",
					failing.name, answer.Raw)
			}
			if answer.Status != http.StatusOK {
				t.Fatalf("%s answered HTTP %d; the reference client treats a non-2xx as a "+
					"connection failure and tears the session down", failing.name, answer.Status)
			}
			// The same session, immediately afterwards. This is the
			// assertion: an error the model can correct itself from must not
			// have cost it the connection.
			if names := h.toolNames(h.Token); len(names) == 0 {
				t.Fatalf("the session did not survive %s", failing.name)
			}
		})
	}
}

// TestTest25ConcurrencyBudget is section 8.1's "8 in flight per
// authorization, 32 across the process; excess is 429 with Retry-After".
//
// The reviewer's plant P25 -- multiplying both limits by a thousand --
// survived the whole Slice 3 suite, because the budget was implemented and
// nothing named it. It is asserted here by really holding requests in flight,
// not by timing: the fake backend parks every send until the test releases
// it, so "eight are in flight" is a fact rather than a hope.
func TestTest25ConcurrencyBudget(t *testing.T) {
	h := newHarness(t)
	accountID := h.addAccount(addressA)
	conv := h.seedConversation(accountID, "conv-budget")
	blocker := h.backend(accountID).BlockOn("SendText")
	// Released unconditionally, so that a FAILING assertion below unparks the
	// held calls instead of leaving eight goroutines in the backend and the
	// test binary hanging at exit. A budget test that hangs when the budget
	// is broken reports "timeout", which says nothing about the budget.
	t.Cleanup(blocker.Release)

	// The loop count is a LITERAL, not the constant under test.
	//
	// Deriving it from `mcp.MaxInFlightPerAuthorization` is how the reviewer's
	// plant -- multiplying both limits by a thousand -- cost ten minutes and
	// reported as a package timeout instead of as an assertion: the test
	// obligingly fired eight thousand goroutines and waited for them. A test
	// that scales with the thing it is checking cannot fail quickly, so the
	// number is written down and the constant is checked against it.
	const inFlight = 8
	if mcp.MaxInFlightPerAuthorization != inFlight {
		t.Fatalf("section 8.1's per-authorization budget is %d, and this test is written "+
			"for %d; change both deliberately", mcp.MaxInFlightPerAuthorization, inFlight)
	}

	var wg sync.WaitGroup
	for i := range inFlight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.rawCall(h.Token, "tools/call", map[string]any{
				"name": "send_message",
				"arguments": map[string]any{
					"conversation_id":   conv.ID,
					"text":              "held",
					"client_request_id": fmt.Sprintf("budget-%d", i),
				},
			})
		}()
	}
	// Every one of the eight is now inside the backend, which means every one
	// of them holds a slot.
	if err := blocker.WaitFor(inFlight); err != nil {
		t.Fatalf("the calls that were supposed to fill the budget never got there: %v", err)
	}

	ninth := h.rawCall(h.Token, "ping", nil)
	if ninth.Status != http.StatusTooManyRequests {
		t.Fatalf("with %d calls in flight the next one answered %d, want 429",
			inFlight, ninth.Status)
	}
	if retry := ninth.Headers.Get("Retry-After"); retry == "" {
		t.Error("the 429 carries no Retry-After; a client with no delay to obey guesses, " +
			"and a guessing client retries too soon")
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ninth.Raw, &envelope); err != nil {
		t.Fatalf("the 429 is not section 7.1's envelope: %v (%s)", err, ninth.Raw)
	}
	if envelope.Error.Code != "rate_limited" {
		t.Errorf("the 429's code is %q, want rate_limited", envelope.Error.Code)
	}

	// A slot frees when a request settles.
	blocker.Release()
	wg.Wait()
	if after := h.rawCall(h.Token, "ping", nil); after.Status != http.StatusOK {
		t.Errorf("after the held calls settled, a new call answered %d, want 200 -- "+
			"the budget must be released, not spent", after.Status)
	}
}

// TestSameOriginComparesTheOriginAndNotTheURL is review finding B-7.
//
// An `Origin` is a scheme, a host and a port, and never a path (RFC 6454).
// `AGENT_GM_PUBLIC_URL` is allowed to carry one. The check used to compare the
// two trimmed strings, so a deployment under a path answered `403` to every
// browser client on its own correct origin -- the one case the check exists to
// let through -- while `https://gm.example.test`, which has no path, was
// unaffected. That is why it survived a slice: the deployment we have is the
// one shape that hides it.
func TestSameOriginComparesTheOriginAndNotTheURL(t *testing.T) {
	for _, c := range []struct {
		name      string
		publicURL string
		origin    string
		want      int
	}{
		{"the exact origin", "https://gm.example.test", "https://gm.example.test", http.StatusOK},
		{"a trailing slash on the origin", "https://gm.example.test", "https://gm.example.test/", http.StatusOK},
		{"a public URL under a path", "https://gm.example.test/gm", "https://gm.example.test", http.StatusOK},
		{"a public URL with a trailing slash", "https://gm.example.test/", "https://gm.example.test", http.StatusOK},
		{"another host", "https://gm.example.test", "https://evil.example", http.StatusForbidden},
		{"another scheme", "https://gm.example.test", "http://gm.example.test", http.StatusForbidden},
		{"another port", "https://gm.example.test", "https://gm.example.test:8443", http.StatusForbidden},
		// A browser sends `Origin: null` from a sandboxed or a
		// cross-origin-redirected context. It is the header saying "do not
		// trust this", so it must not match.
		{"null", "https://gm.example.test", "null", http.StatusForbidden},
		{"a bare host with no scheme", "https://gm.example.test", "gm.example.test", http.StatusForbidden},
		// A host that merely CONTAINS ours. String comparison got this right
		// by accident; parsing must get it right on purpose.
		{"a look-alike host", "https://gm.example.test", "https://gm.example.test.evil.example", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessAt(t, c.publicURL)
			answer := h.rawCallWith(h.Token, "ping", nil, map[string]string{"Origin": c.origin})
			if answer.Status != c.want {
				t.Fatalf("public URL %q with Origin %q answered %d, want %d",
					c.publicURL, c.origin, answer.Status, c.want)
			}
		})
	}
}

// A panic while a refusal is buffered still produces the refusal (review
// finding C-3).
//
// Everything at 400 and above is captured by envelopeWriter and reaches the
// socket only from finish(). If finish() is not deferred, a panic anywhere
// below it writes nothing at all to the real ResponseWriter, and net/http
// closes the connection -- turning a refusal a client could read into a
// transport error it cannot, which is the same failure the whole
// demote-to-200 rule exists to avoid one layer up.
func TestARefusalSurvivesAPanicWhileItIsBuffered(t *testing.T) {
	h := newHarness(t)

	h.MCP.FaultAfterChainForTest(func() { panic("a panic after the refusal was buffered") })

	// A well-formed bearer whose token is invalid: the SDK refuses it with
	// `http.Error`, which is text/plain, which is the case envelopeWriter
	// CAPTURES. An Origin refusal would not do -- this package writes that
	// one as JSON itself, so it goes straight through and never sits in the
	// buffer this test is about.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, mcp.Path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer a-well-formed-token-that-is-not-valid")

	func() {
		defer func() {
			// The panic is expected; the response is the point.
			_ = recover()
		}()
		h.MCP.ServeHTTP(rec, req)
	}()

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after a panic while the refusal was buffered the client got %d, want 401 -- "+
			"an unwritten buffer is a closed connection", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != mcp.Challenge(publicURL) {
		t.Errorf("the delivered refusal carries challenge %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("the refusal is %q, not section 7.1's envelope", ct)
	}
}
