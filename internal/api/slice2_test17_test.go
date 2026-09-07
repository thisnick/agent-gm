package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 17, the big one:
//
//	"The full upload path: reserve, `PUT` the exact bytes, send. **Uploads are
//	 account-agnostic**: an upload reserved with no account in mind sends
//	 successfully into a *second* account's conversation, and a second send of
//	 the same upload -- into either account -- is refused. A short body, a long
//	 body, a wrong `sha256` and a contradicted content type are each refused
//	 **and spend the reservation**. A second `PUT` with the same token fails.
//	 The token presented at another upload's URL is refused **and not spent**,
//	 proven by then redeeming it successfully at its own URL. Two `upload_ids`
//	 in one send is `invalid_request`."
//
// Two of these clauses are the whole design and are easy to get backwards.
//
// **A failed verification spends the reservation.** It would be friendlier
// not to: let the caller retry the `PUT`. But a reservation whose bytes have
// been half-written and rejected is a reservation whose state nobody can
// describe, and the cheap, honest answer is that it is finished -- reserve
// again. So the redemption is counted whether or not the body was acceptable.
//
// **A wrong-audience token is refused and NOT spent.** This is the opposite
// rule, for the opposite reason: if presenting a token at the wrong URL
// consumed it, then anybody who learned a token value could destroy the
// upload it belonged to without ever being able to use it. The audience is
// inside the MAC, so a token verified against the wrong upload's ID fails
// before any row is read -- and the proof is that it still works afterwards
// at its own URL.

// jpegBytes are a real JPEG header, so a reservation declaring `image/jpeg`
// is not refused for contradicting itself.
// htmlBytes sniff as text/html, which contradicts an `image/jpeg`
// reservation at the top level -- and is the shape that matters, because
// serving a stored HTML page from Agent GM's own origin is the hazard
// section 10.1 exists to close.
var htmlBytes = []byte("<!doctype html><html><body>not a photograph at all</body></html>")

var jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0},
	[]byte("...the rest of a small photograph...")...)

// reserve makes a reservation and returns its ID, its URL and its token.
func (s *server) reserve(t *testing.T, name string, size int64, mime, sha string) (id, url, token string) {
	t.Helper()
	body := map[string]any{
		"filename":          "photo.jpg",
		"mime_type":         mime,
		"size_bytes":        size,
	}
	if sha != "" {
		body["sha256"] = sha
	}
	env := s.call("POST", "/v1/uploads", body).ok(t, 201)
	return getString(t, env.Data, "upload_id"),
		getString(t, env.Data, "upload_url"),
		getString(t, env.Data, "token")
}

// put streams bytes at an upload URL with a token, which is the ONLY
// credential that route accepts: the access token is not one.
func (s *server) put(url, token string, data []byte) envelope {
	s.t.Helper()
	return s.callWith(token, "PUT", strings.TrimPrefix(url, publicURL), data, nil)
}

func (s *server) uploadState(t *testing.T, id string) store.Upload {
	t.Helper()
	u, err := s.Store.Upload(context.Background(), id)
	if err != nil {
		t.Fatalf("reading upload %s: %v", id, err)
	}
	return u
}

// TestSlice2_17_UploadsAreAccountAgnosticAndSendOnce is the happy path plus
// the two account clauses.
func TestSlice2_17_UploadsAreAccountAgnosticAndSendOnce(t *testing.T) {
	s := newServer(t)
	accountA := s.addAccount(addressA)
	accountB := s.addAccount(addressB)
	convA := s.seedConversation(accountA, "conv-a")
	convB := s.seedConversation(accountB, "conv-b")

	// Reserved with NO account in mind: an `upl_` row carries no account_id
	// at all, and the account is fixed at send time by the conversation.
	id, url, token := s.reserve(t, "agnostic", int64(len(jpegBytes)), "image/jpeg", media.SHA256(jpegBytes))
	if got := s.uploadState(t, id); got.State != store.UploadReserved {
		t.Fatalf("a fresh reservation is %q, want %q", got.State, store.UploadReserved)
	}

	s.put(url, token, jpegBytes).ok(t, 200)
	if got := s.uploadState(t, id); got.State != store.UploadComplete {
		t.Fatalf("after a good PUT the reservation is %q, want %q", got.State, store.UploadComplete)
	}

	// It sends into the SECOND account's conversation, which it was never
	// associated with.
	sent := s.call("POST", "/v1/conversations/"+convB.ID+"/messages", map[string]any{
		"upload_ids":        []string{id},
		"text":              "the photograph",
	}).ok(t, 200)
	if sent.Data["operation"] == nil {
		t.Fatal("the send produced no operation")
	}
	op, _ := sent.Data["operation"].(map[string]any)
	if op["kind"] != "send_media" {
		t.Errorf("the operation kind is %v, want send_media", op["kind"])
	}
	if op["account_id"] != accountB {
		t.Errorf("the operation belongs to %v, want the second account %s",
			op["account_id"], accountB)
	}
	if got := s.uploadState(t, id); got.State != store.UploadConsumed {
		t.Errorf("after a send the reservation is %q, want %q", got.State, store.UploadConsumed)
	}

	// A second send -- into EITHER account -- is refused. It cannot be fanned
	// out, which is what makes an account-agnostic reservation safe.
	s.call("POST", "/v1/conversations/"+convB.ID+"/messages", map[string]any{
		"upload_ids": []string{id},
	}).refused(t, "invalid_request")
	s.call("POST", "/v1/conversations/"+convA.ID+"/messages", map[string]any{
		"upload_ids": []string{id},
	}).refused(t, "invalid_request")
}

