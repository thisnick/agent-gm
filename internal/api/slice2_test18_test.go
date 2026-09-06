package api_test

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"

	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 18:
//
//	"A download ticket redeems 5 times and the 6th fails, proven against the
//	 `download_tickets.redemptions` counter and across a process restart. Each
//	 redemption re-checks the issuing authorization: narrowing that session's
//	 scopes to drop `messages:read` makes the next redemption fail."
//
// Both halves are about a rule a signed blob cannot enforce.
//
// **A five-use cap cannot live in the token** (D24). A JWT carrying
// `"uses": 5` is a JWT that can be presented five hundred times, because
// nothing decrements it. So the ticket is a ROW, the counter is a column, and
// the increment happens in the same transaction that authorises the read --
// which is also why the cap survives a restart, and why this test proves it
// across one rather than within a single process's memory.
//
// **A revocation cannot live in the token either.** A signed blob cannot know
// it has been revoked since it was signed, so every redemption re-reads the
// issuing authorization. Narrowing a session to drop `messages:read` must
// kill every download ticket it minted, immediately -- otherwise "revoke this
// agent's access" would mean "revoke it in fifteen minutes' time".

// mintTicket fetches an attachment's metadata, which is what mints a ticket.
func (s *server) mintTicket(t *testing.T, attachmentID string) (token, url string) {
	t.Helper()
	env := s.call("GET", "/v1/attachments/"+attachmentID, nil).ok(t, 200)
	if got, _ := env.Data["max_redemptions"].(float64); int(got) != 5 {
		t.Fatalf("max_redemptions is %v, want 5", env.Data["max_redemptions"])
	}
	if want := media.Audience(media.KindDownload, attachmentID); env.Data["token_audience"] != want {
		t.Errorf("token_audience is %v, want %q", env.Data["token_audience"], want)
	}
	return getString(t, env.Data, "token"), "/v1/attachments/" + attachmentID + "/content"
}

func (s *server) ticketRow(t *testing.T, token string) store.DownloadTicket {
	t.Helper()
	row, err := s.Store.DownloadTicketByHash(context.Background(), media.HashToken(token))
	if err != nil {
		t.Fatalf("reading the ticket row: %v", err)
	}
	return row
}

func TestSlice2_18_ATicketRedeemsFiveTimesAndTheSixthFailsAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	clkA := clock.NewFake()

	first := openServer(t, dir, clkA)
	accountID := first.addAccount(addressA)
	first.seedConversation(accountID, "conv-a")
	att := first.seedAttachment(accountID, "conv-a", "m-att", []byte("the photograph's bytes"), "image/jpeg")

	token, url := first.mintTicket(t, att.ID)

	// Three redemptions before the restart.
	for i := 1; i <= 3; i++ {
		body := first.callWith(token, "GET", url, nil, nil)
		if body.Status != 200 {
			t.Fatalf("redemption %d failed: %d %s", i, body.Status, truncate(body.Raw))
		}
		if got := first.ticketRow(t, token).Redemptions; got != i {
			t.Fatalf("download_tickets.redemptions is %d after %d redemptions", got, i)
		}
	}

	// The restart. The counter is a row, so it is still three.
	first.close()
	second := openServer(t, dir, clkA)

	if got := second.ticketRow(t, token).Redemptions; got != 3 {
		t.Fatalf("download_tickets.redemptions is %d after a restart, want 3; the cap "+
			"is a row precisely so it survives one", got)
	}

	// Two more, which reach the cap.
	for i := 4; i <= 5; i++ {
		body := second.callWith(token, "GET", url, nil, nil)
		if body.Status != 200 {
			t.Fatalf("redemption %d failed after the restart: %d %s", i, body.Status, truncate(body.Raw))
		}
		if got := second.ticketRow(t, token).Redemptions; got != i {
			t.Fatalf("download_tickets.redemptions is %d after %d redemptions", got, i)
		}
	}

	// The sixth fails, and does not increment past the cap.
	second.callWith(token, "GET", url, nil, nil).refused(t, "invalid_token")
	if got := second.ticketRow(t, token).Redemptions; got != 5 {
		t.Errorf("the refused sixth redemption moved the counter to %d, want 5", got)
	}

	// A messages:read access token still works: the cap is on the TICKET, not
	// on the attachment.
	body := second.callWith(second.Token, "GET", url, nil, nil)
	if body.Status != 200 {
		t.Errorf("an access token was refused after the ticket was spent: %d %s",
			body.Status, truncate(body.Raw))
	}
}

