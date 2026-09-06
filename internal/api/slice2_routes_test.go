package api_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/store"
)

// TestEveryRouteHasAHandler is the assertion that makes the route inventory
// worth having.
//
// A route declared and left unserved must be a **test failure, not a 500 in
// production**. That is the direction that actually goes wrong: adding a row
// to a table is easy, and the 500 it produces surfaces on the day an agent
// first calls it, which is the worst possible day to find out.
//
// RegisterAll already refuses to return successfully with an unserved route,
// so this test is short -- but it is the test, not the check inside
// RegisterAll, that fails the build.
func TestEveryRouteHasAHandler(t *testing.T) {
	s := newServer(t)

	server := api.NewServer(api.Deps{Authz: s.Deps.Authz, PublicURL: publicURL})
	if err := api.RegisterAll(server, s.Deps); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	registered := map[string]bool{}
	for _, name := range server.Registered() {
		registered[name] = true
	}
	var missing []string
	for _, route := range api.Routes {
		if !registered[route.Name] {
			missing = append(missing, route.Name+" ("+route.String()+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("these routes are declared and have no handler:\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(registered) != len(api.Routes) {
		t.Errorf("%d handlers registered for %d routes; a handler for a route "+
			"nobody declared would have failed Handle, so this is a duplicate name",
			len(registered), len(api.Routes))
	}
}

// TestNoRouteAnswersUnserved walks every route with a request that should
// reach its handler and asserts none of them answers "declared but not served
// by this build", which is the message ServeHTTP produces for a missing
// handler. It is the runtime half of the test above: registration succeeding
// and the handler being reachable are two different facts.
func TestNoRouteAnswersUnserved(t *testing.T) {
	s := newServer(t)
	s.addAccount(addressA)

	for _, route := range api.Routes {
		if route.Name == "accounts_events" || route.Name == "accounts_events_all" {
			continue // an SSE stream never returns; it is exercised on its own
		}
		path := concretePath(t, s, route)
		env := s.callWith(s.Token, route.Method, path, bodyFor(route), map[string]string{
			"Idempotency-Key": key(route.Name),
		})
		if env.Error != nil && strings.Contains(env.Error.Message, "not served by this build") {
			t.Errorf("%s: %s", route.Name, env.Error.Message)
		}
		if env.Status == http.StatusNotImplemented {
			t.Errorf("%s answered 501", route.Name)
		}
	}
}

// --- helpers shared with the wrong-prefix test ------------------------------

// idPrefixes maps every path parameter that carries an Agent GM typed ID to
// the prefix spec section 4.1 gives it. `emoji`, `key` and `pairing_id` are
// deliberately absent: they are not typed IDs and the prefix rule does not
// apply to them.
var idPrefixes = map[string]string{
	"account_id":      store.PrefixAccount,
	"conversation_id": store.PrefixConversation,
	"message_id":      store.PrefixMessage,
	"attachment_id":   store.PrefixAttachment,
	"operation_id":    store.PrefixOperation,
	"upload_id":       store.PrefixUpload,
	"reaction_id":     store.PrefixReaction,
}

// plausible is a well-formed but absent ID for each parameter, so a route
// under test is refused for the reason being tested rather than for a
// different parameter in the same path.
func plausible(param string) string {
	if prefix, ok := idPrefixes[param]; ok {
		return prefix + "00000000-0000-5000-8000-000000000000"
	}
	switch param {
	case "emoji":
		return "%F0%9F%91%8D"
	case "key":
		return "backup.keep"
	default:
		return "pair_00000000"
	}
}

// concretePath fills every path parameter with a plausible value.
func concretePath(t *testing.T, s *server, route api.Route) string {
	t.Helper()
	path := route.Path
	for _, param := range route.PathParams() {
		path = strings.Replace(path, "{"+param+"}", plausible(param), 1)
	}
	return path
}

// bodyValues are the canned values for each body field, so a route's body can
// be built from the fields the INVENTORY says it accepts. Building it any
// other way would risk an unknown-field refusal (spec section 7.1) masking
// the refusal a test is actually looking for.
var bodyValues = map[string]any{
	"text":                "hello",
	"emoji":               "👍",
	"confirm":             true,
	"label":               "a label",
	"cookies":             map[string]string{"SID": "x"},
	"recipients":          []string{fictionalA},
	"name":                "",
	"folder":              "archived",
	"pinned":              nil,
	"unread":              nil,
	"message_id":          "",
	"account_id":          "",
	"conversation_id":     "",
	"device_index":        0,
	"filename":            "photo.jpg",
	"mime_type":           "image/jpeg",
	"size_bytes":          int64(3),
	"sha256":              "",
	"upload_ids":          []string{},
	"reply_to_message_id": "",
	"force_rcs":           false,
	"secret":              testAdminSecret,
	"scopes":              []string{},
	"refresh_token":       "",
	"client_request_id":   "",
}

// bodyFor builds a body from exactly the fields a route declares.
func bodyFor(route api.Route) any {
	if len(route.Body) == 0 {
		return nil
	}
	body := map[string]any{}
	for _, field := range route.Body {
		v, ok := bodyValues[field]
		if !ok || v == nil {
			continue
		}
		if s, isString := v.(string); isString && s == "" {
			continue
		}
		if list, isList := v.([]string); isList && len(list) == 0 {
			continue
		}
		body[field] = v
	}
	if len(body) == 0 {
		return nil
	}
	return body
}
