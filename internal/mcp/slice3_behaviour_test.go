package mcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	sdkjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/mcp"
	"github.com/thisnick/agent-gm/internal/store"
)

// TestSlice3Test17MultiAccount is acceptance test 17.
//
// With two accounts: `list_accounts` returns both with their states; a read
// tool omitting `account_id` covers both and every row carries its
// `account_id`; a **write** tool omitting it returns `isError: true` with
// `details.accounts` naming both, and retrying with one of those IDs
// succeeds; `get_session` works with and without `account_id` under the
// section 7.3 rule; `get_health` returns two account rows and an
// `accounts_summary`. With **one** account, every one of those calls succeeds
// with `account_id` omitted.
//
// ONE FAKE IS ONE ACCOUNT: two accounts is two fakes over one store, which is
// the shape production has.
func TestSlice3Test17MultiAccount(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount(addressA)
	b := h.addAccount(addressB)
	h.seedConversation(a, "thread-a")
	h.seedConversation(b, "thread-b")
	h.seedMessage(a, "thread-a", "m-a-1")
	h.seedMessage(b, "thread-b", "m-b-1")

	// list_accounts returns both, with their states.
	accountsResult := h.tool("list_accounts", nil)
	rows := itemsOf(t, accountsResult)
	if len(rows) != 2 {
		t.Fatalf("list_accounts returned %d accounts, want 2", len(rows))
	}
	seen := map[string]string{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		state, _ := row["state"].(string)
		if state == "" {
			t.Errorf("account %s carries no state", id)
		}
		seen[id] = state
	}
	if _, ok := seen[a]; !ok {
		t.Errorf("list_accounts omits %s", a)
	}
	if _, ok := seen[b]; !ok {
		t.Errorf("list_accounts omits %s", b)
	}

	// A read tool omitting account_id covers both, and every row carries its
	// own account_id -- which is what makes an answer across accounts usable
	// rather than merely large.
	messages := h.tool("list_messages", nil)
	byAccount := map[string]int{}
	for _, row := range itemsOf(t, messages) {
		id, ok := row["account_id"].(string)
		if !ok || id == "" {
			t.Fatalf("a message row carries no account_id: %v", row)
		}
		byAccount[id]++
	}
	if byAccount[a] == 0 || byAccount[b] == 0 {
		t.Fatalf("list_messages with no account_id covered %v, want rows from both %s and %s", byAccount, a, b)
	}

	// A WRITE omitting account_id is refused, and the refusal names the
	// candidates so the caller can retry without a second round trip.
	refused := h.tool("start_conversation", map[string]any{
		"recipients":        []any{fictionalA},
		"client_request_id": "cri-multi-1",
	})
	if !isError(refused) {
		t.Fatal("start_conversation with two accounts and no account_id was accepted")
	}
	e := resultError(t, refused)
	if e["code"] != "invalid_request" {
		t.Fatalf("the refusal is %q, want invalid_request", e["code"])
	}
	details, _ := e["details"].(map[string]any)
	if details["field"] != "account_id" {
		t.Fatalf("the refusal does not name account_id: %v", details)
	}
	candidates, _ := details["accounts"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("details.accounts names %d accounts, want both: %v", len(candidates), details)
	}
	named := map[string]bool{}
	for _, c := range candidates {
		obj, _ := c.(map[string]any)
		id, _ := obj["id"].(string)
		named[id] = true
		if obj["google_account"] == nil || obj["state"] == nil {
			t.Errorf("a candidate is missing google_account or state: %v", obj)
		}
	}
	if !named[a] || !named[b] {
		t.Fatalf("details.accounts names %v, want %s and %s", named, a, b)
	}

	// Retrying with one of those IDs succeeds.
	retried := h.tool("start_conversation", map[string]any{
		"account_id":        a,
		"recipients":        []any{fictionalA},
		"client_request_id": "cri-multi-2",
	})
	if isError(retried) {
		t.Fatalf("retrying with account_id %s was refused: %v", a, resultError(t, retried))
	}

	// get_session works WITH account_id...
	withID := h.tool("get_session", map[string]any{"account_id": b})
	if isError(withID) {
		t.Fatalf("get_session with account_id was refused: %v", resultError(t, withID))
	}
	if dataOf(t, withID)["id"] != b {
		t.Fatalf("get_session answered a different account: %v", dataOf(t, withID))
	}
	// ...and WITHOUT it is invalid_request listing the candidates, because
	// with more than one account there is nothing to default to.
	withoutID := h.tool("get_session", nil)
	if !isError(withoutID) {
		t.Fatal("get_session with two accounts and no account_id was accepted")
	}
	if got := resultError(t, withoutID)["code"]; got != "invalid_request" {
		t.Fatalf("get_session's refusal is %q, want invalid_request", got)
	}

	// get_health returns two account rows and an accounts_summary.
	health := h.tool("get_health", nil)
	data := dataOf(t, health)
	healthRows, _ := data["accounts"].([]any)
	if len(healthRows) != 2 {
		t.Fatalf("get_health returned %d account rows, want 2", len(healthRows))
	}
	summary, ok := data["accounts_summary"].(map[string]any)
	if !ok {
		t.Fatalf("get_health returned no accounts_summary: %v", data)
	}
	if total, _ := summary["total"].(float64); int(total) != 2 {
		t.Fatalf("accounts_summary.total is %v, want 2", summary["total"])
	}
}

