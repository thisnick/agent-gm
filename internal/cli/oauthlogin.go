package cli

// `agm auth login` without `--admin`: the OAuth flow of spec section 11.5.
//
// **It performs the same flow as any other MCP client** -- discovery, dynamic
// registration, a loopback callback on 127.0.0.1, PKCE `S256`, and
// verification of `state` and the RFC 9207 `iss` before the code is exchanged.
// That is not a convenience; it is the point. If `agm` had a private path to a
// token, the flow claude.ai and ChatGPT have to walk would be exercised only
// by them, and only in production.
//
// Two refusals are worth naming here because they are easy to leave out:
//
//   - a callback whose `iss` does not match is refused, **and so is one
//     carrying no `iss` at all**, because this server's metadata advertises
//     `authorization_response_iss_parameter_supported` (RFC 9207 section 2.4)
//     and a callback without it is not one this server sent;
//   - `admin` is refused at `--scopes`. It is issued only by
//     `agm auth login --admin`, which presents AGENT_GM_ADMIN_SECRET.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// loginTimeout bounds the whole browser half of the flow. The owner has to
// read a screen, find an enrollment code and approve a request, so it is
// generous -- but it is bounded, because a CLI that waits for ever on a
// listener nobody will reach is a CLI that has to be killed.
const loginTimeout = 15 * time.Minute

// discovery is the two documents of spec section 9.2, as this client reads
// them.
type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	// Resource comes from the protected-resource document and is the value
	// `resource` must carry at /oauth/authorize.
	Resource string `json:"-"`
}

// oauthLogin runs the whole flow and returns the profile to store.
func (r *runner) oauthLogin(inv *invocation) error {
	scopes, err := requestedScopes(inv.str("--scopes"))
	if err != nil {
		return err
	}

	meta, err := r.discover()
	if err != nil {
		return err
	}

	// The callback listener is bound BEFORE the client is registered, because
	// its port is part of the redirect URI being registered. A registration
	// naming a port nothing is listening on is a registration that cannot
	// complete.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &LocalError{Msg: "no loopback port was available for the callback", Err: err}
	}
	defer func() { _ = listener.Close() }()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	clientID, err := r.registerClient(meta, redirectURI)
	if err != nil {
		return err
	}

	verifier, challenge, err := newPKCE()
	if err != nil {
		return err
	}
	state, err := randomValue()
	if err != nil {
		return err
	}

	authorizeURL := meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {meta.Resource},
		"scope":                 {strings.Join(scopes, " ")},
	}.Encode()

	// Prove the destination writable first. Nothing has been minted yet
	// (spec section 11.5's write-back safety).
	pending, err := r.store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()

	if inv.boolean("--no-browser") {
		r.out.Infof("Open this URL to authorize:\n%s", authorizeURL)
	} else {
		r.out.Infof("Opening the authorization screen. If nothing opens, use:\n%s", authorizeURL)
		openBrowser(authorizeURL)
	}
	r.out.Infof("You will need an enrollment code: run `agm admin enrollment-codes create <label>` " +
		"with an admin session, then approve the request with " +
		"`agm admin authorization-requests approve <authreq-id>`.")

	code, err := r.awaitCallback(listener, state, meta.Issuer)
	if err != nil {
		return err
	}

	tokens, err := r.exchangeCode(meta, clientID, redirectURI, code, verifier)
	if err != nil {
		return err
	}

	granted := strings.Fields(tokens.Scope)
	// `expires_in` is a duration; the profile records an INSTANT, because
	// that is what the proactive refresh of section 11.5 compares the clock
	// against on a later invocation. A response without one records nothing
	// and falls back to refreshing on refusal.
	expiresAt := ""
	if tokens.ExpiresIn > 0 {
		expiresAt = r.env.Now().UTC().
			Add(time.Duration(tokens.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if err := r.store.SaveProfile(pending, r.cred.Profile, Profile{
		Server:       r.cred.Server,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Scopes:       granted,
		ExpiresAt:    expiresAt,
		Issuer:       meta.Issuer,
		Resource:     meta.Resource,
		ClientID:     clientID,
	}); err != nil {
		return err
	}

	r.out.Infof("Logged in to %s", meta.Issuer)
	if len(granted) > 0 {
		r.out.Infof("Scopes: %s", strings.Join(granted, " "))
	}
	// The tokens are stored, not printed: stdout is a transcript, and a
	// transcript is not where a bearer token belongs (spec section 12.1).
	return r.out.Emit(&Response{
		Status: http.StatusOK,
		Data: json.RawMessage(mustMarshal(map[string]any{
			"issuer":    meta.Issuer,
			"resource":  meta.Resource,
			"client_id": clientID,
			"scopes":    granted,
		})),
	})
}

// requestedScopes parses `--scopes` and refuses `admin` there.
func requestedScopes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"messages:read", "messages:write"}, nil
	}
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	for _, s := range fields {
		if s == "admin" {
			return nil, usageErr("admin is never issued through OAuth. It comes only from " +
				"`agm auth login --admin`, which presents AGENT_GM_ADMIN_SECRET")
		}
	}
	return fields, nil
}