// TestSlice2_18_NarrowingTheIssuingSessionKillsItsTickets is the re-check.
//
// The narrowing is done with `POST /v1/auth/refresh`, because that is the
// operation that narrows an EXISTING authorization (spec section 9.6):
// `POST /v1/auth/admin-session` mints a new one, and a new authorization
// leaves the old one -- and its tickets -- exactly as they were, which would
// make this test pass without proving anything.
func TestSlice2_18_NarrowingTheIssuingSessionKillsItsTickets(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	att := s.seedAttachment(accountID, "conv-a", "m-att", []byte("the photograph's bytes"), "image/jpeg")

	token, url := s.mintTicket(t, att.ID)

	// It works while the issuing session holds messages:read.
	if body := s.callWith(token, "GET", url, nil, nil); body.Status != 200 {
		t.Fatalf("the fresh ticket was refused: %d %s", body.Status, truncate(body.Raw))
	}
	if got := s.ticketRow(t, token).Redemptions; got != 1 {
		t.Fatalf("redemptions is %d, want 1", got)
	}

	// Narrow the SAME authorization to drop messages:read.
	narrowed := s.callWith("", "POST", "/v1/auth/refresh", map[string]any{
		"refresh_token": s.Refresh,
		"scopes":        []string{"admin", "messages:write", "messages:delete"},
	}, nil).ok(t, 200)
	if narrowed.Data["authorization_id"] != s.AuthorizationID {
		t.Fatalf("the refresh produced a different authorization (%v, want %s); "+
			"then it is not the issuing session that was narrowed",
			narrowed.Data["authorization_id"], s.AuthorizationID)
	}

	// The next redemption fails, and does not spend a use: a ticket whose
	// authorization can no longer read is not a ticket that has been used.
	s.callWith(token, "GET", url, nil, nil).refused(t, "invalid_token")
	if got := s.ticketRow(t, token).Redemptions; got != 1 {
		t.Errorf("a redemption refused for scope moved the counter to %d, want 1", got)
	}
}

// TestSlice2_18_RevokingTheIssuingAuthorizationKillsItsTickets is the other
// half of the same rule: **a ticket outlives neither its scope nor its
// revocation** (spec section 10.3).
func TestSlice2_18_RevokingTheIssuingAuthorizationKillsItsTickets(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	att := s.seedAttachment(accountID, "conv-a", "m-att", []byte("the photograph's bytes"), "image/jpeg")

	token, url := s.mintTicket(t, att.ID)
	if body := s.callWith(token, "GET", url, nil, nil); body.Status != 200 {
		t.Fatalf("the fresh ticket was refused: %d", body.Status)
	}

	// `POST /v1/auth/logout` ends the TOKEN's session -- and with it every
	// ticket that session minted.
	s.call("POST", "/v1/auth/logout", nil).ok(t, 200)

	s.callWith(token, "GET", url, nil, nil).refused(t, "invalid_token")
}

// TestSlice2_18_ATicketIsRefusedAtAnotherAttachmentAndNotSpent is the
// download side of the audience rule, and the mirror of test 17's upload
// case: learning a ticket value must not let anybody destroy it.
func TestSlice2_18_ATicketIsRefusedAtAnotherAttachmentAndNotSpent(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	mine := s.seedAttachment(accountID, "conv-a", "m-att-1", []byte("mine"), "image/jpeg")
	theirs := s.seedAttachment(accountID, "conv-a", "m-att-2", []byte("theirs"), "image/jpeg")

	token, url := s.mintTicket(t, mine.ID)

	s.callWith(token, "GET", "/v1/attachments/"+theirs.ID+"/content", nil, nil).
		refused(t, "invalid_token")
	if got := s.ticketRow(t, token).Redemptions; got != 0 {
		t.Fatalf("presenting a ticket at another attachment spent it: %d redemptions", got)
	}

	// The proof: it still works at its own URL.
	if body := s.callWith(token, "GET", url, nil, nil); body.Status != 200 {
		t.Errorf("the ticket no longer worked at its own attachment: %d", body.Status)
	}
}

// TestSlice2_18_ContentIsServedSafely covers the three headers and the
// octet-stream rule of spec section 10.1.
//
// **SVG, HTML and every unrecognised type are served as
// `application/octet-stream`, never with their own type.** A message from
// anybody can carry an attachment, and its MIME type is the sender's to
// choose, so repeating it would be a stored cross-site scripting hole on
// Agent GM's own origin -- delivered by a route the owner's agent is expected
// to fetch.
func TestSlice2_18_ContentIsServedSafely(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")

	cases := []struct {
		name, declared, want string
	}{
		{"a photograph keeps its type", "image/jpeg", "image/jpeg"},
		{"an SVG is neutralised", "image/svg+xml", media.OctetStream},
		{"HTML is neutralised", "text/html", media.OctetStream},
		{"XHTML is neutralised", "application/xhtml+xml", media.OctetStream},
		{"an unrecognised type is neutralised", "application/x-something", media.OctetStream},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			att := s.seedAttachment(accountID, "conv-a",
				"m-safe-"+string(rune('a'+i)), htmlBytes, c.declared)
			body := s.callWith(s.Token, "GET", "/v1/attachments/"+att.ID+"/content", nil, nil)
			if body.Status != 200 {
				t.Fatalf("status %d: %s", body.Status, truncate(body.Raw))
			}
			if got := body.Headers.Get("Content-Type"); got != c.want {
				t.Errorf("Content-Type is %q, want %q", got, c.want)
			}
			if got := body.Headers.Get("Content-Disposition"); got == "" ||
				got[:len(media.ContentDisposition)] != media.ContentDisposition {
				t.Errorf("Content-Disposition is %q, want it to begin with %q",
					got, media.ContentDisposition)
			}
			if got := body.Headers.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options is %q, want nosniff", got)
			}
			if got := body.Headers.Get("Content-Security-Policy"); got == "" {
				t.Error("no sandboxing Content-Security-Policy")
			}
			if string(body.Raw) != string(htmlBytes) {
				t.Errorf("the served bytes differ from the stored ones")
			}
		})
	}
}

