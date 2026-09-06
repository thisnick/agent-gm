package apierr

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
)

// TestDecodeQueryRejectsEverythingItDoesNotDefine is spec section 7.1 and
// Slice 2 test 12: nothing is allowlisted, including the cache-busting `_`
// and the `client_request_id` that has exactly two transports, neither of
// them a query parameter (spec section 7.1).
func TestDecodeQueryRejectsEverythingItDoesNotDefine(t *testing.T) {
	allowed := NewAllowedQuery("account_id", "direction", "limit", "cursor")

	t.Run("every allowed parameter passes", func(t *testing.T) {
		q := url.Values{
			"account_id": {"acct_1"},
			"direction":  {"incoming"},
			"limit":      {"50"},
			"cursor":     {"abc"},
		}
		if e := DecodeQuery(q, allowed); e != nil {
			t.Fatalf("a well-formed query was refused: %v", e)
		}
	})

	refused := []string{
		"_",                 // the cache buster the spec names explicitly
		"directon",          // the misspelling the rule exists for
		"client_request_id", // has two transports, neither of them a query
		"Direction",         // parameters are case sensitive
		"limit ",            // trailing space is a different name
		"account_id[]",      // array syntax is not a parameter this API has
		"Idempotency-Key",   // a header is not a query parameter
		"__proto__",         //
		"format",            //
	}
	for _, name := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			q := url.Values{"account_id": {"acct_1"}, name: {"1"}}
			e := DecodeQuery(q, allowed)
			if e == nil {
				t.Fatalf("query parameter %q was silently accepted", name)
			}
			if e.Code != CodeInvalidRequest {
				t.Fatalf("code = %q, want invalid_request", e.Code)
			}
			if e.Details["parameter"] != name {
				t.Errorf("details.parameter = %v, want %q", e.Details["parameter"], name)
			}
		})
	}
}

// TestDecodeQueryIsDeterministicWithSeveralUnknowns proves the same request
// always names the same parameter, whatever order the map iterates in.
func TestDecodeQueryIsDeterministicWithSeveralUnknowns(t *testing.T) {
	q := url.Values{"zeta": {"1"}, "_": {"1"}, "alpha": {"1"}}
	for i := 0; i < 50; i++ {
		e := DecodeQuery(q, NewAllowedQuery())
		if e == nil {
			t.Fatal("three unknown parameters were accepted")
		}
		if e.Details["parameter"] != "_" {
			t.Fatalf("named %v; the first in sorted order is \"_\"", e.Details["parameter"])
		}
	}
}

type sendBody struct {
	Text  string `json:"text"`
	Limit int    `json:"limit"`
}

// TestDecodeBodyRejectsUnknownFields is spec section 7.1: the body decoder
// uses DisallowUnknownFields and turns the result into invalid_request naming
// the field.
func TestDecodeBodyRejectsUnknownFields(t *testing.T) {
	for _, field := range []string{"scope", "action", "_", "client_request_ids"} {
		body := `{"text":"hi","` + field + `":1}`
		var dst sendBody
		e := DecodeBody(strings.NewReader(body), &dst)
		if e == nil {
			t.Fatalf("body field %q was silently accepted", field)
		}
		if e.Code != CodeInvalidRequest {
			t.Fatalf("%q: code = %q, want invalid_request", field, e.Code)
		}
		if e.Details["field"] != field {
			t.Errorf("%q: details.field = %v", field, e.Details["field"])
		}
	}
}

// TestDecodeBodyAcceptsAKnownBody proves the decoder is not simply always
// refusing, and that an empty body is allowed for the DELETE routes of spec
// section 7.7 that carry an optional body.
func TestDecodeBodyAcceptsAKnownBody(t *testing.T) {
	var dst sendBody
	if e := DecodeBody(strings.NewReader(`{"text":"hi","limit":3}`), &dst); e != nil {
		t.Fatalf("a well-formed body was refused: %v", e)
	}
	if dst.Text != "hi" || dst.Limit != 3 {
		t.Errorf("decoded %#v", dst)
	}

	var empty sendBody
	if e := DecodeBody(strings.NewReader(""), &empty); e != nil {
		t.Fatalf("an empty body was refused: %v", e)
	}
	if e := DecodeBody(strings.NewReader("  \n"), &empty); e != nil {
		t.Fatalf("a whitespace-only body was refused: %v", e)
	}
}

