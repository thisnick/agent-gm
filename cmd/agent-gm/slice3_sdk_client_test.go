package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/thisnick/agent-gm/internal/cli"
)

// The OAuth 2.1 server of spec section 9, measured by the OFFICIAL MCP Go
// SDK's own client rather than by anything in this repository.
//
// Every other test of the flow -- the section 16 Slice 3 suite, the `agm auth
// login` test, `scripts/conformance-harness.mjs` -- drives the flow with code
// written here, against expectations written here. That proves the server is
// self-consistent. It cannot prove the server is what a real MCP client
// expects, because the same author wrote both halves of the conversation.
//
// These tests hand the client half to `github.com/modelcontextprotocol/go-sdk`
// v1.7.0: `auth.AuthorizationCodeHandler` performs protected-resource
// discovery, authorization-server discovery, dynamic client registration,
// PKCE, the authorization code exchange and the refresh, entirely out of the
// SDK's code. The only thing this file supplies is the part a real deployment
// supplies too -- the OWNER's browser, and the owner's approval -- because
// section 9.5 puts a human between the client and the code, and an SDK cannot
// play a human.
//
// Where the SDK and this server disagree, the SDK is the reference and the
// disagreement is a finding about this server.

// sdkRedirect is the reference client's registered redirect. It is never
// dialled: the test reads the `Location` of the completion redirect itself,
// which is exactly what a browser hands back to a native client's listener.
const sdkRedirect = "http://127.0.0.1/sdk-reference-callback"

// sdkClientMetadata is the RFC 7591 registration body the SDK sends.
func sdkClientMetadata() *oauthex.ClientRegistrationMetadata {
	return &oauthex.ClientRegistrationMetadata{
		ClientName:              "the mcp go sdk reference client",
		RedirectURIs:            []string{sdkRedirect},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	}
}

// mcpRequest builds the request the SDK is told is the MCP endpoint. Its URL
// is the canonical resource URI, so the protected-resource document's
// `resource` has to equal it byte for byte (RFC 9728 section 3.3, which the
// SDK enforces).
func (h *oauthHarness) mcpRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.http.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req
}

// unauthenticatedMCP is the 401 that starts the whole thing. The SDK is given
// the real refusal, with the real `WWW-Authenticate`, because that header is
// how it finds the protected-resource document.
func (h *oauthHarness) unauthenticatedMCP(t *testing.T) (*http.Request, *http.Response) {
	t.Helper()
	req := h.mcpRequest(t)
	resp := h.do(h.mcpRequest(t))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated /mcp request answered %d, want 401 so that the "+
			"reference client has a challenge to discover from\n%s",
			resp.StatusCode, readBody(t, resp))
	}
	return req, resp
}

// browser plays the owner's browser for one authorization URL.
//
// It is the enrollment screen of section 9.5 driven exactly as
// scripts/conformance-harness.mjs drives it: load the page, carry the
// `Secure` context cookie by hand, echo every hidden field back, submit the
// enrollment code and the selected scopes, approve through the real `agm
// admin authorization-requests approve` command, then complete.
type browser struct {
	h *oauthHarness
	t *testing.T
	// admin is the bootstrap token the approval command authenticates with.
	admin string
	// enrollment is the one-time code the enrollment screen asks for.
	enrollment string
	// tamperChallenge, when non-empty, replaces the SDK's own PKCE challenge
	// in the authorization URL. It is how the mismatched-verifier test gets a
	// code the SDK's verifier cannot redeem.
	tamperChallenge string
	// skipApproval leaves the request pending, which is how the
	// approval-required test proves a code is not minted without the owner.
	skipApproval bool
	// requestID is the authorization request the last run produced.
	requestID string
	// completion is the status the completion POST answered with.
	completion *http.Response
}

