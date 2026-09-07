package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review finding R-10, both halves, driven through `buildServer` -- the same
// constructor `agent-gm serve` binds.
//
// The finding is what a harness cannot see. Both halves were fully
// implemented and fully tested against injected doubles, and the shipped
// binary wrote neither row: the `*.expired` kinds had no writer at all, and
// the supervisor that writes `account.paired`, `account.resumed` and
// `account.signed_out` was constructed by `serve` with a nil Auditor. So
// every assertion here reads `GET /v1/admin/audit` on a real server that has
// really paired, really re-paired, really signed out and really removed.

// auditRows reads the owner's audit listing, filtered.
func (h *oauthHarness) auditRows(admin string, query url.Values) []map[string]any {
	h.t.Helper()
	resp := h.get("/v1/admin/audit?"+query.Encode(), bearer(admin))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("listing audit rows: %d %s", resp.StatusCode, readBody(h.t, resp))
	}
	data := dataOf(h.t, resp)
	raw, _ := data["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// auditOfKind is the listing for one kind.
func (h *oauthHarness) auditOfKind(admin, kind string) []map[string]any {
	h.t.Helper()
	return h.auditRows(admin, url.Values{"kind": {kind}})
}

// payloadOf pulls one row's payload.
func payloadOf(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	p, _ := row["payload"].(map[string]any)
	if p == nil {
		return map[string]any{}
	}
	return p
}

// backdate moves a deadline into the past by writing the column directly,
// with no Agent GM code in the way. Expiry is DERIVED from `expires_at_ms` at
// read time, so moving the column is exactly equivalent to waiting -- and it
// is the only way to make a fifteen-minute deadline pass inside a test that
// must not sleep.
func (h *oauthHarness) backdate(table, column, id string) {
	h.t.Helper()
	db := openRawDB(h.t, filepath.Join(h.dir, "agent-gm.sqlite3"))
	defer func() { _ = db.Close() }()
	res, err := db.Exec(`UPDATE `+table+` SET `+column+` = 1 WHERE id = ?`, id)
	if err != nil {
		h.t.Fatalf("backdating %s.%s: %v", table, column, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		h.t.Fatalf("backdating %s.%s for %s touched %d rows", table, column, id, n)
	}
}

// pendingRequestID drives the authorization screen to a pending request and
// returns its ID, without approving it.
func (h *oauthHarness) pendingRequestID(admin string) string {
	h.t.Helper()
	_, codeValue := h.enrollmentCode(admin, nil)
	redirect := "http://127.0.0.1:18901/callback"
	clientID := h.register(redirect)
	_, challenge := pkce(h.t)
	page := h.authorizePage(authorizeParams{
		ClientID: clientID, RedirectURI: redirect, State: "a-state", Challenge: challenge,
	})
	form := hiddenFields(h.t, page)
	form.Add("scope_selected", "messages:read")
	form.Set("enrollment_code", codeValue)
	resp := h.postForm("/oauth/authorize", form, h.withCookie)
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("submitting the approval form: %d\n%s", resp.StatusCode, readBody(h.t, resp))
	}
	id := strings.TrimPrefix(resp.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(h.t, resp)
	if id == "" {
		h.t.Fatal("no authorization request was created")
	}
	return id
}

// Spec section 12.4 lists "authorization request creation, approval, denial
// and expiry". Creation, approval and denial each have a handler to write
// their row from; expiry does not, because nobody performs it. The row is
// therefore written by the first read that OBSERVES the deadline has passed
// -- and by that read only, however many times the waiting page polls
// afterwards.
func TestAnExpiredAuthorizationRequestIsAuditedOnceOnTheFirstReadThatSeesIt(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	requestID := h.pendingRequestID(admin)

	if rows := h.auditOfKind(admin, "authorization.expired"); len(rows) != 0 {
		t.Fatalf("a pending request already has %d authorization.expired rows", len(rows))
	}

	h.backdate("authorization_requests", "expires_at_ms", requestID)

	// The first derived read. The status the owner is served is `expired`,
	// and the row is written as a side effect of deriving it.
	show := h.get("/v1/admin/authorization-requests/"+requestID, bearer(admin))
	if show.StatusCode != http.StatusOK {
		t.Fatalf("reading the request: %d %s", show.StatusCode, readBody(t, show))
	}
	if status, _ := dataOf(t, show)["status"].(string); status != "expired" {
		t.Fatalf("the request reads as %q, want expired", status)
	}

	rows := h.auditOfKind(admin, "authorization.expired")
	if len(rows) != 1 {
		t.Fatalf("the first read that observed the expiry wrote %d authorization.expired rows, want 1", len(rows))
	}
	row := rows[0]
	if row["account_id"] != nil {
		t.Errorf("authorization.expired is a server-wide kind and must carry a NULL account_id, got %v",
			row["account_id"])
	}
	if got := payloadOf(t, row)["request_id"]; got != requestID {
		t.Errorf("the payload names request_id %v, want %s", got, requestID)
	}
	if tt, _ := row["target_id"].(string); tt != requestID {
		t.Errorf("the row targets %q, want %s", tt, requestID)
	}

	// A second and a third derived read: the waiting page polls every two
	// seconds, and an audit row is never rewritten and never duplicated.
	_ = readBody(t, h.get("/v1/admin/authorization-requests/"+requestID, bearer(admin)))
	_ = readBody(t, h.get("/v1/admin/authorization-requests?status=pending", bearer(admin)))
	if again := h.auditOfKind(admin, "authorization.expired"); len(again) != 1 {
		t.Fatalf("after three derived reads there are %d authorization.expired rows, want exactly 1", len(again))
	}
}

// The enrollment half of the same section 12.4 sentence: "enrollment-code
// creation, consumption, expiry and revocation".
func TestAnExpiredEnrollmentCodeIsAuditedOnceAndNeverCarriesItsValue(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	codeID, codeValue := h.enrollmentCode(admin, nil)

	if rows := h.auditOfKind(admin, "enrollment.expired"); len(rows) != 0 {
		t.Fatalf("a live code already has %d enrollment.expired rows", len(rows))
	}

	h.backdate("enrollment_codes", "expires_at_ms", codeID)

	// The owner's listing is a derived read of the code's expiry.
	_ = readBody(t, h.get("/v1/admin/enrollment-codes", bearer(admin)))

	rows := h.auditOfKind(admin, "enrollment.expired")
	if len(rows) != 1 {
		t.Fatalf("observing the expiry wrote %d enrollment.expired rows, want 1", len(rows))
	}
	row := rows[0]
	if row["account_id"] != nil {
		t.Errorf("enrollment.expired must carry a NULL account_id, got %v", row["account_id"])
	}
	if got := payloadOf(t, row)["enrollment_code_id"]; got != codeID {
		t.Errorf("the payload names enrollment_code_id %v, want %s", got, codeID)
	}

	// Section 12.4: payloads carry IDs, counts and outcomes -- never the code.
	resp := h.get("/v1/admin/audit?kind_prefix=enrollment.", bearer(admin))
	body := readBody(t, resp)
	if strings.Contains(body, codeValue) {
		t.Error("an enrollment audit row carries the code value itself")
	}

	// Reading the listing again observes the same expiry again.
	_ = readBody(t, h.get("/v1/admin/enrollment-codes", bearer(admin)))
	_ = readBody(t, h.get("/v1/admin/enrollment-codes/"+codeID, bearer(admin)))
	if again := h.auditOfKind(admin, "enrollment.expired"); len(again) != 1 {
		t.Fatalf("after three derived reads there are %d enrollment.expired rows, want exactly 1", len(again))
	}
}

// pair drives POST /v1/pairing/start against the fake backend and polls until
// the pairing settles. It never touches a phone: AGENT_GM_BACKEND=fake plus
// AGENT_GM_ALLOW_FAKE=1 is what the harness sets (spec section 13.1).
func (h *oauthHarness) pair(admin, accountID string) string {
	h.t.Helper()
	body := map[string]any{
		// Nonfunctional placeholders. No real cookie value ever enters this
		// repository (spec sections 12.2, 13.4).
		"cookies": map[string]string{
			"SID": "fixture", "HSID": "fixture", "OSID": "fixture",
			"SSID": "fixture", "APISID": "fixture", "SAPISID": "fixture",
			"__Secure-1PSIDTS": "fixture",
		},
	}
	if accountID != "" {
		body["account_id"] = accountID
	}
	resp := h.postJSON("/v1/pairing/start", body, bearer(admin))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("starting a pairing: %d %s", resp.StatusCode, readBody(h.t, resp))
	}
	data := dataOf(h.t, resp)
	pairingID, _ := data["pairing_id"].(string)
	state, _ := data["state"].(string)
	id, _ := data["account_id"].(string)

	deadline := time.Now().Add(20 * time.Second)
	for state == "waiting" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		poll := h.get("/v1/pairing/"+pairingID, bearer(admin))
		if poll.StatusCode != http.StatusOK {
			h.t.Fatalf("polling the pairing: %d %s", poll.StatusCode, readBody(h.t, poll))
		}
		data = dataOf(h.t, poll)
		state, _ = data["state"].(string)
		id, _ = data["account_id"].(string)
	}
	if state != "paired" {
		h.t.Fatalf("the pairing settled as %q, not paired: %v", state, data)
	}
	if id == "" {
		h.t.Fatal("a paired pairing reports no account_id")
	}
	return id
}

