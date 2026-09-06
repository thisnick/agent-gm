package api_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
)

// Section 16 Slice 2 test 12:
//
//	"Every /v1 route rejects an unknown query parameter and an unknown body
//	 field with invalid_request naming it, including ?_=1; route by route,
//	 not a sample."
//
// Route by route is the load-bearing phrase, and it is why the allowed set
// lives in the inventory rather than inside each handler: a handler that read
// its parameters ad hoc would have an allowed set only its own code knew, and
// this test could then only sample.
//
// Plant: give Route.AllowsQuery a special case for "_" -- the shape of an
// innocent-looking cache-busting allowance -- and the `_` subtest fails on
// every route at once. Planted 2026-09-06.
func TestEveryRouteRejectsAnUnknownQueryParameter(t *testing.T) {
	// The cache-busting parameter is named explicitly because it is the one
	// a well-meaning implementation allowlists (spec section 7.1 says so),
	// and the others are the shapes a caller actually mistypes.
	unknowns := []string{"_", "_t", "directon", "Limit", "account", "client_request_id"}

	for _, r := range api.Routes {
		if !strings.HasPrefix(r.Path, "/v1/") {
			continue // /healthz is not a /v1 route
		}
		t.Run(r.Name, func(t *testing.T) {
			// Every parameter the route DOES take is accepted, so the test
			// proves rejection rather than merely proving refusal of
			// everything.
			allowed := url.Values{}
			for _, q := range r.Query {
				allowed.Set(q, "x")
			}
			if err := r.CheckQuery(allowed); err != nil {
				t.Fatalf("%s refused its own parameters: %v", r, err)
			}

			for _, name := range unknowns {
				if r.AllowsQuery(name) {
					t.Fatalf("%s allowlists %q, which spec section 7.1 forbids", r, name)
				}
				values := url.Values{name: []string{"1"}}
				err := r.CheckQuery(values)
				if err == nil {
					t.Errorf("%s accepted the unknown query parameter %q", r, name)
					continue
				}
				if err.Code != apierr.CodeInvalidRequest {
					t.Errorf("%s gave code %q for %q, want invalid_request", r, err.Code, name)
				}
				if err.Details["parameter"] != name {
					t.Errorf("%s did not name %q in details.parameter: %v", r, name, err.Details)
				}
				// Naming it in the message too, because a human reads the
				// message and a program reads the details.
				if !strings.Contains(err.Message, name) {
					t.Errorf("%s did not name %q in the message: %q", r, name, err.Message)
				}
			}

			// An unknown parameter alongside valid ones is still refused: a
			// misspelled filter must not be masked by a correct one.
			mixed := url.Values{}
			for _, q := range r.Query {
				mixed.Set(q, "x")
			}
			mixed.Set("_", "1")
			if err := r.CheckQuery(mixed); err == nil {
				t.Errorf("%s accepted ?_=1 alongside its own parameters", r)
			}
		})
	}
}

func TestEveryRouteRejectsAnUnknownBodyField(t *testing.T) {
	unknowns := []string{"_", "action", "scope", "confirm_all", "clientRequestId", "for_everyone"}

	for _, r := range api.Routes {
		if !strings.HasPrefix(r.Path, "/v1/") {
			continue
		}
		t.Run(r.Name, func(t *testing.T) {
			// The body fields the route does take are accepted.
			own := map[string]any{}
			for _, f := range r.Body {
				own[f] = "x"
			}
			raw, err := json.Marshal(own)
			if err != nil {
				t.Fatal(err)
			}
			if e := r.CheckBodyFields(raw); e != nil {
				t.Fatalf("%s refused its own body fields: %v", r, e)
			}
			// An empty body and no body at all are the same request.
			if e := r.CheckBodyFields(nil); e != nil {
				t.Errorf("%s refused an absent body: %v", r, e)
			}
			if e := r.CheckBodyFields([]byte(`{}`)); e != nil {
				t.Errorf("%s refused an empty body: %v", r, e)
			}
			if e := r.CheckBodyFields([]byte(`null`)); e != nil {
				t.Errorf("%s refused a null body: %v", r, e)
			}

			for _, name := range unknowns {
				if r.AllowsBody(name) {
					t.Fatalf("%s accepts the body field %q", r, name)
				}
				body := map[string]any{name: 1}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				e := r.CheckBodyFields(raw)
				if e == nil {
					t.Errorf("%s accepted the unknown body field %q", r, name)
					continue
				}
				if e.Code != apierr.CodeInvalidRequest {
					t.Errorf("%s gave code %q for %q, want invalid_request", r, e.Code, name)
				}
				if e.Details["field"] != name {
					t.Errorf("%s did not name %q in details.field: %v", r, name, e.Details)
				}
			}
		})
	}
}