func (b *browser) fetch(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	t := b.t
	t.Helper()

	authorizeURL := args.URL
	if b.tamperChallenge != "" {
		u, err := url.Parse(authorizeURL)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("code_challenge", b.tamperChallenge)
		u.RawQuery = q.Encode()
		authorizeURL = u.String()
	}

	req, err := http.NewRequest(http.MethodGet, authorizeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	screen := b.h.do(req)
	if screen.StatusCode != http.StatusOK {
		t.Fatalf("the authorization screen the SDK asked for answered %d\n%s",
			screen.StatusCode, readBody(t, screen))
	}
	b.h.captureCookie(screen)
	page := readBody(t, screen)
	form := hiddenFields(t, page)

	// The scopes the screen offers are the ones the SDK asked for, which it
	// took from the 401's challenge. Selecting exactly those is what an owner
	// clicking "allow" does.
	for _, scope := range strings.Fields(form.Get("scope")) {
		form.Add("scope_selected", scope)
	}
	if len(form["scope_selected"]) == 0 {
		for _, scope := range []string{"messages:read", "messages:write"} {
			form.Add("scope_selected", scope)
		}
	}
	form.Set("enrollment_code", b.enrollment)

	submitted := b.h.postForm("/oauth/authorize", form, b.h.withCookie)
	if submitted.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting the enrollment form answered %d, want 303\n%s",
			submitted.StatusCode, readBody(t, submitted))
	}
	b.requestID = strings.TrimPrefix(submitted.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(t, submitted)

	if !b.skipApproval {
		b.approve()
	}

	complete := b.h.postForm("/oauth/requests/"+b.requestID+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, b.h.withCookie, b.h.sameOrigin)
	b.completion = complete
	if complete.StatusCode != http.StatusSeeOther {
		return nil, &browserRefusal{status: complete.StatusCode, body: readBody(t, complete)}
	}
	location, err := url.Parse(complete.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, complete)
	q := location.Query()
	if q.Get("code") == "" {
		return nil, &browserRefusal{status: complete.StatusCode, body: location.String()}
	}
	return &auth.AuthorizationResult{
		Code:  q.Get("code"),
		State: q.Get("state"),
		Iss:   q.Get("iss"),
	}, nil
}

// approve runs the REAL `agm admin authorization-requests approve` command --
// the same `cli.Run` entry point the binary calls -- rather than posting to
// the admin route by hand. The owner's half of section 9.5 is a command, and
// a test that skipped the command would leave the command unmeasured.
func (b *browser) approve() {
	b.t.Helper()
	var stdout, stderr lockedBuffer
	env := map[string]string{"AGENT_GM_ACCESS_TOKEN": b.admin}
	exit := cli.Run(cli.Env{
		Args: []string{
			"admin", "authorization-requests", "approve", b.requestID,
			"--server", b.h.http.URL,
			"--credentials-file", filepath.Join(b.t.TempDir(), "credentials.json"),
			"--json",
		},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(k string) string { return env[k] },
	})
	if exit != 0 {
		b.t.Fatalf("`agm admin authorization-requests approve %s` exited %d\nstderr:\n%s\nstdout:\n%s",
			b.requestID, exit, stderr.String(), stdout.String())
	}
}

// browserRefusal is what the fetcher returns when the flow produced no code.
// The SDK passes it back out of Authorize unwrapped, so the test can say
// exactly what the owner's browser saw.
type browserRefusal struct {
	status int
	body   string
}

func (e *browserRefusal) Error() string {
	return "the completion produced no authorization code: status " +
		http.StatusText(e.status) + "; " + e.body
}

// sdkHandler builds the reference client and returns it together with the
// oauth2 config and the first token the SDK constructed, which the refresh
// assertions need.
//
// The config is captured through the SDK's own `NewTokenSource` hook rather
// than reconstructed here: a refresh driven by a hand-built config would be
// this repository talking to itself again, which is the exact thing this file
// exists to stop doing.
type sdkClient struct {
	handler *auth.AuthorizationCodeHandler
	cfg     *oauth2.Config
	first   *oauth2.Token
}

func newSDKClient(t *testing.T, b *browser, requestRefresh bool) *sdkClient {
	t.Helper()
	out := &sdkClient{}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: sdkClientMetadata(),
		},
		RedirectURL:              sdkRedirect,
		RequestRefreshToken:      requestRefresh,
		AuthorizationCodeFetcher: b.fetch,
		Client:                   &http.Client{Timeout: 30 * time.Second},
		NewTokenSource: func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			out.cfg, out.first = cfg, tok
			return cfg.TokenSource(ctx, tok), nil
		},
	})
	if err != nil {
		t.Fatalf("the SDK refused this configuration: %v", err)
	}
	out.handler = handler
	return out
}

