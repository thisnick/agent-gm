package api_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
)

// The inventory is the contract, so the first thing tested is the inventory
// itself: every route in spec section 7.5, 7.6 and 7.7 is here, exactly once,
// with the scope the spec assigns it. A route added to the spec and not here
// fails; a route here that the spec does not name fails too.
//
// The list is written out by hand from the spec's tables rather than derived
// from api.Routes, because a test that derived it would agree with the code
// by construction and prove nothing.
func TestRouteInventoryMatchesTheSpec(t *testing.T) {
	type want struct {
		method string
		path   string
		scope  api.Scope
	}
	spec := []want{
		{http.MethodGet, "/healthz", api.ScopeNone},
		{http.MethodGet, "/v1/health", api.ScopeRead},
		{http.MethodPost, "/v1/auth/admin-session", api.ScopeNone},
		{http.MethodPost, "/v1/auth/refresh", api.ScopeNone},
		{http.MethodGet, "/v1/auth/whoami", api.ScopeRead},
		{http.MethodPost, "/v1/auth/logout", api.ScopeRead},
		{http.MethodGet, "/v1/accounts", api.ScopeRead},
		{http.MethodGet, "/v1/accounts/{account_id}", api.ScopeRead},
		{http.MethodPatch, "/v1/accounts/{account_id}", api.ScopeAdmin},
		{http.MethodGet, "/v1/accounts/{account_id}/events", api.ScopeRead},
		{http.MethodGet, "/v1/accounts/events", api.ScopeRead},
		{http.MethodPost, "/v1/accounts/{account_id}/reconnect", api.ScopeAdmin},
		{http.MethodPost, "/v1/accounts/{account_id}/sign-out", api.ScopeAdmin},
		{http.MethodDelete, "/v1/accounts/{account_id}", api.ScopeAdmin},
		{http.MethodPost, "/v1/accounts/{account_id}/refresh-cookies", api.ScopeAdmin},
		{http.MethodPost, "/v1/pairing/start", api.ScopeAdmin},
		{http.MethodGet, "/v1/pairing/{pairing_id}", api.ScopeAdmin},
		{http.MethodDelete, "/v1/pairing/{pairing_id}", api.ScopeAdmin},
		{http.MethodGet, "/v1/conversations", api.ScopeRead},
		{http.MethodGet, "/v1/conversations/{conversation_id}", api.ScopeRead},
		{http.MethodGet, "/v1/conversations/{conversation_id}/messages", api.ScopeRead},
		{http.MethodGet, "/v1/messages", api.ScopeRead},
		{http.MethodGet, "/v1/messages/{message_id}", api.ScopeRead},
		{http.MethodGet, "/v1/messages/{message_id}/context", api.ScopeRead},
		{http.MethodGet, "/v1/messages/{message_id}/attachments", api.ScopeRead},
		{http.MethodGet, "/v1/search/messages", api.ScopeRead},
		{http.MethodGet, "/v1/contacts", api.ScopeRead},
		{http.MethodGet, "/v1/attachments/{attachment_id}", api.ScopeRead},
		{http.MethodGet, "/v1/attachments/{attachment_id}/content", api.ScopeRead},
		{http.MethodGet, "/v1/operations", api.ScopeWrite},
		{http.MethodGet, "/v1/operations/{operation_id}", api.ScopeWrite},
		{http.MethodGet, "/v1/uploads/{upload_id}", api.ScopeWrite},
		{http.MethodPost, "/v1/conversations", api.ScopeWrite},
		{http.MethodPost, "/v1/conversations/{conversation_id}/messages", api.ScopeWrite},
		{http.MethodPost, "/v1/conversations/{conversation_id}/typing", api.ScopeWrite},
		{http.MethodPost, "/v1/conversations/{conversation_id}/read", api.ScopeWrite},
		{http.MethodPatch, "/v1/conversations/{conversation_id}", api.ScopeWrite},
		{http.MethodPost, "/v1/messages/{message_id}/reactions", api.ScopeWrite},
		{http.MethodDelete, "/v1/messages/{message_id}/reactions/{emoji}", api.ScopeWrite},
		{http.MethodDelete, "/v1/reactions/{reaction_id}", api.ScopeWrite},
		{http.MethodPost, "/v1/uploads", api.ScopeWrite},
		{http.MethodDelete, "/v1/uploads/{upload_id}", api.ScopeWrite},
		{http.MethodPut, "/v1/uploads/{upload_id}/content", api.ScopeNone},
		{http.MethodDelete, "/v1/messages/{message_id}", api.ScopeDelete},
		{http.MethodDelete, "/v1/conversations/{conversation_id}", api.ScopeDelete},
		{http.MethodGet, "/v1/admin/settings", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/settings/{key}", api.ScopeAdmin},
		{http.MethodPatch, "/v1/admin/settings", api.ScopeAdmin},
		{http.MethodPost, "/v1/admin/backfill", api.ScopeAdmin},
		{http.MethodPost, "/v1/admin/backup", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/audit", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/diagnostics", api.ScopeAdmin},

		// Section 7.7's admin table ends with one row that names four route
		// families and defers their shapes to section 9:
		//
		//     | -- | /v1/admin/enrollment-codes,
		//            /v1/admin/authorization-requests,
		//            /v1/admin/authorizations, /v1/admin/clients | section 9 |
		//
		// So these fourteen are restated from sections 9.5 and 9.6 rather
		// than from 7.7, and they are listed here for the same reason every
		// other row is: this test is the place the inventory and the spec are
		// held to each other, and a family the spec defers is still a family
		// the spec names.
		{http.MethodPost, "/v1/admin/enrollment-codes", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/enrollment-codes", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/enrollment-codes/{enrollment_code_id}", api.ScopeAdmin},
		{http.MethodDelete, "/v1/admin/enrollment-codes/{enrollment_code_id}", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/authorization-requests", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/authorization-requests/{authorization_request_id}", api.ScopeAdmin},
		{http.MethodPost, "/v1/admin/authorization-requests/{authorization_request_id}/approve", api.ScopeAdmin},
		{http.MethodPost, "/v1/admin/authorization-requests/{authorization_request_id}/deny", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/authorizations", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/authorizations/{authorization_id}", api.ScopeAdmin},
		{http.MethodDelete, "/v1/admin/authorizations/{authorization_id}", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/clients", api.ScopeAdmin},
		{http.MethodGet, "/v1/admin/clients/{client_id}", api.ScopeAdmin},
		{http.MethodDelete, "/v1/admin/clients/{client_id}", api.ScopeAdmin},
	}

	key := func(m, p string) string { return m + " " + p }
	have := map[string]api.Route{}
	for _, r := range api.Routes {
		k := key(r.Method, r.Path)
		if _, dup := have[k]; dup {
			t.Errorf("%s is declared twice", k)
		}
		have[k] = r
	}
	for _, w := range spec {
		k := key(w.method, w.path)
		r, ok := have[k]
		if !ok {
			t.Errorf("the spec names %s and the inventory does not", k)
			continue
		}
		if r.Scope != w.scope {
			t.Errorf("%s carries scope %q, the spec says %q", k, r.Scope, w.scope)
		}
		delete(have, k)
	}
	for k := range have {
		t.Errorf("the inventory declares %s and the spec does not name it", k)
	}
	if len(api.Routes) != len(spec) {
		t.Errorf("the inventory has %d routes, the spec names %d", len(api.Routes), len(spec))
	}
}

// Names are the stable identifier the CLI, the tests and (in Slice 3) MCP
// use, so a duplicate or an empty one would silently make two routes one.
func TestRouteNamesAreUniqueAndPresent(t *testing.T) {
	seen := map[string]string{}
	for _, r := range api.Routes {
		if r.Name == "" {
			t.Errorf("%s has no name", r)
			continue
		}
		if prev, dup := seen[r.Name]; dup {
			t.Errorf("name %q is used by both %s and %s", r.Name, prev, r)
		}
		seen[r.Name] = r.String()
		if got, ok := api.RouteByName(r.Name); !ok || got.Path != r.Path {
			t.Errorf("RouteByName(%q) did not return %s", r.Name, r)
		}
	}
}

// There is no `?client_request_id=` query parameter on any route. The
// idempotency key has exactly two transports -- the Idempotency-Key header
// and the client_request_id body field -- and one presented as a query
// parameter is invalid_request naming it, like any other unknown parameter
// (spec section 7.1).
//
// Plant: add "client_request_id" to any route's Query and this test fails.
// Planted 2026-09-06.
func TestNoRouteTakesTheIdempotencyKeyAsAQueryParameter(t *testing.T) {
	for _, r := range api.Routes {
		for _, q := range r.Query {
			if q == "client_request_id" || q == "idempotency_key" {
				t.Errorf("%s accepts %q as a query parameter", r, q)
			}
		}
	}
}

// Every mutation requires an idempotency key (D7), and the two transports of
// it are the header and the body field. Typing is the one deliberate
// exception: it has no lasting effect, so there is nothing to make
// idempotent (D15).
func TestEveryMutationRequiresAnIdempotencyKey(t *testing.T) {
	mutations := map[string]bool{}
	for _, r := range api.Routes {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		default:
			continue
		}
		// Credential routes, pairing and admin acts are owner ceremonies,
		// not agent mutations, and carry confirmation rather than keys.
		if r.Scope == api.ScopeAdmin || r.Scope == api.ScopeNone ||
			strings.HasPrefix(r.Path, "/v1/auth/") {
			continue
		}
		mutations[r.Name] = r.IdempotencyKey
	}
	// Two deliberate exceptions, each for a stated reason rather than an
	// oversight:
	//   conversations_typing -- no lasting effect, so there is nothing to
	//     make idempotent (D15), which is also why it has no MCP tool.
	//   uploads_delete -- it drops a purely local reservation the caller
	//     already holds the ID of. Repeating it is already a no-op, and 7.7
	//     gives it no body at all. It has no CLI command and no MCP tool for
	//     the same reason (11.3, 8.2).
	exempt := map[string]string{
		"conversations_typing": "no lasting effect (D15)",
		"uploads_delete":       "drops a purely local reservation, already a no-op on repeat",
	}
	for name, hasKey := range mutations {
		if why, ok := exempt[name]; ok {
			if hasKey {
				t.Errorf("%s carries an idempotency key, but it is exempt: %s", name, why)
			}
			continue
		}
		if !hasKey {
			t.Errorf("%s is a mutation with no idempotency key", name)
		}
		r, _ := api.RouteByName(name)
		if !r.AllowsBody("client_request_id") {
			t.Errorf("%s does not accept client_request_id in its body", name)
		}
	}
}

// Every listing is paginated the same way: cursor and limit, and nothing
// else, so a client that walks one listing walks them all (spec section 7.4).
func TestEveryPaginatedRouteTakesCursorAndLimit(t *testing.T) {
	for _, r := range api.Routes {
		if !r.Paginated {
			for _, q := range r.Query {
				if q == "cursor" {
					t.Errorf("%s takes a cursor but is not marked paginated", r)
				}
			}
			continue
		}
		if !r.AllowsQuery("cursor") || !r.AllowsQuery("limit") {
			t.Errorf("%s is paginated but does not take both cursor and limit", r)
		}
	}
}

// Path parameters are part of the address and are never query parameters: a
// path parameter presented as a query parameter is an unknown parameter like
// any other.
func TestPathParametersAreNotAlsoQueryParameters(t *testing.T) {
	for _, r := range api.Routes {
		for _, p := range r.PathParams() {
			if r.AllowsQuery(p) {
				t.Errorf("%s takes %q both in the path and as a query parameter", r, p)
			}
		}
	}
	// Spot-check the extractor itself.
	r, ok := api.RouteByName("reactions_remove")
	if !ok {
		t.Fatal("reactions_remove is missing")
	}
	got := r.PathParams()
	want := []string{"message_id", "emoji"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PathParams = %v, want %v", got, want)
	}
}

// Parameters() is what the CLI two-way mapping test walks, so it must be
// sorted, deduplicated, and cover all three sources.
func TestParametersCoversPathQueryAndBody(t *testing.T) {
	r, ok := api.RouteByName("messages_send")
	if !ok {
		t.Fatal("messages_send is missing")
	}
	got := r.Parameters()
	if !sort.StringsAreSorted(got) {
		t.Errorf("Parameters() is not sorted: %v", got)
	}
	for _, want := range []string{
		"conversation_id", "text", "upload_ids", "reply_to_message_id",
		"force_rcs", "client_request_id",
	} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Parameters() = %v, missing %q", got, want)
		}
	}
}

// Nothing is allowlisted, including a cache-busting parameter (spec section
// 7.1, section 16 Slice 2 test 12's `?_=1` case). The inventory is what makes
// that testable route by route rather than by sampling.
func TestNoRouteAllowsACacheBustingParameter(t *testing.T) {
	for _, r := range api.Routes {
		for _, banned := range []string{"_", "_t", "cb", "nocache"} {
			if r.AllowsQuery(banned) {
				t.Errorf("%s allowlists the cache-busting parameter %q", r, banned)
			}
		}
	}
}

// The two deletes are the only messages:delete routes, and there is no other
// delete and no delete option (D14).
func TestOnlyTwoRoutesCarryTheDeleteScope(t *testing.T) {
	var got []string
	for _, r := range api.Routes {
		if r.Scope == api.ScopeDelete {
			got = append(got, r.Name)
		}
		for _, b := range r.Body {
			if b == "action" || b == "scope" || b == "for_everyone" {
				t.Errorf("%s takes a delete switch %q; there is no delete option (D14)", r, b)
			}
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "conversations_delete,messages_delete" {
		t.Errorf("messages:delete routes = %v, want exactly the two deletes", got)
	}
}
