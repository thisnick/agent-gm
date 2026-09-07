package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The Slice 3 OAuth acceptance suite drives **the binary's own constructor**:
// `buildServer` builds the same `built` value `agent-gm serve` binds, and
// these tests serve `b.Handler` -- the OAuth server, the MCP handler and the
// REST surface mounted together -- through httptest.
//
// That is deliberate, and it is the Slice 2 review's closing lesson applied:
// every blocker that slice was rejected for was invisible to a green suite
// built over a harness that assembled a *similar* server, and visible within
// minutes of driving the real one. A harness that built its own
// `oauth.Server` would prove that `internal/oauth` works and prove nothing
// about whether `serve` mounts it.

// oauthTestIssuer is what these tests set AGENT_GM_PUBLIC_URL to.
//
// Nothing about the flow depends on the httptest listener's own address: every
// URL this server hands out is built from AGENT_GM_PUBLIC_URL and never from
// the request's Host (spec sections 10.3, 12.3), so the issuer can be a name
// that resolves nowhere while the tests dial 127.0.0.1.
const oauthTestIssuer = "https://gm.agent-wx.app"

const oauthTestAdminSecret = "a-test-admin-secret-well-over-the-43-character-minimum-0123456789"

// oauthHarness is one running server plus the small amount of state the flow
// carries between requests.
type oauthHarness struct {
	t    *testing.T
	b    *built
	http *httptest.Server
	// dir is the data directory, so a test can stop the server and start a
	// second one over the same database (section 16 Slice 3 test 11).
	dir string
	// issuer is AGENT_GM_PUBLIC_URL for this harness, which is what a
	// same-origin `Origin` header has to carry.
	issuer string
	// cookie is the signed context cookie. It is carried by hand rather than
	// by a cookie jar because the cookie is `Secure` and these tests speak
	// plain HTTP to a loopback listener, so a jar would silently drop it --
	// and a test that silently lost the cookie would "prove" the 404 rule of
	// section 9.5 while exercising nothing.
	cookie string
}

func newOAuthHarness(t *testing.T) *oauthHarness {
	t.Helper()
	dir := t.TempDir()
	return newOAuthHarnessIn(t, dir)
}