// TestDecodeBodySizeRule is spec section 7.2's exact rule: a body over 1 MiB
// is 413 payload_too_large; a body that fails to read for any other reason is
// 400, because "too large" would be a guess.
func TestDecodeBodySizeRule(t *testing.T) {
	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		// {"text":"aaa..."} padded to exactly MaxBodyBytes.
		prefix := `{"text":"`
		suffix := `"}`
		pad := int(MaxBodyBytes) - len(prefix) - len(suffix)
		body := prefix + strings.Repeat("a", pad) + suffix
		if int64(len(body)) != MaxBodyBytes {
			t.Fatalf("test body is %d bytes, want %d", len(body), MaxBodyBytes)
		}
		var dst sendBody
		if e := DecodeBody(strings.NewReader(body), &dst); e != nil {
			t.Fatalf("a body exactly at the limit was refused: %v", e)
		}
	})

	t.Run("one byte over the limit is 413", func(t *testing.T) {
		body := strings.Repeat("a", int(MaxBodyBytes)+1)
		var dst sendBody
		e := DecodeBody(strings.NewReader(body), &dst)
		if e == nil {
			t.Fatal("an oversized body was accepted")
		}
		if e.Code != CodePayloadTooLarge {
			t.Fatalf("code = %q, want payload_too_large", e.Code)
		}
		if e.HTTPStatus() != 413 {
			t.Errorf("status = %d, want 413", e.HTTPStatus())
		}
	})

	t.Run("a read failure is 400, not 413", func(t *testing.T) {
		var dst sendBody
		e := DecodeBody(errReader{}, &dst)
		if e == nil {
			t.Fatal("a failed read was accepted")
		}
		if e.Code == CodePayloadTooLarge {
			t.Fatal(`a read failure answered payload_too_large; "too large" would be a guess`)
		}
		if e.Code != CodeInvalidRequest || e.HTTPStatus() != 400 {
			t.Fatalf("code %q status %d, want invalid_request 400", e.Code, e.HTTPStatus())
		}
	})

	t.Run("a truncated oversized read is still 413", func(t *testing.T) {
		// The reader fails, but only after more than the limit has arrived:
		// the size is then a fact, not a guess.
		var dst sendBody
		r := io.MultiReader(strings.NewReader(strings.Repeat("a", int(MaxBodyBytes)+1)), errReader{})
		e := DecodeBody(r, &dst)
		if e == nil || e.Code != CodePayloadTooLarge {
			t.Fatalf("got %v, want payload_too_large", e)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestDecodeBodyMalformedJSONIs400 covers the other 400 cases: bad syntax, a
// truncated value, a wrong type and a second JSON value.
func TestDecodeBodyMalformedJSONIs400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `<xml/>`},
		{"truncated", `{"text":"hi"`},
		{"trailing value", `{"text":"hi"}{"text":"again"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dst sendBody
			e := DecodeBody(strings.NewReader(tc.body), &dst)
			if e == nil {
				t.Fatal("accepted")
			}
			if e.Code != CodeInvalidRequest || e.HTTPStatus() != 400 {
				t.Fatalf("code %q status %d, want invalid_request 400", e.Code, e.HTTPStatus())
			}
		})
	}

	t.Run("wrong type names the field", func(t *testing.T) {
		var dst sendBody
		e := DecodeBody(strings.NewReader(`{"limit":"fifty"}`), &dst)
		if e == nil || e.Code != CodeInvalidRequest {
			t.Fatalf("got %v, want invalid_request", e)
		}
		if e.Details["field"] != "limit" {
			t.Errorf("details.field = %v, want limit", e.Details["field"])
		}
	})
}

// TestUnknownFieldMessageMatchesTheRealDecoder pins the one string this
// package parses out of encoding/json against what the decoder actually
// produces, so a Go release that reworded it fails here rather than turning
// every unknown field into a generic 400 with no field name.
func TestUnknownFieldMessageMatchesTheRealDecoder(t *testing.T) {
	var dst sendBody
	dec := newDecoderDisallowingUnknown(`{"surprise":1}`)
	err := dec.Decode(&dst)
	if err == nil {
		t.Fatal("the decoder accepted an unknown field")
	}
	if !strings.HasPrefix(err.Error(), unknownFieldPrefix) {
		t.Fatalf("encoding/json now says %q; unknownFieldPrefix is %q", err.Error(), unknownFieldPrefix)
	}
	if got := decodeError(err); got.Details["field"] != "surprise" {
		t.Errorf("decodeError named %v, want surprise", got.Details["field"])
	}
}

// TestDecodeBodyBytesMatchesDecodeBody proves the in-memory variant the
// idempotency layer needs answers identically.
func TestDecodeBodyBytesMatchesDecodeBody(t *testing.T) {
	raw := []byte(`{"text":"hi","surprise":1}`)

	var a, b sendBody
	ea := DecodeBody(bytes.NewReader(raw), &a)
	eb := DecodeBodyBytes(raw, &b)
	if ea == nil || eb == nil {
		t.Fatalf("one variant accepted the body: %v / %v", ea, eb)
	}
	if ea.Code != eb.Code || ea.Details["field"] != eb.Details["field"] {
		t.Errorf("the two variants disagree: %#v vs %#v", ea, eb)
	}
}

// newDecoderDisallowingUnknown builds the same decoder DecodeBodyBytes uses,
// so the test above interrogates the real encoding/json behaviour.
func newDecoderDisallowingUnknown(body string) *json.Decoder {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	return dec
}