// TestSlice3Test17SingleAccount is the other half of test 17: **with one
// account, every one of those calls succeeds with `account_id` omitted.** A
// single-account deployment never has to think about accounts.
func TestSlice3Test17SingleAccount(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount(addressA)
	h.seedConversation(a, "thread-a")
	h.seedMessage(a, "thread-a", "m-a-1")

	for _, name := range []string{"list_accounts", "list_conversations", "list_messages", "get_session", "get_health"} {
		result := h.tool(name, nil)
		if isError(result) {
			t.Errorf("%s with one account and no account_id was refused: %v", name, resultError(t, result))
		}
	}
	session := h.tool("get_session", nil)
	if dataOf(t, session)["id"] != a {
		t.Fatalf("get_session with no account_id answered %v, want %s", dataOf(t, session)["id"], a)
	}
	started := h.tool("start_conversation", map[string]any{
		"recipients":        []any{fictionalA},
		"client_request_id": "cri-single-1",
	})
	if isError(started) {
		t.Fatalf("a write with one account and no account_id was refused: %v", resultError(t, started))
	}
}

// TestSlice3Test19IsErrorSemantics is acceptance test 19.
//
// A domain failure is a **result** with `isError: true` and
// `structuredContent.error`, not a JSON-RPC error; an unknown tool name **is**
// a JSON-RPC error. Both directions asserted.
func TestSlice3Test19IsErrorSemantics(t *testing.T) {
	h := newHarness(t)
	h.addAccount(addressA)

	// Direction one: a domain failure is a RESULT.
	answer := h.callWith(h.Token, "tools/call", map[string]any{
		"name":      "get_message",
		"arguments": map[string]any{"message_id": "msg_00000000000000000000000000"},
	}, nil)
	if answer.Error != nil {
		t.Fatalf("not_found came back as a JSON-RPC error (%d %s); many clients never hand one to the model",
			answer.Error.Code, answer.Error.Message)
	}
	if answer.Status != http.StatusOK {
		t.Fatalf("a domain failure answered HTTP %d, want 200", answer.Status)
	}
	if !isError(answer.Result) {
		t.Fatalf("a not_found result does not carry isError: %v", answer.Result)
	}
	e := resultError(t, answer.Result)
	if e["code"] != "not_found" {
		t.Fatalf("the result's error code is %q, want not_found", e["code"])
	}
	for _, key := range []string{"code", "message", "retryable", "details"} {
		if _, ok := e[key]; !ok {
			t.Errorf("structuredContent.error has no %q; it must be the whole REST error envelope", key)
		}
	}

	// Direction two: an unknown TOOL NAME is a JSON-RPC error.
	answer = h.callWith(h.Token, "tools/call", map[string]any{
		"name": "delete_everything", "arguments": map[string]any{},
	}, nil)
	if answer.Error == nil {
		t.Fatalf("an unknown tool name answered a result rather than a JSON-RPC error: %s", answer.Raw)
	}

	// ...and so is an unknown METHOD. This one is posted by hand: no
	// reference client will send a method its own protocol does not have,
	// so the assertion cannot go through the SDK client.
	answer = h.rawCallWith(h.Token, "tools/summon", nil, atProtocol(mcp.ProtocolVersion))
	if answer.Error == nil {
		t.Fatalf("an unknown method answered a result rather than a JSON-RPC error: %s", answer.Raw)
	}
	if answer.Error.Code != -32601 {
		t.Fatalf("an unknown method answered code %d, want -32601", answer.Error.Code)
	}

	// A malformed request is the third of the four, and it is the one the
	// SDK answers at the TRANSPORT rather than as a JSON-RPC error: a body
	// that is not a JSON-RPC message carries no `id`, so there is no request
	// to answer and nothing to put an error object's `id` field. It comes
	// back as `400` with section 7.1's envelope, which is a refusal a client
	// can read; a JSON-RPC error with a null id would not be attributable to
	// anything the client sent.
	malformed := h.post(h.Token, []byte("{not json"), atProtocol(mcp.ProtocolVersion))
	if malformed.Status != http.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400: %s", malformed.Status, malformed.Raw)
	}
	if malformed.Error != nil {
		t.Fatalf("a malformed body answered a JSON-RPC error, which has no id to carry: %s", malformed.Raw)
	}

	// And the boundary of the rule: `resources/read` is NOT a tools/call, so
	// its failures are JSON-RPC errors. A ReadResourceResult has no isError
	// field and must carry `contents`; a "result" reporting a failure is not
	// a valid result, and an official SDK client rejects it on schema before
	// the client's own code runs. The model learns nothing either way, so the
	// only thing at stake is whether the CLIENT gets a readable refusal or a
	// parse error.
	answer = h.callWith(h.Token, "resources/read",
		map[string]any{"uri": "agm://attachments/att_00000000-0000-0000-0000-000000000000"}, nil)
	if answer.Error == nil {
		t.Fatalf("a missing resource answered a result rather than a JSON-RPC error: %s",
			answer.Raw)
	}
	if int64(answer.Error.Code) != sdkjsonrpc.CodeInvalidParams {
		t.Errorf("a missing resource answered code %d, want the SDK's resource-not-found %d",
			answer.Error.Code, sdkjsonrpc.CodeInvalidParams)
	}
	// And it is a `200`, because a reference client treats a 4xx as a
	// connection failure -- see TestAJSONRPCErrorDoesNotEndTheSession.
	if answer.Status != http.StatusOK {
		t.Errorf("a JSON-RPC error was delivered with HTTP %d, want 200", answer.Status)
	}
}