// deleteJSON sends a DELETE with a body, which is what the two irreversible
// routes of section 4.7 require.
func (h *oauthHarness) deleteJSON(path string, body any, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodDelete, h.http.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	return h.do(req)
}

// The four account-lifecycle kinds section 12.4 requires, each carrying
// `account_id`, written by the real serve wiring rather than by a supervisor
// a test handed a recorder to.
func TestServePairingWritesTheAccountLifecycleAuditRows(t *testing.T) {
	// A pinned fake address is what makes the SECOND pairing a re-pair onto
	// the same acct_ row rather than a second account (spec section 13.1).
	t.Setenv("AGENT_GM_FAKE_ACCOUNT", "owner@example.test")
	h := newOAuthHarness(t)
	admin := h.adminToken()

	accountID := h.pair(admin, "")
	assertOneAccountRow(t, h, admin, "account.paired", accountID)

	// Section 12.4 words `account.state_changed` as "(from, to,
	// state_reason)", and those have to be READABLE state names: a row that
	// records that something changed and will not say what to is not a
	// diagnosis. `from` and `to` are also the field names a message's
	// endpoints go under, so the redactor had been hashing both.
	var connected bool
	for _, row := range h.auditOfKind(admin, "account.state_changed") {
		if to, _ := payloadOf(t, row)["to"].(string); to == "connected" {
			connected = true
		}
	}
	if !connected {
		t.Errorf("no account.state_changed row reports `to: connected` after a pairing; "+
			"the rows are %v", h.auditOfKind(admin, "account.state_changed"))
	}

	// The same address again: a re-pair, which resumes the acct_ row.
	if resumed := h.pair(admin, accountID); resumed != accountID {
		t.Fatalf("the re-pair produced %s, want the same account %s", resumed, accountID)
	}
	assertOneAccountRow(t, h, admin, "account.resumed", accountID)
	if paired := h.auditOfKind(admin, "account.paired"); len(paired) != 1 {
		t.Errorf("a re-pair wrote %d account.paired rows; a resume is not a second pairing", len(paired))
	}

	// Signing out: the session file is shredded and every row is kept.
	out := h.postJSON("/v1/accounts/"+accountID+"/sign-out", map[string]any{"confirm": true}, bearer(admin))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("signing out: %d %s", out.StatusCode, readBody(t, out))
	}
	_ = readBody(t, out)
	assertOneAccountRow(t, h, admin, "account.signed_out", accountID)

	// Removal, whose row must carry the deleted row counts.
	del := h.deleteJSON("/v1/accounts/"+accountID, map[string]any{"confirm": true}, bearer(admin))
	if del.StatusCode != http.StatusOK {
		t.Fatalf("removing the account: %d %s", del.StatusCode, readBody(t, del))
	}
	_ = readBody(t, del)
	removed := assertOneAccountRow(t, h, admin, "account.removed", accountID)
	payload := payloadOf(t, removed)
	for _, key := range []string{
		"conversations", "participants", "messages", "attachments",
		"reactions", "operations", "contacts",
	} {
		if _, ok := payload[key]; !ok {
			t.Errorf("account.removed carries no %q count; section 12.4 requires the deleted row counts", key)
		}
	}

	// The reviewer's own query: after two pairings and a removal, rows
	// filtered BY account are found, and the trail survives the erasure
	// (sections 12.4, 4.7).
	byAccount := h.auditRows(admin, url.Values{"account_id": {accountID}})
	if len(byAccount) < 4 {
		t.Fatalf("filtering the audit log by account_id found %d rows; the account lifecycle alone is four",
			len(byAccount))
	}
	for _, row := range byAccount {
		if row["account_id"] == nil {
			t.Errorf("a row returned by the account_id filter carries a NULL account_id: %v", row)
		}
	}
}

// assertOneAccountRow checks that exactly one row of the kind exists and that
// it carries the account it belongs to. Section 12.4's rule is a column, not
// a payload key: `--account-id` filters on the column, and a row without it
// is a row that disappears from "what happened to this account" the moment
// the account is removed.
func assertOneAccountRow(t *testing.T, h *oauthHarness, admin, kind, accountID string) map[string]any {
	t.Helper()
	rows := h.auditOfKind(admin, kind)
	if len(rows) != 1 {
		t.Fatalf("serve wrote %d %s rows, want exactly 1", len(rows), kind)
	}
	got, _ := rows[0]["account_id"].(string)
	if got != accountID {
		t.Fatalf("%s carries account_id %q, want %s", kind, got, accountID)
	}
	return rows[0]
}
