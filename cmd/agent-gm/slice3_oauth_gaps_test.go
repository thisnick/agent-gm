package main

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// The tests the Slice 3 review's surviving plants asked for.
//
// Every one of these covers a rule that production already obeyed and that
// nothing would have noticed losing. A plant that survives a full suite is
// not a note: it is a statement that the rule is currently held by nobody, so
// each test below names the plant it kills and the date it was planted.

// pendingRequest runs the flow as far as a pending request and returns its
// ID and the form the browser would submit next.
func (h *oauthHarness) pendingRequest(admin, redirect string) (string, url.Values) {
	h.t.Helper()
	_, codeValue := h.enrollmentCode(admin, nil)
	clientID := h.register(redirect)
	_, challenge := pkce(h.t)
	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "s", Challenge: challenge,
	})
	form := hiddenFields(h.t, page)
	form.Add("scope_selected", "messages:read")
	form.Set("enrollment_code", codeValue)
	submitted := h.postForm("/oauth/authorize", form, h.withCookie)
	if submitted.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("submitting: %d\n%s", submitted.StatusCode, readBody(h.t, submitted))
	}
	id := strings.TrimPrefix(submitted.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(h.t, submitted)
	return id, form
}

// TestSlice3CompletionNeedsAnApproval is spec section 9.5's "expired,
// completed or still-pending answers 409".
//
// **There is no self-service**, and this is the line that makes that true on
// the wire: a browser that has the cookie, the form token and a same-origin
// Origin still cannot turn a request into a code until the owner has said
// yes. Nothing else in the suite exercised the still-pending arm.
//
// Plant P24: `case store.AuthRequestApproved, store.AuthRequestPending:` in
// requests.go. Planted 2026-09-07 by the reviewer; killed here.
func TestSlice3CompletionNeedsAnApproval(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	const redirect = "http://127.0.0.1:53211/callback"

	id, form := h.pendingRequest(admin, redirect)

	before := h.authorizationCount(admin)
	complete := h.postForm("/oauth/requests/"+id+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	body := readBody(t, complete)
	if complete.StatusCode != http.StatusConflict {
		t.Fatalf("completing a request the owner has NOT approved answered %d, want 409. "+
			"There is no self-service: the owner has to say yes\n%s",
			complete.StatusCode, body)
	}
	if !strings.Contains(body, "not waiting to be completed") {
		t.Errorf("the 409 says %q; it must distinguish a request that has not been "+
			"decided from one that has already been completed, or a client cannot tell "+
			"whether to keep waiting", body)
	}
	if complete.Header.Get("Location") != "" {
		t.Error("a still-pending request redirected to the callback")
	}
	if after := h.authorizationCount(admin); after != before {
		t.Errorf("completing an unapproved request minted an authorization (%d -> %d)",
			before, after)
	}

	// And a DENIED request redirects with error=access_denied, because a
	// denial is a decision the client is entitled to hear.
	deny := h.postJSON("/v1/admin/authorization-requests/"+id+"/deny",
		map[string]any{"reason": "not mine"}, bearer(admin))
	if deny.StatusCode != http.StatusOK {
		t.Fatalf("denying answered %d\n%s", deny.StatusCode, readBody(t, deny))
	}
	_ = readBody(t, deny)

	denied := h.postForm("/oauth/requests/"+id+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	_ = readBody(t, denied)
	if denied.StatusCode != http.StatusSeeOther {
		t.Fatalf("completing a DENIED request answered %d, want a 303 carrying the denial",
			denied.StatusCode)
	}
	target, err := url.Parse(denied.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := target.Query().Get("error"); got != "access_denied" {
		t.Errorf("a denial redirected with error=%q, want access_denied", got)
	}
	if target.Query().Get("code") != "" {
		t.Error("a denied request handed out a code")
	}
	if target.Query().Get("iss") != h.issuer {
		t.Errorf("the denial redirect's iss is %q", target.Query().Get("iss"))
	}
	if after := h.authorizationCount(admin); after != before {
		t.Errorf("a denial minted an authorization (%d -> %d)", before, after)
	}
}

func (h *oauthHarness) authorizationCount(admin string) int {
	h.t.Helper()
	data := dataOf(h.t, h.get("/v1/admin/authorizations?include_revoked=true", bearer(admin)))
	items, _ := data["items"].([]any)
	return len(items)
}

// TestSlice3TheRequestIDIsBoundToOneBrowser is spec section 9.5's "without a
// valid cookie every /oauth/requests route answers 404", in the form that
// matters: a cookie from ANOTHER authorization is a valid cookie, and it must
// not open somebody else's request.
//
// Plant P15: drop the handle-hash comparison in requests.go's resolveRequest.
// Planted 2026-09-07 by the reviewer; killed here. Test 7 only ever sent NO
// cookie, which the signature check alone would have caught.
func TestSlice3TheRequestIDIsBoundToOneBrowser(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	mine, myForm := h.pendingRequest(admin, "http://127.0.0.1:53211/callback")
	myCookie := h.cookie

	// A second, independent authorization in a second browser. Loading its
	// screen replaces the harness's cookie, which is exactly the situation:
	// a live, correctly signed cookie that belongs to another request.
	theirs, _ := h.pendingRequest(admin, "http://127.0.0.1:53212/callback")
	theirCookie := h.cookie
	if myCookie == theirCookie {
		t.Fatal("the two authorizations share a cookie, so this test proves nothing")
	}

	for _, path := range []string{
		"/oauth/requests/" + mine,
		"/oauth/requests/" + mine + "/status",
	} {
		resp := h.get(path, func(r *http.Request) { r.Header.Set("Cookie", theirCookie) })
		_ = readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d to ANOTHER authorization's cookie, want 404. The "+
				"request ID conveys no authority and the cookie must be the one this "+
				"request was served to", path, resp.StatusCode)
		}
	}

	complete := h.postForm("/oauth/requests/"+mine+"/complete",
		url.Values{"form_token": {myForm.Get("form_token")}},
		func(r *http.Request) { r.Header.Set("Cookie", theirCookie) }, h.sameOrigin)
	_ = readBody(t, complete)
	if complete.StatusCode != http.StatusNotFound {
		t.Errorf("completing with another authorization's cookie answered %d, want 404",
			complete.StatusCode)
	}

	// The right cookie still works, so the 404s above are the binding and
	// not a broken fixture.
	ok := h.get("/oauth/requests/"+mine, func(r *http.Request) { r.Header.Set("Cookie", myCookie) })
	_ = readBody(t, ok)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("the request's OWN cookie answered %d", ok.StatusCode)
	}
	_ = theirs
}