// TestSlice3Test21Transport is acceptance test 21 and the rest of section
// 8.1's pre-parse checks.
//
// `Origin: https://evil.example` on `/mcp` is `403` **before parsing**; no
// `Origin` is accepted; two `Authorization` headers are refused rather than
// resolved.
func TestSlice3Test21Transport(t *testing.T) {
	h := newHarness(t)

	t.Run("a foreign Origin is 403 before anything is parsed", func(t *testing.T) {
		// The body is deliberately unparseable. A 403 proves the Origin check
		// ran FIRST: had the transport parsed first, this would have been a
		// -32700 instead.
		answer := h.post(h.Token, []byte("{not json at all"),
			map[string]string{"Origin": "https://evil.example"})
		if answer.Status != http.StatusForbidden {
			t.Fatalf("a foreign Origin answered %d, want 403", answer.Status)
		}
		if answer.Error != nil {
			t.Fatal("a foreign Origin was answered with a JSON-RPC error, which means the body was parsed first")
		}
	})

	t.Run("the configured public URL is accepted as an Origin", func(t *testing.T) {
		answer := h.callWith(h.Token, "ping", nil, map[string]string{"Origin": publicURL})
		if answer.Status != http.StatusOK {
			t.Fatalf("the server's own Origin answered %d, want 200", answer.Status)
		}
	})

	t.Run("no Origin is supported", func(t *testing.T) {
		answer := h.callWith(h.Token, "ping", nil, map[string]string{"Origin": ""})
		if answer.Status != http.StatusOK {
			t.Fatalf("a non-browser client sending no Origin answered %d, want 200", answer.Status)
		}
	})

	t.Run("two Authorization headers are refused rather than resolved", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, h.HTTP.URL+mcp.Path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		// The FIRST is valid. A server that resolved to whichever happens to
		// be first would answer 200 here, which is the failure this refuses.
		req.Header.Add("Authorization", "Bearer "+h.Token)
		req.Header.Add("Authorization", "Bearer somebody-elses-token")
		resp, err := h.HTTP.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("two Authorization headers answered %d, want 401 -- the first must not win", resp.StatusCode)
		}
	})

	t.Run("another scheme is refused", func(t *testing.T) {
		answer := h.callWith("", "ping", nil, map[string]string{"Authorization": "Basic " + h.Token})
		if answer.Status != http.StatusUnauthorized {
			t.Fatalf("a Basic credential answered %d, want 401", answer.Status)
		}
	})

	t.Run("a value carrying two tokens is refused", func(t *testing.T) {
		answer := h.callWith("", "ping", nil,
			map[string]string{"Authorization": "Bearer " + h.Token + " " + h.Token})
		if answer.Status != http.StatusUnauthorized {
			t.Fatalf("two tokens in one value answered %d, want 401", answer.Status)
		}
	})

	t.Run("no token is 401 with the challenge", func(t *testing.T) {
		answer := h.callWith("", "ping", nil, nil)
		if answer.Status != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated call answered %d, want 401", answer.Status)
		}
		challenge := answer.Headers.Get("WWW-Authenticate")
		if challenge != mcp.Challenge(publicURL) {
			t.Fatalf("the challenge is %q, want %q", challenge, mcp.Challenge(publicURL))
		}
		if !strings.Contains(challenge, "resource_metadata=") {
			t.Fatalf("the challenge carries no resource_metadata: %q", challenge)
		}
	})

	t.Run("a valid token with no messaging scope is 403 with the SAME challenge", func(t *testing.T) {
		adminOnly := h.narrowToken("admin")
		// Raw: a reference client cannot even connect with this token, which
		// is the point -- the refusal happens before any session exists.
		answer := h.rawCall(adminOnly, "ping", nil)
		if answer.Status != http.StatusForbidden {
			t.Fatalf("an admin-only token answered %d, want 403 -- deliberately distinct from the 401", answer.Status)
		}
		if got := answer.Headers.Get("WWW-Authenticate"); got != mcp.Challenge(publicURL) {
			t.Fatalf("the 403's challenge is %q, want the same as the 401's %q", got, mcp.Challenge(publicURL))
		}
	})

	t.Run("a body over 1 MiB is 413", func(t *testing.T) {
		big := make([]byte, mcp.MaxBodyBytes+1)
		for i := range big {
			big[i] = ' '
		}
		answer := h.post(h.Token, big, nil)
		if answer.Status != http.StatusRequestEntityTooLarge {
			t.Fatalf("a body over 1 MiB answered %d, want 413", answer.Status)
		}
	})

	// Content-Type and Accept are the SDK transport's checks now (decision
	// D36), and it answers 415 rather than 400 for the wrong media type --
	// which is the more correct status, and is what every other MCP server
	// built on this SDK answers.
	t.Run("Content-Type must be application/json", func(t *testing.T) {
		answer := h.callWith(h.Token, "ping", nil, map[string]string{"Content-Type": "text/plain"})
		if answer.Status != http.StatusUnsupportedMediaType {
			t.Fatalf("a text/plain body answered %d, want 415", answer.Status)
		}
	})

	// The MCP streamable transport requires BOTH media types in `Accept`,
	// not either: the server chooses which to answer with, so a client that
	// admitted only one would be refusing an answer the server is entitled
	// to give. Section 8.1 said "or" and the SDK says "and"; the SDK is the
	// protocol.
	t.Run("Accept must admit both application/json and text/event-stream", func(t *testing.T) {
		for _, accept := range []string{"application/xml", "application/json", "text/event-stream"} {
			answer := h.callWith(h.Token, "ping", nil, map[string]string{"Accept": accept})
			if answer.Status != http.StatusBadRequest {
				t.Fatalf("an Accept of %q answered %d, want 400", accept, answer.Status)
			}
		}
		for _, accept := range []string{"application/json, text/event-stream", "*/*"} {
			answer := h.callWith(h.Token, "ping", nil, map[string]string{"Accept": accept})
			if answer.Status != http.StatusOK {
				t.Fatalf("an Accept of %q answered %d, want 200", accept, answer.Status)
			}
		}
	})

	// GET and DELETE have nothing to do: the transport is stateless, so
	// there is no server-initiated stream to open and no session to delete.
	// Authentication comes first in section 8.1's order, so an
	// unauthenticated GET is a 401 and only an authenticated one reaches the
	// transport's 405.
	t.Run("GET is 405 once authenticated, and 401 before", func(t *testing.T) {
		anonymous, err := h.HTTP.Client().Get(h.HTTP.URL + mcp.Path)
		if err != nil {
			t.Fatalf("GET /mcp: %v", err)
		}
		defer func() { _ = anonymous.Body.Close() }()
		if anonymous.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated GET /mcp answered %d, want 401", anonymous.StatusCode)
		}

		req, err := http.NewRequest(http.MethodGet, h.HTTP.URL+mcp.Path, nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+h.Token)
		resp, err := h.HTTP.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /mcp: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("an authenticated GET /mcp answered %d, want 405", resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
			t.Errorf("the 405 carries Allow: %q, want POST", allow)
		}
	})
}

