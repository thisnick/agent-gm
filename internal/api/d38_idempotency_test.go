package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// D38: the idempotency key is optional, its only transport is the
// `Idempotency-Key` header, and `client_request_id` is gone from every body.
//
// These are the wire-level halves of the decision. The core-level halves --
// that a keyless send is a new operation with one backend call, and that two
// keyless sends are two operations -- live in internal/core, because that is
// where the operation is minted.

// A mutation with NO key at all succeeds and comes back with a server-minted
// operation ID. Before D38 this was `invalid_request`; it is now the ordinary
// case, and the operation ID is the thing that replaces the key, so a
// response without one would leave a caller with nothing to check status by.
func TestD38AMutationWithNoIdempotencyKeyMintsAnOperationID(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")

	sent := s.call("POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"text": "no key at all"}).ok(t, 200)

	op, _ := sent.Data["operation"].(map[string]any)
	if op == nil {
		t.Fatal("a keyless send returned no operation")
	}
	id, _ := op["id"].(string)
	if id == "" {
		t.Fatal("a keyless send returned an operation with no id; there is nothing to check status by")
	}

	// And it is readable by that ID, which is the whole instruction the
	// tool descriptions now give: check status by the operation id.
	s.call("GET", "/v1/operations/"+id, nil).ok(t, 200)

	// A second keyless send is a SECOND operation. Collapsing them would
	// silently drop a real message, which is worse than sending twice.
	again := s.call("POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"text": "no key at all"}).ok(t, 200)
	op2, _ := again.Data["operation"].(map[string]any)
	if op2 == nil {
		t.Fatal("the second keyless send returned no operation")
	}
	if op2["id"] == op["id"] {
		t.Fatal("two keyless sends returned the same operation; the second message was dropped")
	}
}

// `client_request_id` in a body is now an unknown field like any other, on
// every route that used to take it. This is the clause a caller written
// against the old contract meets first, so the refusal has to name the field
// rather than failing somewhere less legible.
func TestD38ClientRequestIDInABodyIsRefusedByName(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")
	msg := s.seedMessage(accountID, "conv-a", "m0001", s.Clock.Now(), false)

	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"send", "POST", "/v1/conversations/" + conv.ID + "/messages",
			map[string]any{"text": "hi", "client_request_id": "k1"}},
		{"start", "POST", "/v1/conversations",
			map[string]any{"recipients": []string{"+12025550123"}, "client_request_id": "k1"}},
		{"mark read", "POST", "/v1/conversations/" + conv.ID + "/read",
			map[string]any{"message_id": msg.ID, "client_request_id": "k1"}},
		{"update", "PATCH", "/v1/conversations/" + conv.ID,
			map[string]any{"folder": "archived", "client_request_id": "k1"}},
		{"react", "POST", "/v1/messages/" + msg.ID + "/reactions",
			map[string]any{"emoji": "👍", "client_request_id": "k1"}},
		{"upload", "POST", "/v1/uploads",
			map[string]any{"filename": "a.jpg", "mime_type": "image/jpeg", "size_bytes": 3,
				"client_request_id": "k1"}},
		{"delete message", "DELETE", "/v1/messages/" + msg.ID,
			map[string]any{"client_request_id": "k1"}},
		{"delete conversation", "DELETE", "/v1/conversations/" + conv.ID,
			map[string]any{"client_request_id": "k1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := s.call(tc.method, tc.path, tc.body)
			if env.Status != http.StatusBadRequest {
				t.Fatalf("%s %s answered %d, want 400 for an unknown body field",
					tc.method, tc.path, env.Status)
			}
			if env.Error == nil || env.Error.Code != "invalid_request" {
				t.Fatalf("the refusal is %v, want invalid_request", env.Error)
			}
			if got := env.Error.Details["field"]; got != "client_request_id" {
				t.Errorf("details.field = %v, want client_request_id so a caller written "+
					"against the old contract can see what to remove", got)
			}
		})
	}
}

// The header still does everything it did: replay on the same key and body,
// conflict on the same key with a different body. D38 removed the body
// transport, not the protection, and a script that retries on a timeout is
// the caller it exists for.
func TestD38TheHeaderStillReplaysAndStillConflicts(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")
	be := s.backend(accountID)

	header := map[string]string{"Idempotency-Key": "k-d38"}
	first := s.callWith(s.Token, "POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"text": "once"}, header).ok(t, 200)
	after := be.CallCount("SendText")

	replay := s.callWith(s.Token, "POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"text": "once"}, header).ok(t, 200)
	firstOp, _ := first.Data["operation"].(map[string]any)
	replayOp, _ := replay.Data["operation"].(map[string]any)
	if firstOp == nil || replayOp == nil || firstOp["id"] != replayOp["id"] {
		t.Fatalf("a replay under the same key returned a different operation: %v vs %v", firstOp, replayOp)
	}
	if got := be.CallCount("SendText"); got != after {
		t.Fatalf("a replay called the backend %d extra times", got-after)
	}

	conflict := s.callWith(s.Token, "POST", "/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"text": "something else"}, header)
	if conflict.Status != http.StatusConflict {
		t.Fatalf("the same key with a different body answered %d, want 409", conflict.Status)
	}
	if conflict.Error == nil || conflict.Error.Code != "idempotency_conflict" {
		t.Fatalf("the refusal is %v, want idempotency_conflict", conflict.Error)
	}
	if got := be.CallCount("SendText"); got != after {
		t.Fatalf("a conflicting body called the backend %d extra times", got-after)
	}
}

// A key that is present and unusable is still refused, and the refusal names
// the header rather than the field that no longer exists. The 200-byte bound
// is what stops the key being used as a smuggling channel into the audit log.
func TestD38AnUnusableHeaderIsRefusedNamingTheHeader(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")

	// Only the length case is exercised over the wire: net/http refuses to
	// PUT a control character in a header value at all, so the transport
	// stops it before the server can. That half is asserted directly against
	// IdempotencyKeyFrom in strict_test.go.
	for name, key := range map[string]string{
		"too long": strings.Repeat("k", 201),
	} {
		env := s.callWith(s.Token, "POST", "/v1/conversations/"+conv.ID+"/messages",
			map[string]any{"text": "hi"}, map[string]string{"Idempotency-Key": key})
		if env.Status != http.StatusBadRequest {
			t.Fatalf("%s answered %d, want 400", name, env.Status)
		}
		if env.Error == nil || env.Error.Details["field"] != "Idempotency-Key" {
			t.Errorf("%s named %v, want the Idempotency-Key header", name, env.Error)
		}
	}
}