func newOAuthHarnessIn(t *testing.T, dir string) *oauthHarness {
	t.Helper()
	t.Setenv("AGENT_GM_DATA_DIR", dir)
	t.Setenv("AGENT_GM_DATA_KEY",
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("AGENT_GM_ADMIN_SECRET", oauthTestAdminSecret)
	t.Setenv("AGENT_GM_PUBLIC_URL", oauthTestIssuer)
	t.Setenv("AGENT_GM_BACKEND", "fake")
	t.Setenv("AGENT_GM_ALLOW_FAKE", "1")
	t.Setenv("AGENT_GM_LOG_LEVEL", "error")

	b, code := buildServer(context.Background(), "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	srv := httptest.NewServer(b.Handler)
	h := &oauthHarness{t: t, b: b, http: srv, dir: dir, issuer: oauthTestIssuer}
	t.Cleanup(func() {
		srv.Close()
		b.Close()
	})
	return h
}

// newOAuthHarnessOnItsOwnURL builds a server whose AGENT_GM_PUBLIC_URL IS the
// address it listens on.
//
// Every other test in this suite deliberately separates the two -- the issuer
// is a name that resolves nowhere while the tests dial loopback -- because
// that is what proves no URL is built from the request's Host. But a REAL
// client checks the issuer against the server it dialled and refuses a
// mismatch (section 11.5), so `agm auth login` cannot be driven that way. The
// listener is therefore bound first and the environment is set from it, which
// is the only order in which a process can know its own public URL before it
// starts.
func newOAuthHarnessOnItsOwnURL(t *testing.T) *oauthHarness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := "http://" + ln.Addr().String()

	dir := t.TempDir()
	t.Setenv("AGENT_GM_DATA_DIR", dir)
	t.Setenv("AGENT_GM_DATA_KEY",
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("AGENT_GM_ADMIN_SECRET", oauthTestAdminSecret)
	t.Setenv("AGENT_GM_PUBLIC_URL", public)
	t.Setenv("AGENT_GM_BACKEND", "fake")
	t.Setenv("AGENT_GM_ALLOW_FAKE", "1")
	t.Setenv("AGENT_GM_LOG_LEVEL", "error")

	b, code := buildServer(context.Background(), "127.0.0.1:0")
	if code != exitOK {
		_ = ln.Close()
		t.Fatalf("buildServer returned exit %d", code)
	}
	srv := httptest.NewUnstartedServer(b.Handler)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()

	h := &oauthHarness{t: t, b: b, http: srv, dir: dir, issuer: public}
	t.Cleanup(func() {
		srv.Close()
		b.Close()
	})
	return h
}

// sameOrigin sets the `Origin` header POST /oauth/requests/{id}/complete
// requires (spec section 9.5). It is a request option rather than something
// the harness adds everywhere, because the tests that assert the requirement
// need to be able to leave it off.
func (h *oauthHarness) sameOrigin(req *http.Request) {
	req.Header.Set("Origin", h.issuer)
}

// stop shuts the server down without removing the data directory, so a second
// harness can be started over the same database.
func (h *oauthHarness) stop() {
	h.http.Close()
	h.b.Close()
}

// do sends one request. Redirects are never followed: every redirect in this
// flow is an assertion target.
func (h *oauthHarness) do(req *http.Request) *http.Response {
	h.t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

func (h *oauthHarness) get(path string, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.http.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	return h.do(req)
}

func (h *oauthHarness) postForm(path string, form url.Values, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.http.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, o := range opts {
		o(req)
	}
	return h.do(req)
}

func (h *oauthHarness) postJSON(path string, body any, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.http.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	return h.do(req)
}

// withCookie sends the harness's stored context cookie.
func (h *oauthHarness) withCookie(req *http.Request) {
	if h.cookie != "" {
		req.Header.Set("Cookie", h.cookie)
	}
}

// captureCookie records the `agm_oauth_context` cookie a response set.
func (h *oauthHarness) captureCookie(resp *http.Response) {
	for _, c := range resp.Cookies() {
		if c.Name == "agm_oauth_context" && c.Value != "" {
			h.cookie = c.Name + "=" + c.Value
		}
	}
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func decodeEnvelope(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	body := readBody(t, resp)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return out
}

// dataOf pulls `data` out of a REST envelope.
func dataOf(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	env := decodeEnvelope(t, resp)
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("the envelope carries no data object: %v", env)
	}
	return data
}

// adminToken mints an admin bootstrap session, which is how these tests reach
// the owner's routes. It is NOT how they reach `/mcp`: the whole point of the
// suite is that a connector's token comes through the OAuth flow.
func (h *oauthHarness) adminToken() string {
	h.t.Helper()
	resp := h.postJSON("/v1/auth/admin-session", map[string]any{"secret": oauthTestAdminSecret})
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("minting an admin session: %d %s", resp.StatusCode, readBody(h.t, resp))
	}
	data := dataOf(h.t, resp)
	token, _ := data["access_token"].(string)
	if token == "" {
		h.t.Fatal("the admin session carries no access token")
	}
	return token
}

// enrollmentCode issues one code and returns its value, which is the only
// time it is ever returned.
func (h *oauthHarness) enrollmentCode(admin string, body map[string]any) (id, value string) {
	h.t.Helper()
	if body == nil {
		body = map[string]any{"label": "a test connector"}
	}
	resp := h.postJSON("/v1/admin/enrollment-codes", body, bearer(admin))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("issuing an enrollment code: %d %s", resp.StatusCode, readBody(h.t, resp))
	}
	data := dataOf(h.t, resp)
	id, _ = data["id"].(string)
	value, _ = data["code"].(string)
	if value == "" {
		h.t.Fatal("the issued code carries no value")
	}
	return id, value
}

// register performs dynamic client registration.
func (h *oauthHarness) register(redirects ...string) string {
	h.t.Helper()
	resp := h.postJSON("/oauth/register", map[string]any{
		"client_name":   "a test client",
		"redirect_uris": redirects,
	})
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("registering: %d %s", resp.StatusCode, readBody(h.t, resp))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(readBody(h.t, resp)), &out); err != nil {
		h.t.Fatal(err)
	}
	id, _ := out["client_id"].(string)
	if id == "" {
		h.t.Fatal("registration returned no client_id")
	}
	return id
}

// pkce mints a verifier and its S256 challenge.
func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

var hiddenField = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// hiddenFields pulls the approval form's hidden inputs out of the rendered
// page, which is what a browser would submit back.
func hiddenFields(t *testing.T, page string) url.Values {
	t.Helper()
	out := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(page, -1) {
		out.Set(m[1], html.UnescapeString(m[2]))
	}
	if out.Get("context") == "" || out.Get("form_token") == "" {
		t.Fatalf("the approval page carries no signed context or form token:\n%s", page)
	}
	return out
}

// authorizeParams is one authorization request's query string.
type authorizeParams struct {
	ClientID    string
	RedirectURI string
	State       string
	Challenge   string
	Resource    string
	Scope       string
	Method      string
	NoMethod    bool
}

func (p authorizeParams) query(h *oauthHarness) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURI)
	if p.State != "" {
		q.Set("state", p.State)
	}
	q.Set("code_challenge", p.Challenge)
	if !p.NoMethod {
		method := p.Method
		if method == "" {
			method = "S256"
		}
		q.Set("code_challenge_method", method)
	}
	resource := p.Resource
	if resource == "" {
		resource = oauthTestIssuer + "/mcp"
	}
	q.Set("resource", resource)
	if p.Scope != "" {
		q.Set("scope", p.Scope)
	}
	return "/oauth/authorize?" + q.Encode()
}