// TestSlice3ProtocolRevisions asserts section 8.1's two revisions: the current
// one, and `2025-11-25` accepted for compatibility.
func TestSlice3ProtocolRevisions(t *testing.T) {
	h := newHarness(t)

	// `2026-07-28` is reached through `server/discover`, and only through it.
	//
	// `initialize` is deprecated at that revision, so the SDK caps it at
	// `2025-11-25` -- asserted below. Driving `server/discover` by hand,
	// rather than only reading what the SDK client negotiated, is what makes
	// this an assertion about the SERVER: the client's own answer would still
	// look right if it had negotiated with itself.
	discover := h.rawCallWith(h.Token, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo": map[string]any{
				"name": "test", "version": "0",
			},
		},
	}, atProtocol(mcp.ProtocolVersion))
	if discover.Result == nil {
		t.Fatalf("server/discover answered no result: %s", discover.Raw)
	}
	supported, _ := discover.Result["supportedVersions"].([]any)
	if !containsString(supported, mcp.ProtocolVersion) {
		t.Fatalf("server/discover offers %v, which does not include section 8.1's %q",
			supported, mcp.ProtocolVersion)
	}

	// And the reference client, which uses that handshake, really lands on it.
	negotiated := h.session(h.Token).InitializeResult().ProtocolVersion
	if negotiated != mcp.ProtocolVersion {
		t.Fatalf("the reference client negotiated %q, want section 8.1's %q",
			negotiated, mcp.ProtocolVersion)
	}

	// `initialize` is CAPPED at `2025-11-25`, because the SDK will not
	// negotiate a revision that deprecated the handshake being used. Above
	// the cap it answers the cap; at or below it, ordinary negotiation, so it
	// answers what was asked for. That is the SDK's rule and not a shortfall
	// of this server, and it is asserted so that a reader who finds
	// `2025-11-25` on the wire can see it was expected rather than a bug.
	for asked, want := range map[string]string{
		mcp.ProtocolVersion:       mcp.ProtocolVersionCompat,
		mcp.ProtocolVersionCompat: mcp.ProtocolVersionCompat,
		"2025-06-18":              "2025-06-18",
	} {
		// Plain `initialize` params: no SEP-2575 `_meta`, because that is
		// what a client speaking the deprecated handshake sends.
		answer := h.rawCall(h.Token, "initialize", map[string]any{
			"protocolVersion": asked,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		})
		if answer.Result == nil {
			t.Fatalf("initialize at %s answered no result: %s", asked, answer.Raw)
		}
		if got := answer.Result["protocolVersion"]; got != want {
			t.Errorf("initialize asking for %s answered %v, want %s", asked, got, want)
		}
	}
}