// discover fetches both documents of section 9.2 and checks the one thing a
// client must check about them: that the issuer is the server it asked.
func (r *runner) discover() (discovery, error) {
	var meta discovery

	resourceDoc := struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}{}
	if err := r.getJSON(r.cred.Server+"/.well-known/oauth-protected-resource/mcp", &resourceDoc); err != nil {
		return meta, err
	}
	if err := r.getJSON(r.cred.Server+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return meta, err
	}
	meta.Resource = resourceDoc.Resource

	// Byte equality, not parsed-URL equivalence. A client that normalises
	// before comparing is a client that would accept an issuer it did not
	// ask for (spec section 9.2).
	if meta.Issuer != strings.TrimRight(r.cred.Server, "/") {
		return meta, &ContractError{Msg: fmt.Sprintf(
			"the server at %s advertises the issuer %q; a profile is bound to an exact "+
				"issuer and this one does not match", r.cred.Server, meta.Issuer)}
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.RegistrationEndpoint == "" {
		return meta, &ContractError{Msg: "the authorization server metadata is missing an endpoint"}
	}
	if meta.Resource == "" {
		return meta, &ContractError{Msg: "the protected-resource metadata names no resource"}
	}
	return meta, nil
}

// registerClient performs dynamic client registration. There is no other way
// to obtain a client_id: client resolution is DCR-only (D20).
func (r *runner) registerClient(meta discovery, redirectURI string) (string, error) {
	body := mustMarshal(map[string]any{
		"client_name":                "agm",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := r.postJSON(meta.RegistrationEndpoint, body, &registered); err != nil {
		return "", err
	}
	if registered.ClientID == "" {
		return "", &ContractError{Msg: "registration answered without a client_id"}
	}
	return registered.ClientID, nil
}

// awaitCallback serves the loopback redirect once and returns the code.
//
// It verifies `state` and `iss` BEFORE returning, and refuses a callback
// carrying no `iss` at all: the server's metadata advertises
// `authorization_response_iss_parameter_supported`, so a callback without one
// did not come from it (RFC 9207 section 2.4).
func (r *runner) awaitCallback(listener net.Listener, state, issuer string) (string, error) {
	type outcome struct {
		code string
		err  error
	}
	results := make(chan outcome, 1)

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			q := req.URL.Query()
			var result outcome
			switch {
			case q.Get("error") != "":
				result.err = &apierrFromCallback{code: q.Get("error"), description: q.Get("error_description")}
			case q.Get("state") != state:
				result.err = &ContractError{Msg: "the callback's state does not match the one sent; " +
					"the response was not the one this login started"}
			case q.Get("iss") == "":
				result.err = &ContractError{Msg: "the callback carries no iss. This server advertises " +
					"authorization_response_iss_parameter_supported, so a callback without one " +
					"did not come from it (RFC 9207)"}
			case q.Get("iss") != issuer:
				result.err = &ContractError{Msg: fmt.Sprintf(
					"the callback's iss is %q and the issuer is %q", q.Get("iss"), issuer)}
			case q.Get("code") == "":
				result.err = &ContractError{Msg: "the callback carries no code"}
			default:
				result.code = q.Get("code")
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if result.err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "Authorization failed. Return to the terminal.\n")
			} else {
				_, _ = io.WriteString(w, "Authorized. You can close this window.\n")
			}
			select {
			case results <- result:
			default:
			}
		}),
	}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	select {
	case <-r.ctx.Done():
		return "", &LocalError{Msg: "the login was cancelled", Err: r.ctx.Err()}
	case <-time.After(loginTimeout):
		return "", &LocalError{Msg: "the authorization was not completed within " +
			loginTimeout.String()}
	case out := <-results:
		return out.code, out.err
	}
}