// flow runs the whole owner-approved flow and returns the token response.
//
// It is one function because section 8.4's conformance run does exactly this
// and a test that shortcut any step of it would leave the client-shaped path
// unmeasured.
type flowResult struct {
	ClientID     string
	RequestID    string
	Code         string
	Verifier     string
	RedirectURI  string
	State        string
	AccessToken  string
	RefreshToken string
	Scope        string
}

func (h *oauthHarness) fullFlow(redirect string, scopes ...string) flowResult {
	h.t.Helper()
	admin := h.adminToken()
	_, codeValue := h.enrollmentCode(admin, map[string]any{
		"label":  "a test connector",
		"scopes": []string{"messages:read", "messages:write", "messages:delete"},
	})
	clientID := h.register(redirect)
	verifier, challenge := pkce(h.t)
	scope := strings.Join(scopes, " ")

	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "a-state",
		Challenge: challenge, Scope: scope,
	})
	form := hiddenFields(h.t, page)
	selected := scopes
	if len(selected) == 0 {
		selected = []string{"messages:read", "messages:write"}
	}
	for _, s := range selected {
		form.Add("scope_selected", s)
	}
	form.Set("enrollment_code", codeValue)

	resp := h.postForm("/oauth/authorize", form, h.withCookie)
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("submitting the approval form: %d\n%s", resp.StatusCode, readBody(h.t, resp))
	}
	requestID := strings.TrimPrefix(resp.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(h.t, resp)

	approve := h.postJSON("/v1/admin/authorization-requests/"+requestID+"/approve",
		map[string]any{}, bearer(admin))
	if approve.StatusCode != http.StatusOK {
		h.t.Fatalf("approving: %d %s", approve.StatusCode, readBody(h.t, approve))
	}
	_ = readBody(h.t, approve)

	complete := h.postForm("/oauth/requests/"+requestID+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	if complete.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("completing: %d\n%s", complete.StatusCode, readBody(h.t, complete))
	}
	location, err := url.Parse(complete.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	_ = readBody(h.t, complete)

	out := flowResult{
		ClientID: clientID, RequestID: requestID, Verifier: verifier,
		RedirectURI: redirect, State: "a-state",
		Code:  location.Query().Get("code"),
		Scope: location.Query().Get("scope"),
	}
	if out.Code == "" {
		h.t.Fatalf("the callback carries no code: %s", location)
	}
	return out
}

// authorizePage loads the approval screen and records its cookie.
func (h *oauthHarness) authorizePage(p authorizeParams) string {
	h.t.Helper()
	resp := h.get(p.query(h))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("loading the authorization screen: %d\n%s",
			resp.StatusCode, readBody(h.t, resp))
	}
	h.captureCookie(resp)
	return readBody(h.t, resp)
}

// exchange runs the token endpoint's authorization_code grant.
func (h *oauthHarness) exchange(f flowResult, overrides url.Values) *http.Response {
	h.t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {f.Code},
		"client_id":     {f.ClientID},
		"redirect_uri":  {f.RedirectURI},
		"code_verifier": {f.Verifier},
	}
	for k, v := range overrides {
		form[k] = v
	}
	return h.postForm("/oauth/token", form)
}

// tokenValues decodes a token response.
func tokenValues(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	body := readBody(t, resp)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding the token response %q: %v", body, err)
	}
	return out
}

// openRawDB opens the database file directly, with no Agent GM code in the
// way. Test 12 uses it to read `enrollment_codes.code_hash` out of the file
// itself, so that what is asserted is what is stored rather than what the
// store layer says is stored.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// mcpServerInfo connects the OFFICIAL MCP Go SDK client to this harness's
// `/mcp` and returns the `serverInfo` the client ends up holding.
//
// It goes through the reference client on purpose. From protocol 2026-07-28 a
// client learns the server through `server/discover` and rebuilds
// `serverInfo` from that result's `_meta` itself, discarding everything else
// in it, so what a connector actually receives and what the wire carries are
// not the same object. Section 1.4's AGPL obligation is about the former.
func (h *oauthHarness) mcpServerInfo(t *testing.T, token string) *sdk.Implementation {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "agent-gm-tests", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint: h.http.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{
			token: token, next: http.DefaultTransport,
		}},
	}
	cs, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("the SDK client could not connect to /mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	info := cs.InitializeResult().ServerInfo
	if info == nil {
		t.Fatal("the SDK client holds no serverInfo after connecting")
	}
	return info
}

// bearerTransport presents one token on every request.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(clone)
}