// Test A. The official SDK's authorization-code client, end to end.
//
// Discovery, dynamic registration, PKCE, the owner's enrollment and approval,
// the code exchange, a request to /mcp that is authorized, the SDK's refresh,
// and rotation of the refresh token. Nothing in the client half is ours.
func TestSlice3SDKReferenceClientCompletesTheFlow(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	admin := h.adminToken()
	_, enrollment := h.enrollmentCode(admin, map[string]any{
		"label":  "the mcp go sdk reference client",
		"scopes": []string{"messages:read", "messages:write", "messages:delete"},
	})

	b := &browser{h: h, t: t, admin: admin, enrollment: enrollment}
	client := newSDKClient(t, b, true)

	ctx := context.Background()
	req, resp := h.unauthenticatedMCP(t)
	if err := client.handler.Authorize(ctx, req, resp); err != nil {
		t.Fatalf("the official MCP Go SDK could not complete this server's OAuth flow: %v", err)
	}

	source, err := client.handler.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if source == nil {
		t.Fatal("the SDK finished authorizing and holds no token source")
	}
	first, err := source.Token()
	if err != nil {
		t.Fatalf("reading the SDK's token: %v", err)
	}
	if first.AccessToken == "" {
		t.Fatal("the SDK's token carries no access token")
	}
	if first.RefreshToken == "" {
		t.Fatal("the SDK asked for a refresh token and this server issued none; the " +
			"refresh_token grant is advertised in the authorization-server document")
	}

	// The token works. `internal/mcp` is being rewritten, so the assertion is
	// deliberately about AUTHORIZATION and nothing else: the request is not
	// refused as unauthenticated or as insufficiently scoped.
	assertAuthorized(t, h, first.AccessToken, "the token the SDK obtained")

	// The refresh, through the SDK's own oauth2 config. The token is presented
	// already expired, which is exactly what the SDK's reuse token source does
	// on its own once the access token ages out.
	stale := *client.first
	stale.Expiry = time.Now().Add(-time.Minute)
	refreshed, err := client.cfg.TokenSource(ctx, &stale).Token()
	if err != nil {
		t.Fatalf("the SDK's refresh path failed: %v", err)
	}
	if refreshed.AccessToken == first.AccessToken {
		t.Error("the refresh returned the same access token")
	}
	if refreshed.RefreshToken == "" {
		t.Fatal("the refresh returned no refresh token, so the SDK cannot refresh again")
	}
	if refreshed.RefreshToken == client.first.RefreshToken {
		t.Error("the refresh token was not rotated: the server handed back the value it " +
			"was given, so a stolen refresh token would live for ever (section 9.6)")
	}
	assertAuthorized(t, h, refreshed.AccessToken, "the token the SDK refreshed to")

	// Rotation is only rotation if the spent value is dead. The SDK's own
	// config presents the superseded refresh token; the server must refuse it.
	spent := *client.first
	spent.Expiry = time.Now().Add(-time.Minute)
	if _, err := client.cfg.TokenSource(ctx, &spent).Token(); err == nil {
		t.Error("the server accepted a refresh token that had already been spent; " +
			"rotation without reuse detection rotates nothing (section 9.6)")
	}
}

// assertAuthorized proves a bearer reaches /mcp as an authorized caller.
//
// It asserts NOT-401 and NOT-403 rather than a particular MCP body on
// purpose: the MCP handler is being rewritten in this slice, and a test of
// the OAuth server that fails when the tool list changes is a test of the
// wrong thing. 401 and 403 are the two answers that would mean the token did
// not work, and they are the two this rules out.
func assertAuthorized(t *testing.T, h *oauthHarness, token, what string) {
	t.Helper()
	req := h.mcpRequest(t)
	bearer(token)(req)
	resp := h.do(req)
	body := readBody(t, resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("%s was refused at /mcp with %d %s\nWWW-Authenticate: %s\n%s",
			what, resp.StatusCode, http.StatusText(resp.StatusCode),
			resp.Header.Get("WWW-Authenticate"), body)
	}
}