// tokenResponse is what /oauth/token answers.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (r *runner) exchangeCode(meta discovery, clientID, redirectURI, code, verifier string) (tokenResponse, error) {
	return r.postTokenForm(meta.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {meta.Resource},
	})
}

// postTokenForm posts one form-encoded grant to `/oauth/token` and reads the
// token response. Both grants this client uses go through it -- the
// authorization code at login and the refresh token afterwards -- so the two
// cannot come to disagree about how an OAuth error is read.
func (r *runner) postTokenForm(endpoint string, form url.Values) (tokenResponse, error) {
	var out tokenResponse
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return out, &LocalError{Msg: "the token request could not be built", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.client.httpClient().Do(req)
	if err != nil {
		return out, &TransportError{Op: "POST " + endpoint, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return out, &TransportError{Op: "reading the token response", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		var oe struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &oe)
		return out, &apierrFromCallback{code: oe.Error, description: oe.Description}
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return out, &ContractError{Msg: "the token endpoint answered without an access_token"}
	}
	return out, nil
}

// apierrFromCallback is an OAuth `error` from a redirect or a token response.
// It is its own type so that ExitCodeFor can map it, and so that the message
// names the OAuth code rather than a status number.
type apierrFromCallback struct {
	code        string
	description string
}

func (e *apierrFromCallback) Error() string {
	if e.description == "" {
		return "the authorization server refused: " + e.code
	}
	return "the authorization server refused: " + e.code + " (" + e.description + ")"
}

// --- small helpers ------------------------------------------------------------

func (r *runner) getJSON(target string, dst any) error {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, target, nil)
	if err != nil {
		return &LocalError{Msg: "the request could not be built", Err: err}
	}
	req.Header.Set("Accept", "application/json")
	return r.doJSON(req, dst)
}

func (r *runner) postJSON(target string, body []byte, dst any) error {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return &LocalError{Msg: "the request could not be built", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return r.doJSON(req, dst)
}

func (r *runner) doJSON(req *http.Request, dst any) error {
	resp, err := r.client.httpClient().Do(req)
	if err != nil {
		return &TransportError{Op: req.Method + " " + req.URL.String(), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &TransportError{Op: "reading " + req.URL.String(), Err: err}
	}
	if resp.StatusCode >= 400 {
		var oe struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(raw, &oe) == nil && oe.Error != "" {
			return &apierrFromCallback{code: oe.Error, description: oe.Description}
		}
		return &ContractError{Msg: fmt.Sprintf("%s answered %d", req.URL.String(), resp.StatusCode)}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &ContractError{Msg: fmt.Sprintf("%s answered something that is not JSON", req.URL.String())}
	}
	return nil
}

func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomValue()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomValue() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", &LocalError{Msg: "no randomness was available", Err: err}
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func mustMarshal(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return out
}

// openBrowser is best effort and deliberately ignores its own failure: the
// URL has already been printed, so an owner on a machine with no opener can
// paste it. Failing the login because a browser did not start would be
// refusing to do the thing that still works.
func openBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}