func containsString(values []any, want string) bool {
	for _, v := range values {
		if s, _ := v.(string); s == want {
			return true
		}
	}
	return false
}

// TestSlice3Test28ServerInfo is the MCP half of acceptance test 28.
//
// `serverInfo` reports a `commit` equal to the built commit and a
// `source_url` -- the AGPL section 13 obligation of section 1.4. A deployment
// that served neither would be a licence gap rather than merely an incomplete
// object.
func TestSlice3Test28ServerInfo(t *testing.T) {
	h := newHarness(t)
	answer := h.call("initialize", nil)
	info, ok := answer.Result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize answered no serverInfo: %s", answer.Raw)
	}
	if info["name"] != mcp.ServerName {
		t.Errorf("serverInfo.name is %v, want agent-gm", info["name"])
	}
	// The built commit rides in `serverInfo.version` as semver build
	// metadata and the source URL in `serverInfo.websiteUrl`, because those
	// are the only two fields a reference client keeps -- see ServerVersion
	// for why `_meta` does not survive `server/discover`. This assertion is
	// made on the result THE OFFICIAL CLIENT HOLDS, not on the wire, which
	// is the whole point: a licence obligation nobody's client can read is
	// not met.
	wantVersion := mcp.ServerVersion(testVersion, testCommit)
	if info["version"] != wantVersion {
		t.Errorf("serverInfo.version is %v, want %q -- the built version and the built commit",
			info["version"], wantVersion)
	}
	sourceURL, _ := info["websiteUrl"].(string)
	if sourceURL == "" {
		t.Fatal("serverInfo carries no websiteUrl, which is the AGPL section 13 obligation of section 1.4")
	}
	if !strings.Contains(sourceURL, testCommit) {
		t.Errorf("source_url %q does not point at the built commit %q", sourceURL, testCommit)
	}

	// The same two facts are on GET /v1/health, and they must agree: two
	// surfaces reporting different commits is worse than one reporting none.
	rest := h.getHealth(t)
	restCommit, _ := rest["commit"].(string)
	restVersion, _ := rest["version"].(string)
	if got := info["version"]; got != mcp.ServerVersion(restVersion, restCommit) {
		t.Errorf("GET /v1/health reports version %q at commit %q, and serverInfo.version is %v",
			restVersion, restCommit, got)
	}
	if rest["source_url"] != sourceURL {
		t.Errorf("GET /v1/health reports source_url %v and serverInfo.websiteUrl is %v",
			rest["source_url"], sourceURL)
	}
}