// The refusal must be deterministic: two unknown parameters in one query
// always name the same one, or the error a caller sees depends on Go's map
// iteration and any test of it is flaky.
func TestTheRefusalNamesTheSameParameterEveryTime(t *testing.T) {
	r, ok := api.RouteByName("conversations_list")
	if !ok {
		t.Fatal("conversations_list is missing")
	}
	values := url.Values{"zzz": []string{"1"}, "aaa": []string{"1"}, "mmm": []string{"1"}}
	first := r.CheckQuery(values)
	if first == nil {
		t.Fatal("three unknown parameters were accepted")
	}
	for i := 0; i < 50; i++ {
		got := r.CheckQuery(values)
		if got == nil || got.Details["parameter"] != first.Details["parameter"] {
			t.Fatalf("the refusal named %v then %v", first.Details["parameter"], got.Details["parameter"])
		}
	}
	if first.Details["parameter"] != "aaa" {
		t.Errorf("the refusal named %v; sorted order names aaa", first.Details["parameter"])
	}
}

// A body that is not a JSON object is malformed, which is a different answer
// from "unknown field": the caller has to fix something else.
func TestANonObjectBodyIsMalformedNotAnUnknownField(t *testing.T) {
	r, _ := api.RouteByName("messages_send")
	for _, body := range []string{`[1,2]`, `"text"`, `42`, `{`} {
		e := r.CheckBodyFields([]byte(body))
		if e == nil {
			t.Errorf("%q was accepted as a body", body)
			continue
		}
		if e.Code != apierr.CodeInvalidRequest {
			t.Errorf("%q gave code %q", body, e.Code)
		}
		if _, named := e.Details["field"]; named {
			t.Errorf("%q was reported as an unknown field: %v", body, e.Details)
		}
	}
}

// Section 16 Slice 2 test 7's third clause, and the reason it belongs here:
// "a key supplied as a query parameter is invalid_request". It is true by
// construction -- no route lists client_request_id in Query -- and this
// asserts it route by route rather than trusting the construction.
func TestTheIdempotencyKeyIsNeverAQueryParameter(t *testing.T) {
	for _, r := range api.Routes {
		if !strings.HasPrefix(r.Path, "/v1/") {
			continue
		}
		for _, name := range []string{"client_request_id", "idempotency_key", "Idempotency-Key"} {
			e := r.CheckQuery(url.Values{name: []string{"k1"}})
			if e == nil {
				t.Errorf("%s accepted %q as a query parameter", r, name)
				continue
			}
			if e.Code != apierr.CodeInvalidRequest {
				t.Errorf("%s gave code %q for %q as a query parameter", r, e.Code, name)
			}
		}
	}
}

// The key's two transports, and the contradiction between them (spec section
// 6.3). Supplying both with different values is refused rather than resolved
// by a precedence rule: a server that picked one would be guessing which of
// two things the caller meant, and guessing wrong sends a second real text
// message to a real person.
func TestIdempotencyKeyTransports(t *testing.T) {
	t.Run("the header alone", func(t *testing.T) {
		got, err := api.IdempotencyKeyFrom("k1", "")
		if err != nil || got != "k1" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("the body field alone", func(t *testing.T) {
		got, err := api.IdempotencyKeyFrom("", "k1")
		if err != nil || got != "k1" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("both with the same value is not a contradiction", func(t *testing.T) {
		got, err := api.IdempotencyKeyFrom("k1", "k1")
		if err != nil || got != "k1" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("both with different values is refused", func(t *testing.T) {
		_, err := api.IdempotencyKeyFrom("k1", "k2")
		if err == nil {
			t.Fatal("a contradiction was resolved rather than refused")
		}
		if err.Code != apierr.CodeInvalidRequest || err.Details["field"] != "client_request_id" {
			t.Errorf("got %q / %v", err.Code, err.Details)
		}
	})
	t.Run("neither is refused, naming client_request_id", func(t *testing.T) {
		_, err := api.IdempotencyKeyFrom("", "")
		if err == nil {
			t.Fatal("a mutation with no key was accepted")
		}
		if err.Details["field"] != "client_request_id" {
			t.Errorf("details = %v", err.Details)
		}
		// The message says the thing that stops the mistake this rule
		// exists for.
		if !strings.Contains(err.Message, "fresh") {
			t.Errorf("the message does not say a fresh key is a different call: %q", err.Message)
		}
	})
	t.Run("an unusable key is refused and writes nothing", func(t *testing.T) {
		for name, key := range map[string]string{
			"too long":            strings.Repeat("k", api.MaxIdempotencyKeyBytes+1),
			"a control character": "k\x00k",
			"a newline":           "k\nk",
		} {
			if _, err := api.IdempotencyKeyFrom(key, ""); err == nil {
				t.Errorf("%s was accepted", name)
			} else if err.Details["field"] != "client_request_id" {
				t.Errorf("%s named %v", name, err.Details)
			}
		}
		// Exactly at the bound is fine: the rule is "over 200 bytes".
		if _, err := api.IdempotencyKeyFrom(strings.Repeat("k", api.MaxIdempotencyKeyBytes), ""); err != nil {
			t.Errorf("a key of exactly the maximum length was refused: %v", err)
		}
	})
}
