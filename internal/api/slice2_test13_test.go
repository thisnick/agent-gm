package api_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
)

// Section 16 Slice 2 test 13:
//
//	"A `msg_` ID where a `conv_` is expected is `invalid_request` naming the
//	 parameter and the expected prefix -- **never `not_found`**. A raw Google
//	 ID is the same."
//
// The distinction is not pedantry. `not_found` says "the object you named is
// gone", and an agent that reads it will tell its owner the conversation was
// deleted, or start a new one, or give up. `invalid_request` naming the
// parameter and the prefix says "you passed the wrong kind of ID", which is a
// mistake with a fix. A raw Google ID lands in the same place because it
// carries no Agent GM prefix at all, and an agent that has one has almost
// certainly read it out of a log or a diagnostics response where it does not
// belong on a public surface.
//
// This walks **every route with an ID path parameter**, route by route, so a
// route added later without the check fails here rather than being noticed by
// somebody's owner.
func TestSlice2_13_WrongPrefixIsInvalidRequestNeverNotFound(t *testing.T) {
	s := newServer(t)
	s.addAccount(addressA)

	// Two shapes of wrong ID: another Agent GM type, and a raw Google
	// identifier. Google's own conversation IDs look like this and carry no
	// prefix, so nothing about them can be mistaken for an Agent GM ID.
	wrongValues := map[string]string{
		"another Agent GM type": "",
		"a raw Google ID":       "17_1a2b3c4d5e6f",
	}

	tested := 0
	for _, route := range route13Cases() {
		for _, param := range route.PathParams() {
			wantPrefix, isTypedID := idPrefixes[param]
			if !isTypedID {
				continue
			}
			tested++
			for label, wrong := range wrongValues {
				value := wrong
				if value == "" {
					// A different Agent GM type: msg_ where conv_ belongs,
					// and conv_ where msg_ belongs.
					value = otherPrefix(wantPrefix) + "00000000-0000-5000-8000-000000000000"
				}
				path := route.Path
				for _, other := range route.PathParams() {
					replacement := plausible(other)
					if other == param {
						replacement = value
					}
					path = strings.Replace(path, "{"+other+"}", replacement, 1)
				}

				name := route.Name + "/" + param + "/" + label
				t.Run(name, func(t *testing.T) {
					env := s.callWith(s.Token, route.Method, path, bodyFor(route),
						map[string]string{"Idempotency-Key": key(name)})

					if env.Error == nil {
						t.Fatalf("a %s in %s was accepted: %s", label, param, truncate(env.Raw))
					}
					if env.Error.Code == "not_found" {
						t.Fatalf("a %s in %s answered not_found; spec section 16 test 13 "+
							"requires invalid_request naming the parameter and the expected prefix",
							label, param)
					}
					if env.Error.Code != "invalid_request" {
						t.Fatalf("a %s in %s answered %s, want invalid_request",
							label, param, env.Error.Code)
					}
					if got := env.detail("parameter"); got != param {
						t.Errorf("details.parameter is %v, want %q", got, param)
					}
					if got := env.detail("expected_prefix"); got != wantPrefix {
						t.Errorf("details.expected_prefix is %v, want %q", got, wantPrefix)
					}
					// The offending value is deliberately NOT echoed: it may
					// be a raw Google identifier, which is admin-only on a
					// public surface (spec section 4.1).
					if strings.Contains(string(env.Raw), "17_1a2b3c4d5e6f") {
						t.Errorf("the refusal echoed the raw Google ID back: %s", truncate(env.Raw))
					}
				})
			}
		}
	}
	if tested == 0 {
		t.Fatal("no route with an ID path parameter was exercised; the walk is broken")
	}
}

// otherPrefix picks a different typed prefix, so the value under test is a
// well-formed Agent GM ID of the wrong kind rather than gibberish.
func otherPrefix(want string) string {
	if want == "conv_" {
		return "msg_"
	}
	return "conv_"
}

// route13Cases is every route with at least one typed-ID path parameter.
func route13Cases() []api.Route {
	var out []api.Route
	for _, r := range api.Routes {
		if r.Name == "accounts_events" {
			continue // an SSE stream never returns; its prefix check is asserted separately
		}
		for _, p := range r.PathParams() {
			if _, ok := idPrefixes[p]; ok {
				out = append(out, r)
				break
			}
		}
	}
	return out
}