// getHealth reads GET /v1/health over the same listener, which is mounted
// behind the same wrapper the MCP endpoint is.
func (h *harness) getHealth(t *testing.T) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.HTTP.URL+"/v1/health", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := h.HTTP.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decoding health: %v", err)
	}
	return parsed.Data
}

// TestSlice3Test22Attachments is acceptance test 22.
//
// `get_attachment` returns inline image content under
// `media.inline_mcp_image_max_bytes` and a resource link above it, **and the
// download ticket either way**; the first content block is the text summary.
func TestSlice3Test22Attachments(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount(addressA)
	h.seedConversation(a, "thread-a")

	// A small PNG: well under the 1 MiB default, and a type the media layer
	// serves as an image rather than as octet-stream.
	small := pngBytes(64)
	smallRow := h.seedAttachment(a, "thread-a", "m-small", small, "image/png", "small.png")

	result := h.tool("get_attachment", map[string]any{"attachment_id": smallRow.ID})
	if isError(result) {
		t.Fatalf("get_attachment was refused: %v", resultError(t, result))
	}
	blocks := contentBlocks(t, result)
	if len(blocks) == 0 {
		t.Fatal("get_attachment returned no content blocks")
	}
	// **The first content block is the text summary**, so a client that reads
	// only the first block reads a sentence rather than a megabyte of base64.
	if blocks[0]["type"] != "text" {
		t.Fatalf("the first content block is %v, want the text summary", blocks[0]["type"])
	}
	if text, _ := blocks[0]["text"].(string); !strings.Contains(text, "get_attachment") {
		t.Errorf("the summary does not say what was returned: %q", text)
	}

	data := dataOf(t, result)
	// **The download ticket comes back either way.**
	assertTicket(t, data)

	if data["inline"] != true {
		t.Fatalf("a %d-byte PNG is not inline; the limit is 1 MiB by default", len(small))
	}
	if len(blocks) < 2 {
		t.Fatal("a small image returned no second content block")
	}
	if blocks[1]["type"] != "image" {
		t.Fatalf("a small image came back as %v, want inline image content", blocks[1]["type"])
	}
	encoded, _ := blocks[1]["data"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the inline image is not base64: %v", err)
	}
	if len(decoded) != len(small) {
		t.Errorf("the inline image is %d bytes, want the attachment's %d", len(decoded), len(small))
	}

	// Above the limit: a resource link, and the ticket still comes back.
	h.setInlineLimit(t, 16)
	result = h.tool("get_attachment", map[string]any{"attachment_id": smallRow.ID})
	if isError(result) {
		t.Fatalf("get_attachment was refused after the limit was lowered: %v", resultError(t, result))
	}
	blocks = contentBlocks(t, result)
	if blocks[0]["type"] != "text" {
		t.Fatalf("the first content block is %v, want the text summary", blocks[0]["type"])
	}
	data = dataOf(t, result)
	assertTicket(t, data)
	if data["inline"] != false {
		t.Fatal("an image over the limit is still marked inline")
	}
	if len(blocks) < 2 || blocks[1]["type"] != "resource_link" {
		t.Fatalf("an image over the limit did not come back as a resource link: %v", blocks)
	}
	if uri, _ := blocks[1]["uri"].(string); uri != mcp.AttachmentURIPrefix+smallRow.ID {
		t.Fatalf("the resource link is %q, want %q", uri, mcp.AttachmentURIPrefix+smallRow.ID)
	}
}

