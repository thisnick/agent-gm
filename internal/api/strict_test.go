package api_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/settings"
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
		if r.RawBody {
			// The body is bytes, so there is no field to be unknown. The
			// route is bounded by its reservation instead, which
			// TestSlice2_17_* asserts.
			continue
		}
		if r.OpenBody {
			// The field set is not a fixed list -- it IS the settings keys of
			// section 15.1. Strictness is not waived here, it moves: the
			// settings registry refuses an unknown key, and refuses it
			// harder than a name check could. That is asserted separately,
			// by TestTheOpenBodyRouteIsStillStrict, so this exception cannot
			// become a hole.
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
// "a key supplied anywhere but the header is invalid_request". It is true by
// construction -- no route lists a key in Query, and D38 removed it from
// every Body -- and this asserts it route by route rather than trusting the
// construction.
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

// The key's ONE transport, and the fact that it is optional (spec section
// 6.3, D38).
//
// The old contradiction rule is gone with the body field it arbitrated: there
// is only one place a key can come from, so there is nothing left to
// contradict. What survives is the shape rule, because a key that IS present
// and unusable is a caller mistake worth reporting, and the 200-byte bound is
// what stops a key being used as a smuggling channel.
func TestIdempotencyKeyTransport(t *testing.T) {
	t.Run("the header", func(t *testing.T) {
		got, err := api.IdempotencyKeyFrom("k1")
		if err != nil || got != "k1" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("no header is not an error", func(t *testing.T) {
		got, err := api.IdempotencyKeyFrom("")
		if err != nil {
			t.Fatalf("an absent key was refused: %v", err)
		}
		if got != "" {
			t.Fatalf("an absent key produced %q; the server mints the operation ID, not a key", got)
		}
	})
	t.Run("an unusable key is refused and writes nothing", func(t *testing.T) {
		for name, key := range map[string]string{
			"too long":            strings.Repeat("k", api.MaxIdempotencyKeyBytes+1),
			"a control character": "k\x00k",
			"a newline":           "k\nk",
		} {
			if _, err := api.IdempotencyKeyFrom(key); err == nil {
				t.Errorf("%s was accepted", name)
			} else if err.Details["field"] != "Idempotency-Key" {
				t.Errorf("%s named %v, want the Idempotency-Key header", name, err.Details)
			}
		}
		// Exactly at the bound is fine: the rule is "over 200 bytes".
		if _, err := api.IdempotencyKeyFrom(strings.Repeat("k", api.MaxIdempotencyKeyBytes)); err != nil {
			t.Errorf("a key of exactly the maximum length was refused: %v", err)
		}
	})
}

// The two routes exempt from the JSON field check are exempt for a stated
// reason, and neither reason is "it is easier". This asserts the reasons hold.
//
// Plant: set OpenBody on a route that is not the settings PATCH and this test
// fails at "is declared OpenBody". Planted 2026-09-06.
func TestTheBodyCheckExemptionsAreExactlyTwoAndBothAreJustified(t *testing.T) {
	var raw, open []string
	for _, r := range api.Routes {
		if r.RawBody {
			raw = append(raw, r.Name)
		}
		if r.OpenBody {
			open = append(open, r.Name)
		}
		if r.RawBody && r.OpenBody {
			t.Errorf("%s is both RawBody and OpenBody, which is meaningless", r)
		}
	}
	if strings.Join(raw, ",") != "uploads_content" {
		t.Errorf("RawBody routes = %v; only the upload PUT carries bytes (10.2)", raw)
	}
	if strings.Join(open, ",") != "admin_settings_set" {
		t.Errorf("OpenBody routes = %v; only the settings PATCH is declared OpenBody, "+
			"because only its field names are somebody else's list (7.7, 15.1)", open)
	}
}

// The OpenBody route is still strict, and this is where that is proven: the
// settings registry refuses an unknown key, naming it, exactly as section 7.1
// requires -- and refuses more besides, which a name check could not.
func TestTheOpenBodyRouteIsStillStrict(t *testing.T) {
	set := settings.New(settings.NewRegistry(), settings.NewMemory(), func(string) string { return "" })
	ctx := context.Background()

	for name, body := range map[string]string{
		"an unknown key":  `{"backfill.concurrenc": 4}`,
		"a made-up key":   `{"totally.invented": 1}`,
		"a cache buster":  `{"_": 1}`,
		"a delete switch": `{"action": "wipe"}`,
	} {
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Patch(ctx, raw); err == nil {
			t.Errorf("%s was accepted by the settings registry", name)
		}
	}

	// And a real key is accepted, so the check is refusing the right things
	// rather than everything.
	raw := map[string]json.RawMessage{"backfill.concurrency": json.RawMessage("4")}
	if _, err := set.Patch(ctx, raw); err != nil {
		t.Errorf("a real settings key was refused: %v", err)
	}

	// The whole body is validated before anything is written: one bad key
	// rejects the request and changes nothing (7.7).
	mixed := map[string]json.RawMessage{
		"backfill.concurrency": json.RawMessage("2"),
		"nonsense.key":         json.RawMessage("1"),
	}
	if _, err := set.Patch(ctx, mixed); err == nil {
		t.Fatal("a body with one bad key was accepted")
	}
	eff, err := set.Get(ctx, "backfill.concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Value.Int != 4 {
		t.Errorf("the rejected PATCH changed backfill.concurrency to %d; it must change nothing",
			eff.Value.Int)
	}
}