// Test B. PKCE is verified.
//
// The browser rewrites the `code_challenge` in the authorization URL the SDK
// generated, so the code is bound to a challenge the SDK's own verifier does
// not match. Everything else is unchanged and the owner really approves. The
// exchange must fail: if it succeeds, the code_verifier is decoration.
func TestSlice3SDKReferenceClientMismatchedVerifierIsRefused(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	admin := h.adminToken()
	_, enrollment := h.enrollmentCode(admin, map[string]any{
		"label":  "a mismatched verifier",
		"scopes": []string{"messages:read", "messages:write", "messages:delete"},
	})

	// A challenge for a verifier the SDK has never seen.
	otherVerifier := "a-verifier-the-sdk-does-not-hold-0123456789abcdefghijklmno"
	sum := sha256.Sum256([]byte(otherVerifier))
	b := &browser{
		h: h, t: t, admin: admin, enrollment: enrollment,
		tamperChallenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}
	client := newSDKClient(t, b, true)

	req, resp := h.unauthenticatedMCP(t)
	err := client.handler.Authorize(context.Background(), req, resp)
	if err == nil {
		t.Fatal("the SDK exchanged a code whose PKCE challenge belongs to another " +
			"verifier and got a token; the code_verifier is not being checked (section 9.6)")
	}
	if !strings.Contains(err.Error(), "token exchange failed") {
		t.Errorf("the flow failed somewhere other than the exchange, so this test did "+
			"not measure PKCE: %v", err)
	}
	if ts, _ := client.handler.TokenSource(context.Background()); ts != nil {
		t.Error("the SDK holds a token source after a failed exchange")
	}
}

// Test C. No code is minted before the owner approves.
//
// The browser does everything a real one does except call `agm admin
// authorization-requests approve`. The completion must not hand back a code:
// section 9.5 puts the owner between the enrollment screen and the callback,
// and a code minted on a pending request removes the owner from their own
// authorization.
func TestSlice3SDKReferenceClientGetsNoCodeWithoutApproval(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	admin := h.adminToken()
	_, enrollment := h.enrollmentCode(admin, map[string]any{
		"label":  "never approved",
		"scopes": []string{"messages:read", "messages:write", "messages:delete"},
	})

	b := &browser{h: h, t: t, admin: admin, enrollment: enrollment, skipApproval: true}
	client := newSDKClient(t, b, true)

	req, resp := h.unauthenticatedMCP(t)
	err := client.handler.Authorize(context.Background(), req, resp)
	if err == nil {
		t.Fatal("the SDK obtained a token for an authorization request the owner never " +
			"approved (section 9.5)")
	}
	var refusal *browserRefusal
	if !asBrowserRefusal(err, &refusal) {
		t.Fatalf("the flow failed before the completion, so this test did not measure "+
			"the approval requirement: %v", err)
	}
	if refusal.status == http.StatusSeeOther {
		t.Errorf("completing an unapproved request redirected to the callback with a "+
			"code: %s", refusal.body)
	}
	if ts, _ := client.handler.TokenSource(context.Background()); ts != nil {
		t.Error("the SDK holds a token source for an unapproved request")
	}

	// And the request is still there, still pending, for the owner to decide
	// on: a refused completion must not consume it.
	show := h.get("/v1/admin/authorization-requests/"+b.requestID, bearer(admin))
	if show.StatusCode != http.StatusOK {
		t.Fatalf("reading the request back answered %d\n%s", show.StatusCode, readBody(t, show))
	}
	if status, _ := dataOf(t, show)["status"].(string); status != "pending" {
		t.Errorf("the unapproved request's status is %q, want pending", status)
	}
}

// asBrowserRefusal unwraps the error the fetcher returned. The SDK documents
// that it leaves a fetcher's error unwrappable so the caller can handle it, so
// this is a type assertion rather than errors.As.
func asBrowserRefusal(err error, out **browserRefusal) bool {
	refusal, ok := err.(*browserRefusal)
	if ok {
		*out = refusal
	}
	return ok
}