// TestSlice2_17_TwoUploadIDsIsInvalidRequestNamingTheLimit keeps a caller
// from silently sending one of two attachments. An agent that sent two and
// saw one arrive would have no way to tell which.
func TestSlice2_17_TwoUploadIDsIsInvalidRequestNamingTheLimit(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")

	first, firstURL, firstToken := s.reserve(t, "two-a", int64(len(jpegBytes)), "image/jpeg", "")
	second, secondURL, secondToken := s.reserve(t, "two-b", int64(len(jpegBytes)), "image/jpeg", "")
	s.put(firstURL, firstToken, jpegBytes).ok(t, 200)
	s.put(secondURL, secondToken, jpegBytes).ok(t, 200)

	env := s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids":        []string{first, second},
	}).refused(t, "invalid_request")

	if got := env.detail("field"); got != "upload_ids" {
		t.Errorf("details.field is %v, want \"upload_ids\"", got)
	}
	if got, _ := env.detail("limit").(float64); int(got) != 1 {
		t.Errorf("details.limit is %v, want 1; the refusal must name the limit",
			env.detail("limit"))
	}
	// Neither reservation was consumed by a refused send.
	for _, id := range []string{first, second} {
		if got := s.uploadState(t, id); got.State != store.UploadComplete {
			t.Errorf("a refused send changed reservation %s to %q", id, got.State)
		}
	}
}

// TestSlice2_17_EveryFailedVerificationSpendsTheReservation is the four
// refusals, each asserted twice: the request fails, AND the reservation is
// finished.
func TestSlice2_17_EveryFailedVerificationSpendsTheReservation(t *testing.T) {
	cases := []struct {
		name     string
		size     int64
		mime     string
		sha      string
		body     []byte
		wantCode string
	}{
		{
			name: "a short body", size: int64(len(jpegBytes)), mime: "image/jpeg",
			body: jpegBytes[:len(jpegBytes)-5], wantCode: "invalid_request",
		},
		{
			name: "a long body", size: int64(len(jpegBytes)), mime: "image/jpeg",
			body:     append(append([]byte(nil), jpegBytes...), []byte("and more")...),
			wantCode: "payload_too_large",
		},
		{
			name: "a wrong sha256", size: int64(len(jpegBytes)), mime: "image/jpeg",
			sha:  media.SHA256([]byte("some entirely different bytes")),
			body: jpegBytes, wantCode: "invalid_request",
		},
		{
			name: "a contradicted content type", size: int64(len(htmlBytes)), mime: "image/jpeg",
			body: htmlBytes, wantCode: "invalid_request",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			id, url, token := s.reserve(t, "spend", c.size, c.mime, c.sha)

			s.put(url, token, c.body).refused(t, c.wantCode)

			after := s.uploadState(t, id)
			if after.State == store.UploadReserved {
				t.Fatalf("%s left the reservation %q; a reservation that fails "+
					"verification is spent -- reserve again rather than retry",
					c.name, after.State)
			}
			if after.Redemptions != 1 {
				t.Errorf("%s spent %d redemptions, want 1", c.name, after.Redemptions)
			}

			// And the retry the caller might attempt is refused, which is the
			// observable half of "spent".
			s.put(url, token, jpegBytes).refused(t, "invalid_token")
		})
	}
}