func assertTicket(t *testing.T, data map[string]any) {
	t.Helper()
	if token, _ := data["token"].(string); token == "" {
		t.Error("get_attachment returned no download token; the ticket comes back EITHER WAY")
	}
	if url, _ := data["download_url"].(string); url == "" {
		t.Error("get_attachment returned no download_url")
	}
	if curl, _ := data["curl"].(string); curl == "" {
		t.Error("get_attachment returned no curl command")
	}
}

// setInlineLimit lowers `media.inline_mcp_image_max_bytes`, which is what
// makes "by size, not preference" testable without a megabyte fixture.
func (h *harness) setInlineLimit(t *testing.T, bytes int64) {
	t.Helper()
	body := map[string]json.RawMessage{
		"media.inline_mcp_image_max_bytes": json.RawMessage(strconv.FormatInt(bytes, 10)),
	}
	if _, err := h.Deps.Settings.Patch(context.Background(), body); err != nil {
		t.Fatalf("lowering the inline limit: %v", err)
	}
}

// TestSlice3Resources covers the resource half of section 8.2.
func TestSlice3Resources(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount(addressA)
	h.seedConversation(a, "thread-a")
	text := []byte("a plain text attachment")
	row := h.seedAttachment(a, "thread-a", "m-text", text, "text/plain", "note.txt")
	png := pngBytes(48)
	pngRow := h.seedAttachment(a, "thread-a", "m-png", png, "image/png", "shot.png")

	t.Run("resources/list is empty on purpose", func(t *testing.T) {
		answer := h.call("resources/list", map[string]any{})
		list, _ := answer.Result["resources"].([]any)
		if len(list) != 0 {
			t.Fatalf("resources/list returned %d entries; attachments are addressed by template, not enumerated", len(list))
		}
	})

	t.Run("resources/templates/list offers the attachment template", func(t *testing.T) {
		answer := h.call("resources/templates/list", map[string]any{})
		list, _ := answer.Result["resourceTemplates"].([]any)
		if len(list) != 1 {
			t.Fatalf("resources/templates/list returned %d templates, want 1: %s", len(list), answer.Raw)
		}
		entry, _ := list[0].(map[string]any)
		if entry["uriTemplate"] != mcp.AttachmentURITemplate {
			t.Fatalf("the template is %v, want %q", entry["uriTemplate"], mcp.AttachmentURITemplate)
		}
	})

	t.Run("the template is offered only to messages:read", func(t *testing.T) {
		writeOnly := h.narrowToken("messages:write")
		answer := h.callWith(writeOnly, "resources/templates/list", map[string]any{}, nil)
		list, _ := answer.Result["resourceTemplates"].([]any)
		if len(list) != 0 {
			t.Fatalf("a messages:write-only token was offered %d templates", len(list))
		}
	})

	t.Run("text media comes back as text", func(t *testing.T) {
		answer := h.call("resources/read", map[string]any{"uri": mcp.AttachmentURIPrefix + row.ID})
		contents := firstContent(t, answer.Result)
		if got, _ := contents["text"].(string); got != string(text) {
			t.Fatalf("the text attachment came back as %q, want %q", got, text)
		}
		if _, base64Present := contents["blob"]; base64Present {
			t.Error("a text attachment was also base64-encoded")
		}
	})

	t.Run("everything else comes back base64", func(t *testing.T) {
		answer := h.call("resources/read", map[string]any{"uri": mcp.AttachmentURIPrefix + pngRow.ID})
		contents := firstContent(t, answer.Result)
		encoded, _ := contents["blob"].(string)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("the blob is not base64: %v", err)
		}
		if len(decoded) != len(png) {
			t.Fatalf("the blob is %d bytes, want %d", len(decoded), len(png))
		}
	})

	// Every resources/read failure is a JSON-RPC ERROR, not an isError
	// result. A ReadResourceResult has no isError field and MUST carry
	// `contents`, so a "result" reporting a failure is not a valid result:
	// the official TypeScript SDK rejects it on schema before the client's
	// own code runs, which is the opposite of what the isError rule is for.
	// Section 8.2's isError paragraph is about tools/call, and this is the
	// boundary of its scope.
	//
	// Plant: return `rpcResult(req.ID, errorResult(...))` from any of the
	// three refusals in resources.go and one of these fails. Planted
	// 2026-09-07 after reviewer finding R-1.
	t.Run("a resource read needs messages:read", func(t *testing.T) {
		writeOnly := h.narrowToken("messages:write")
		answer := h.callWith(writeOnly, "resources/read",
			map[string]any{"uri": mcp.AttachmentURIPrefix + row.ID}, nil)
		if answer.Error == nil {
			t.Fatalf("a messages:write-only token read an attachment: %v", answer.Result)
		}
		if answer.Result != nil {
			t.Errorf("the refusal also carried a result: %v", answer.Result)
		}
	})

	t.Run("an unknown URI scheme is a JSON-RPC resource-not-found", func(t *testing.T) {
		answer := h.call("resources/read", map[string]any{"uri": "file:///etc/passwd"})
		if answer.Error == nil {
			t.Fatalf("a file:// URI was served: %v", answer.Result)
		}
		// -32602, which is what `mcp.ResourceNotFoundError` returns at
		// v1.7.0: SEP-2164 moved resource-not-found off the old -32002 and
		// the SDK is the authority for protocol codes (decision D36). The
		// code can be this one only because this server does not adopt the
		// SDK's SEP-2575 HTTP status mapping -- see
		// TestAJSONRPCErrorDoesNotEndTheSession.
		if int64(answer.Error.Code) != sdkjsonrpc.CodeInvalidParams {
			t.Errorf("the error code is %d, want the SDK's resource-not-found %d",
				answer.Error.Code, sdkjsonrpc.CodeInvalidParams)
		}
	})

	t.Run("an unknown attachment is a JSON-RPC resource-not-found", func(t *testing.T) {
		answer := h.call("resources/read",
			map[string]any{"uri": mcp.AttachmentURIPrefix + "att_00000000-0000-0000-0000-000000000000"})
		if answer.Error == nil {
			t.Fatalf("an attachment that does not exist was served: %v", answer.Result)
		}
		if int64(answer.Error.Code) != sdkjsonrpc.CodeInvalidParams {
			t.Errorf("the error code is %d, want the SDK's resource-not-found %d",
				answer.Error.Code, sdkjsonrpc.CodeInvalidParams)
		}
	})
}

