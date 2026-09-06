package apierr

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/uuid"
)

// TestSuccessEnvelopeGolden pins the success envelope of spec section 7.1
// byte for byte, including that next_cursor is null and warnings is an empty
// array rather than null.
func TestSuccessEnvelopeGolden(t *testing.T) {
	env := NewSuccess("req_0199c0f0-0000-7000-8000-000000000001", map[string]any{})

	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"data":{},"next_cursor":null,"warnings":[],"request_id":"req_0199c0f0-0000-7000-8000-000000000001"}`
	if string(got) != want {
		t.Errorf("success envelope\n got: %s\nwant: %s", got, want)
	}
}

// TestSuccessEnvelopeWithCursorAndWarnings proves the two optional fields
// carry what the route put in them and that an empty cursor stays null: ""
// is not a cursor, and a client that pages on it would loop.
func TestSuccessEnvelopeWithCursorAndWarnings(t *testing.T) {
	env := NewSuccess("req_x", nil).
		WithCursor("eyJhIjoxfQ").
		WithWarnings(WarnReasonTruncated, WarnFilenameNormalized)

	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"data":null,"next_cursor":"eyJhIjoxfQ","warnings":["reason_truncated","filename_normalized"],"request_id":"req_x"}`
	if string(got) != want {
		t.Errorf("success envelope\n got: %s\nwant: %s", got, want)
	}

	if c := NewSuccess("req_x", nil).WithCursor("").NextCursor; c != nil {
		t.Errorf("an empty cursor became %q instead of null", *c)
	}
}

// TestErrorEnvelopeGolden pins the error envelope of spec section 7.1 byte
// for byte, including that details is {} and not null when the error carries
// none.
func TestErrorEnvelopeGolden(t *testing.T) {
	env := NotFound("conversation").Envelope("req_0199c0f0-0000-7000-8000-000000000002")

	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"error":{"code":"not_found","message":"No such conversation.","retryable":false,"details":{}},` +
		`"request_id":"req_0199c0f0-0000-7000-8000-000000000002"}`
	if string(got) != want {
		t.Errorf("error envelope\n got: %s\nwant: %s", got, want)
	}
}

// TestErrorEnvelopeCarriesDetailsAndRetryable proves a structured error keeps
// its details and that retryable comes from spec section 7.2's table.
func TestErrorEnvelopeCarriesDetailsAndRetryable(t *testing.T) {
	env := UnknownQueryParameter("_").Envelope("req_y")
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", env.Error.Code)
	}
	if env.Error.Retryable {
		t.Error("invalid_request must not be retryable")
	}
	if env.Error.Details["parameter"] != "_" {
		t.Errorf("details.parameter = %v, want \"_\"", env.Error.Details["parameter"])
	}

	if !PhoneNotResponding().Envelope("req_y").Error.Retryable {
		t.Error("phone_not_responding must be retryable in the envelope")
	}
}

// requestIDPattern is req_ followed by a UUID whose version nibble is 7.
var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewRequestIDIsAUUIDv7 proves the request ID has the section 4.1 prefix
// and is genuinely a v7, so request IDs sort by time in a log.
func TestNewRequestIDIsAUUIDv7(t *testing.T) {
	seen := make(map[string]bool, 64)
	var previous string
	for i := 0; i < 64; i++ {
		id := NewRequestID()
		if !requestIDPattern.MatchString(id) {
			t.Fatalf("request ID %q is not a req_ UUIDv7", id)
		}
		parsed, err := uuid.Parse(id[len(RequestIDPrefix):])
		if err != nil {
			t.Fatalf("parse %q: %v", id, err)
		}
		if parsed.Version() != 7 {
			t.Fatalf("request ID %q is UUID version %d, want 7", id, parsed.Version())
		}
		if seen[id] {
			t.Fatalf("request ID %q was minted twice", id)
		}
		seen[id] = true
		if previous != "" && id < previous {
			t.Fatalf("request IDs must sort by time: %q came after %q", id, previous)
		}
		previous = id
	}
}