// TestSlice2_17_ASecondPUTWithTheSameTokenFails is the single-redemption rule
// on the success path, which is the one that would otherwise let an upload be
// rewritten between the verification and the send.
func TestSlice2_17_ASecondPUTWithTheSameTokenFails(t *testing.T) {
	s := newServer(t)
	id, url, token := s.reserve(t, "twice", int64(len(jpegBytes)), "image/jpeg", "")

	s.put(url, token, jpegBytes).ok(t, 200)
	s.put(url, token, jpegBytes).refused(t, "invalid_token")

	if got := s.uploadState(t, id); got.Redemptions != 1 {
		t.Errorf("the reservation records %d redemptions after two PUTs, want 1", got.Redemptions)
	}
}

// TestSlice2_17_AWrongAudienceTokenIsRefusedAndNotSpent is the clause that
// makes a leaked token value useless to an attacker rather than destructive.
//
// The proof is not that the refusal happened -- a handler that spent the
// reservation and then refused would look identical from the outside. The
// proof is that the token still works at its OWN url afterwards.
func TestSlice2_17_AWrongAudienceTokenIsRefusedAndNotSpent(t *testing.T) {
	s := newServer(t)

	mine, myURL, myToken := s.reserve(t, "audience-mine", int64(len(jpegBytes)), "image/jpeg", "")
	theirs, theirURL, _ := s.reserve(t, "audience-theirs", int64(len(jpegBytes)), "image/jpeg", "")

	// My token, presented at THEIR URL.
	s.put(theirURL, myToken, jpegBytes).refused(t, "invalid_token")

	// Neither reservation moved. The wrong audience never reached a row.
	for label, id := range map[string]string{"mine": mine, "theirs": theirs} {
		got := s.uploadState(t, id)
		if got.State != store.UploadReserved || got.Redemptions != 0 {
			t.Fatalf("presenting a token at the wrong URL spent %s: state %q, %d redemptions",
				label, got.State, got.Redemptions)
		}
	}

	// The proof: it still redeems at its own URL.
	s.put(myURL, myToken, jpegBytes).ok(t, 200)
	if got := s.uploadState(t, mine); got.State != store.UploadComplete {
		t.Errorf("the token no longer worked at its own URL: state %q", got.State)
	}
	// And their reservation is still untouched and still fillable.
	if got := s.uploadState(t, theirs); got.State != store.UploadReserved {
		t.Errorf("the other reservation is %q, want untouched", got.State)
	}
}

// TestSlice2_17_ReservationRefusesAnUnsupportedTypeAndReservesNothing is spec
// section 10.2's validation at reservation time. Validating at send time
// instead would let a caller upload 100 MiB before finding out.
func TestSlice2_17_ReservationRefusesAnUnsupportedTypeAndReservesNothing(t *testing.T) {
	s := newServer(t)

	before := countRows(t, s.Store, "SELECT COUNT(*) FROM uploads")
	env := s.call("POST", "/v1/uploads", map[string]any{
		"filename": "thing.xyz",
		// `font/woff2` is genuinely absent from upstream's table AND its
		// `font` prefix is absent too, so it is refused by the same
		// two-step rule libgm.UploadMedia applies. An `application/x-…`
		// would be ACCEPTED here, because upstream's prefix fallback finds
		// the bare `application` row -- which is exactly why Agent GM asks
		// gm.SupportedMIME rather than looking for an exact match.
		"mime_type":         "font/woff2",
		"size_bytes":        10,
	}).refused(t, "media_unsupported_type")
	if env.Status != 415 {
		t.Errorf("status %d, want 415", env.Status)
	}
	if got := countRows(t, s.Store, "SELECT COUNT(*) FROM uploads"); got != before {
		t.Errorf("an unsupported type reserved %d rows; it must reserve nothing", got-before)
	}
}

// TestSlice2_17_AnotherAuthorizationsUploadIsNotFound keeps an `upl_` ID from
// being probed for existence, and keeps one caller from sending another's
// staged bytes.
func TestSlice2_17_AnotherAuthorizationsUploadIsNotFound(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")

	id, url, token := s.reserve(t, "owned", int64(len(jpegBytes)), "image/jpeg", "")
	s.put(url, token, jpegBytes).ok(t, 200)

	// A second, entirely separate authorization.
	other, err := s.Deps.Authz.MintAdminSession(context.Background(), testAdminSecret, nil, "127.0.0.2")
	if err != nil {
		t.Fatalf("minting a second session: %v", err)
	}

	s.callWith(other.AccessToken, "GET", "/v1/uploads/"+id, nil, nil).refused(t, "not_found")
	s.callWith(other.AccessToken, "POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"upload_ids": []string{id}, }, nil).
		refused(t, "not_found")

	// And the owner can still send it, so the refusal did not consume it.
	s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids": []string{id},
	}).ok(t, 200)
}