// TestSlice3CompletionRequiresAPresentSameOriginOrigin is spec section 9.5's
// third requirement on `/complete`, which is easy to read as "not a foreign
// Origin" and is in fact "a same-origin Origin".
//
// A check that accepted an ABSENT header would be satisfied by any client
// that simply omitted it, which is every client that is not a browser -- and
// this endpoint is only ever reached by the waiting page's own form.
func TestSlice3CompletionRequiresAPresentSameOriginOrigin(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	id, form := h.pendingRequest(admin, "http://127.0.0.1:53211/callback")

	approve := h.postJSON("/v1/admin/authorization-requests/"+id+"/approve",
		map[string]any{}, bearer(admin))
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approving: %d", approve.StatusCode)
	}
	_ = readBody(t, approve)

	body := url.Values{"form_token": {form.Get("form_token")}}

	absent := h.postForm("/oauth/requests/"+id+"/complete", body, h.withCookie)
	_ = readBody(t, absent)
	if absent.StatusCode != http.StatusForbidden {
		t.Errorf("completing with NO Origin answered %d, want 403", absent.StatusCode)
	}

	foreign := h.postForm("/oauth/requests/"+id+"/complete", body, h.withCookie,
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	_ = readBody(t, foreign)
	if foreign.StatusCode != http.StatusForbidden {
		t.Errorf("completing with a foreign Origin answered %d, want 403", foreign.StatusCode)
	}

	same := h.postForm("/oauth/requests/"+id+"/complete", body, h.withCookie, h.sameOrigin)
	_ = readBody(t, same)
	if same.StatusCode != http.StatusSeeOther {
		t.Errorf("completing with a same-origin Origin answered %d, want 303", same.StatusCode)
	}
}