// TestSlice2_18_SHA256IsOfTheDecryptedBytes is spec section 10.1's digest
// rule. Google carries a digest of the CIPHERTEXT, which is not what an agent
// comparing its downloaded copy would compute -- so serving Google's value
// would look like an answer and never match.
func TestSlice2_18_SHA256IsOfTheDecryptedBytes(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")

	plaintext := []byte("exactly these decrypted bytes")
	att := s.seedAttachment(accountID, "conv-a", "m-att", plaintext, "image/jpeg")

	env := s.call("GET", "/v1/attachments/"+att.ID, nil).ok(t, 200)
	if env.Data["sha256_available"] != true {
		t.Fatalf("sha256_available is %v, want true: %s", env.Data["sha256_available"], truncate(env.Raw))
	}
	if got := getString(t, env.Data, "sha256"); got != media.SHA256(plaintext) {
		t.Errorf("sha256 is %s, want the digest of the DECRYPTED bytes %s",
			got, media.SHA256(plaintext))
	}
	if _, present := env.Data["sha256_unavailable_reason"]; present {
		t.Errorf("sha256_unavailable_reason is present on a successful digest: %v",
			env.Data["sha256_unavailable_reason"])
	}

	// It is cached on the row, so the second read does not re-download.
	row, err := s.Store.Attachment(context.Background(), att.ID)
	if err != nil || row.SHA256 != media.SHA256(plaintext) {
		t.Errorf("the digest was not cached on the attachment row: %q (%v)", row.SHA256, err)
	}
}

// TestSlice2_18_SHA256IsNullWithANamedReasonWhenItCannotBeComputed is the
// other branch. A null with a reason is honest; a wrong number is not, and a
// missing field would leave a caller unable to tell "no digest" from "the
// server forgot".
func TestSlice2_18_SHA256IsNullWithANamedReasonWhenItCannotBeComputed(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	att := s.seedAttachment(accountID, "conv-a", "m-att", []byte("bytes"), "image/jpeg")

	// The bytes are not here: the attachment is still downloading.
	if err := s.Store.SetAttachmentDownloadState(context.Background(),
		att.ID, "pending", ""); err != nil {
		t.Fatalf("marking the attachment pending: %v", err)
	}

	env := s.call("GET", "/v1/attachments/"+att.ID, nil).ok(t, 200)
	if env.Data["sha256"] != nil {
		t.Errorf("sha256 is %v, want null", env.Data["sha256"])
	}
	if env.Data["sha256_available"] != false {
		t.Errorf("sha256_available is %v, want false", env.Data["sha256_available"])
	}
	reason, _ := env.Data["sha256_unavailable_reason"].(string)
	if !media.ValidSHA256UnavailableReason(reason) {
		t.Errorf("sha256_unavailable_reason is %q, want one of %v",
			reason, media.SHA256UnavailableReasons())
	}
	if reason != media.ReasonBytesUnavailable {
		t.Errorf("sha256_unavailable_reason is %q, want %q for an attachment whose "+
			"bytes are not downloaded", reason, media.ReasonBytesUnavailable)
	}
	// The reason also appears in warnings, so a caller reading only the
	// envelope still learns the field is absent for a reason.
	want := media.SHA256UnavailableWarning(reason)
	if !contains(env.Warnings, want) {
		t.Errorf("warnings are %v, want them to carry %q", env.Warnings, want)
	}

	// And the bytes route refuses with media_pending rather than serving an
	// empty body that looks like a zero-length file.
	body := s.callWith(s.Token, "GET", "/v1/attachments/"+att.ID+"/content", nil, nil).
		refused(t, "unsupported_capability")
	if got := body.detail("reason"); got != "media_pending" {
		t.Errorf("details.reason is %v, want \"media_pending\"", got)
	}
}