func firstContent(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	list, ok := result["contents"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("the read returned no contents: %v", result)
	}
	obj, _ := list[0].(map[string]any)
	return obj
}

// TestSlice3ResultShape asserts the `structuredContent` shape section 8.2
// fixes -- `{data, next_cursor, warnings}`, the REST envelope minus the
// request ID -- and that the text half says whether more exists.
func TestSlice3ResultShape(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount(addressA)
	h.seedConversation(a, "thread-a")
	for _, id := range []string{"m1", "m2", "m3"} {
		h.seedMessage(a, "thread-a", id)
	}

	result := h.tool("list_messages", map[string]any{"limit": 2})
	sc := structured(t, result)
	for _, key := range []string{"data", "next_cursor", "warnings"} {
		if _, ok := sc[key]; !ok {
			t.Errorf("structuredContent has no %q", key)
		}
	}
	if _, leaked := sc["request_id"]; leaked {
		t.Error("structuredContent carries request_id, which section 8.2 leaves out")
	}
	cursor, _ := sc["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("a page of 2 out of 3 returned no next_cursor")
	}
	blocks := contentBlocks(t, result)
	text, _ := blocks[0]["text"].(string)
	if !strings.Contains(text, "2") {
		t.Errorf("the summary does not say how many were returned: %q", text)
	}
	if !strings.Contains(text, "more") {
		t.Errorf("the summary does not say more exists, so a model reading only the text is misled: %q", text)
	}

	// The last page says so.
	last := h.tool("list_messages", map[string]any{"limit": 2, "cursor": cursor})
	text, _ = contentBlocks(t, last)[0]["text"].(string)
	if !strings.Contains(text, "no more") {
		t.Errorf("the last page's summary does not say it is the last: %q", text)
	}
}

// pngBytes builds a valid PNG header followed by filler, so the media layer
// sees an image rather than an unrecognised type.
func pngBytes(n int) []byte {
	header := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	out := make([]byte, 0, len(header)+n)
	out = append(out, header...)
	for range n {
		out = append(out, 0x00)
	}
	return out
}

var _ = store.Attachment{}