// TestSlice3ApprovalRefusesAnEmptyScopeList is spec section 9.5: "widening or
// empty is invalid_request".
//
// `{"scopes": []}` is somebody asking to grant nothing, which is a denial
// written as an approval. Treating it as "no narrowing" would grant
// everything the browser selected -- the exact opposite of what was asked.
func TestSlice3ApprovalRefusesAnEmptyScopeList(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	id, _ := h.pendingRequest(admin, "http://127.0.0.1:53211/callback")

	resp := h.postJSON("/v1/admin/authorization-requests/"+id+"/approve",
		map[string]any{"scopes": []string{}}, bearer(admin))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_request") {
		t.Fatalf(`approving with {"scopes": []} answered %d %s, want 400 invalid_request`,
			resp.StatusCode, body)
	}

	// The request is untouched: an approval that was refused is not an
	// approval.
	req := dataOf(t, h.get("/v1/admin/authorization-requests/"+id, bearer(admin)))
	if req["status"] != "pending" {
		t.Errorf("the request is now %v; a refused approval must decide nothing", req["status"])
	}

	// And an ABSENT scopes key still means "do not narrow".
	ok := h.postJSON("/v1/admin/authorization-requests/"+id+"/approve",
		map[string]any{}, bearer(admin))
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("approving with no scopes key answered %d\n%s", ok.StatusCode, readBody(t, ok))
	}
	granted := dataOf(t, ok)["granted_scopes"]
	list, _ := granted.([]any)
	if len(list) != 1 || list[0] != "messages:read" {
		t.Errorf("granted_scopes is %v, want the browser's selection", granted)
	}
}

// TestSlice3RevokeNeedsTheClientAndOnlyTouchesOAuthGrants is spec section
// 9.6: `/oauth/revoke` "takes `token` and `client_id`".
//
// A reviewer driving this endpoint found that an omitted `client_id` revoked
// whatever the token belonged to -- including an ADMIN BOOTSTRAP session,
// which no OAuth client owns. An unauthenticated endpoint that can end the
// owner's own session on a guessed value is a bigger hole than the oracle the
// endpoint's silence was protecting against.
func TestSlice3RevokeNeedsTheClientAndOnlyTouchesOAuthGrants(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)

	alive := func(token string) bool {
		resp := h.get("/v1/auth/whoami", bearer(token))
		defer func() { _ = readBody(t, resp) }()
		return resp.StatusCode == http.StatusOK
	}

	// An ADMIN session presented at /oauth/revoke, with its own "client_id"
	// invented. It answers 200 and revokes nothing.
	adminRevoke := h.postForm("/oauth/revoke", url.Values{
		"token": {admin}, "client_id": {f.ClientID},
	})
	_ = readBody(t, adminRevoke)
	if adminRevoke.StatusCode != http.StatusOK {
		t.Errorf("/oauth/revoke answered %d; it always answers 200", adminRevoke.StatusCode)
	}
	if !alive(admin) {
		t.Fatal("/oauth/revoke ended an ADMIN BOOTSTRAP session. That session belongs to " +
			"no OAuth client, and an unauthenticated endpoint must not be able to end it")
	}

	// The same admin token with NO client_id, which is the shape the
	// reviewer drove.
	noClient := h.postForm("/oauth/revoke", url.Values{"token": {admin}})
	_ = readBody(t, noClient)
	if noClient.StatusCode != http.StatusOK {
		t.Errorf("/oauth/revoke with no client_id answered %d; it always answers 200",
			noClient.StatusCode)
	}
	if !alive(admin) {
		t.Fatal("/oauth/revoke with no client_id ended the admin session")
	}

	// An OAuth token with no client_id revokes nothing either: client_id is
	// required, and its absence is answered with silence rather than action.
	if r := h.postForm("/oauth/revoke", url.Values{"token": {access}}); r.StatusCode != http.StatusOK {
		t.Errorf("answered %d", r.StatusCode)
	}
	if !alive(access) {
		t.Error("/oauth/revoke acted on a request carrying no client_id")
	}

	// With the right client, it works.
	if r := h.postForm("/oauth/revoke", url.Values{
		"token": {access}, "client_id": {f.ClientID},
	}); r.StatusCode != http.StatusOK {
		t.Errorf("answered %d", r.StatusCode)
	}
	if alive(access) {
		t.Error("/oauth/revoke did not revoke the client's own token")
	}
}

// TestSlice3RefreshIsBoundToItsClient is spec section 9.6's client binding on
// the refresh grant.
//
// Plant P5: drop the `p.ClientID != "" && auth.Client != p.ClientID` guard in
// refreshSession. Planted 2026-09-07 by the reviewer; killed here.
func TestSlice3RefreshIsBoundToItsClient(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	refresh, _ := tokens["refresh_token"].(string)

	// A second, real registration. Another client presenting somebody
	// else's refresh token is refused without being told whether it exists.
	other := h.register("http://127.0.0.1:53299/callback")

	wrong := h.postForm("/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {other},
	})
	body := readBody(t, wrong)
	if wrong.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("another client's refresh answered %d %s, want 400 invalid_grant",
			wrong.StatusCode, body)
	}

	// And it did not SPEND the token: the rightful client can still rotate.
	right := h.postForm("/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {f.ClientID},
	})
	if right.StatusCode != http.StatusOK {
		t.Fatalf("after another client's attempt the rightful client answered %d; a "+
			"refusal must not spend somebody else's token\n%s",
			right.StatusCode, readBody(t, right))
	}
	_ = readBody(t, right)
}

// TestSlice3ARacedCodeIsExchangedOnce and its refresh twin are the
// conditional-UPDATE tests.
//
// Both rules are enforced by a WHERE clause -- `AND consumed_at_ms IS NULL`,
// `AND spent_at_ms IS NULL` -- and a WHERE clause is exactly the kind of thing
// that can be deleted without any sequential test noticing, because
// sequentially the row is always in the state the caller expects. Only two
// callers arriving at once tell the two implementations apart.
//
// Plant P22: drop the `AND consumed_at_ms IS NULL` from
// store.ConsumeAuthorizationCode. Planted 2026-09-07 by the reviewer; killed
// here.
func TestSlice3ARacedCodeIsExchangedOnce(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:53211/callback")

	const racers = 6
	statuses := make(chan int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := h.exchange(f, nil)
			statuses <- resp.StatusCode
			_ = readBody(t, resp)
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)

	ok, refused := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			refused++
		default:
			t.Errorf("an exchange answered %d", status)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent exchanges of the SAME authorization code succeeded; "+
			"a code is single-use and the conditional UPDATE is what makes that true "+
			"when two callers arrive at once", ok, racers)
	}
	if refused != racers-1 {
		t.Errorf("%d exchanges were refused, want %d", refused, racers-1)
	}

	// And the losers were replays, so the winner's grant is revoked: a code
	// seen twice means the value leaked.
	admin := h.adminToken()
	data := dataOf(t, h.get("/v1/admin/authorizations?include_revoked=true", bearer(admin)))
	items, _ := data["items"].([]any)
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		if row["kind"] != "oauth" {
			continue
		}
		if row["revoked"] != true {
			t.Errorf("after a raced exchange the OAuth grant is still live: %v; a replay "+
				"revokes what the first exchange produced", row)
		}
	}
}

// TestSlice3ARacedRefreshRotatesOnce is the same rule one table over.
//
// Plant P4: remove the in-transaction `if cur.Spent() || cur.Revoked()`
// re-read from service.go's refresh. Planted 2026-09-07 by the reviewer;
// killed here. The read-only lookup before the transaction is not enough on
// its own: both callers pass it, and only the re-read inside the writer's
// transaction can see what the other one did.
func TestSlice3ARacedRefreshRotatesOnce(t *testing.T) {
	h := newOAuthHarness(t)
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	refresh, _ := tokens["refresh_token"].(string)

	const racers = 6
	statuses := make(chan int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := h.postForm("/oauth/token", url.Values{
				"grant_type":    {"refresh_token"},
				"refresh_token": {refresh},
				"client_id":     {f.ClientID},
			})
			statuses <- resp.StatusCode
			_ = readBody(t, resp)
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)

	ok := 0
	for status := range statuses {
		if status == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent refreshes with the SAME token rotated; a refresh "+
			"token is single-use, and the losers are reuse", ok, racers)
	}

	// Reuse revokes the family and the authorization, so nothing that
	// rotation produced survives either.
	admin := h.adminToken()
	data := dataOf(t, h.get("/v1/admin/authorizations?include_revoked=true", bearer(admin)))
	items, _ := data["items"].([]any)
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		if row["kind"] == "oauth" && row["revoked"] != true {
			t.Errorf("after refresh-token reuse the grant is still live: %v", row)
		}
	}
}
