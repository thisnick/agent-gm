package api_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/authz"
)

// api.Server.Invoke is how the MCP surface of spec section 8 reaches a route
// handler in process. These tests assert the one property that makes it worth
// having: **it runs the same checks the transport runs**, so the two surfaces
// cannot drift.
//
// A tool that reached the handler directly would skip the strict parameter
// rejection of section 7.1 and the scope check of section 9.7, and the drift
// would be invisible until a model sent a misspelled filter and got a full
// unfiltered list back.

// auth authenticates the harness's admin token into the value a handler sees.
func (s *server) auth(t *testing.T, token string) *authz.Authorization {
	t.Helper()
	a, err := s.Deps.Authz.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("authenticating: %v", err)
	}
	return a
}

func TestInvokeRunsTheSameHandler(t *testing.T) {
	s := newServer(t)
	s.addAccount(addressA)

	invoked, e := s.Server.Invoke(api.Invocation{
		Ctx:       context.Background(),
		RouteName: "accounts_list",
		Auth:      s.auth(t, s.Token),
		Source:    "test",
	})
	if e != nil {
		t.Fatalf("invoking accounts_list: %v", e)
	}
	if invoked.Route.Name != "accounts_list" {
		t.Fatalf("the invocation reports route %q", invoked.Route.Name)
	}
	if invoked.Response == nil || invoked.Response.Data == nil {
		t.Fatal("accounts_list answered no data")
	}
}

// TestInvokeRunsTheStrictParameterChecks is the whole point: an unknown query
// parameter and an unknown body field are refused in process exactly as they
// are on the wire.
func TestInvokeRunsTheStrictParameterChecks(t *testing.T) {
	s := newServer(t)
	s.addAccount(addressA)
	auth := s.auth(t, s.Token)

	_, e := s.Server.Invoke(api.Invocation{
		Ctx:       context.Background(),
		RouteName: "conversations_list",
		Query:     url.Values{"directon": {"incoming"}},
		Auth:      auth,
		Source:    "test",
	})
	if e == nil {
		t.Fatal("a misspelled query parameter was accepted in process")
	}
	if string(e.Code) != "invalid_request" {
		t.Fatalf("the refusal is %q, want invalid_request", e.Code)
	}

	_, e = s.Server.Invoke(api.Invocation{
		Ctx:       context.Background(),
		RouteName: "conversations_start",
		Body:      []byte(`{"recipients":["+12025550101"],"nmae":"typo"}`),
		Auth:      auth,
		Source:    "test",
	})
	if e == nil {
		t.Fatal("an unknown body field was accepted in process")
	}
	if string(e.Code) != "invalid_request" {
		t.Fatalf("the refusal is %q, want invalid_request", e.Code)
	}
}

// TestInvokeRechecksScope proves the call-time half of section 8.2's "visibility
// is not authorization".
func TestInvokeRechecksScope(t *testing.T) {
	s := newServer(t)
	sess, err := s.Deps.Authz.MintAdminSession(context.Background(), testAdminSecret,
		[]string{"messages:read"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("minting a narrowed session: %v", err)
	}
	readOnly := s.auth(t, sess.AccessToken)

	_, e := s.Server.Invoke(api.Invocation{
		Ctx:       context.Background(),
		RouteName: "messages_delete",
		Path:      map[string]string{"message_id": "msg_x"},
		Body:      []byte(`{}`),
		Auth:      readOnly,
		Source:    "test",
	})
	if e == nil {
		t.Fatal("a messages:read authorization reached a messages:delete route")
	}
	if string(e.Code) != "insufficient_scope" {
		t.Fatalf("the refusal is %q, want insufficient_scope", e.Code)
	}

	// And no authorization at all on a scoped route is invalid_token, not a
	// handler running with a nil caller.
	_, e = s.Server.Invoke(api.Invocation{
		Ctx: context.Background(), RouteName: "accounts_list", Source: "test",
	})
	if e == nil || string(e.Code) != "invalid_token" {
		t.Fatalf("an unauthenticated invocation answered %v, want invalid_token", e)
	}
}

// TestInvokeRefusesAnUnknownRoute keeps a typo in the MCP catalogue a startup
// or test failure rather than a 500 in production.
func TestInvokeRefusesAnUnknownRoute(t *testing.T) {
	s := newServer(t)
	_, e := s.Server.Invoke(api.Invocation{
		Ctx: context.Background(), RouteName: "conversations_teleport",
		Auth: s.auth(t, s.Token), Source: "test",
	})
	if e == nil || string(e.Code) != "internal_error" {
		t.Fatalf("an unknown route name answered %v, want internal_error", e)
	}
}

// TestDataShapeForNamesRealDTOs asserts every declared shape resolves, so the
// MCP output schemas describe something.
func TestDataShapeForNamesRealDTOs(t *testing.T) {
	for _, name := range []string{
		"conversations_list", "conversations_get", "messages_list", "messages_get",
		"messages_context", "search_messages", "attachments_get", "contacts_list",
		"accounts_get", "accounts_list", "health", "messages_send",
		"conversations_start", "conversations_mark_read", "reactions_add",
		"reactions_remove", "reactions_remove_by_id", "conversations_update",
		"uploads_create", "operations_get", "messages_delete", "conversations_delete",
	} {
		shape, ok := api.DataShapeFor(name)
		if !ok {
			t.Errorf("%s has no declared data shape", name)
			continue
		}
		if shape.Type == nil {
			t.Errorf("%s's data shape names no type", name)
		}
		if _, isRoute := api.RouteByName(name); !isRoute {
			t.Errorf("%s is not a route in the inventory", name)
		}
	}
	if api.SearchCoverageType() == nil {
		t.Fatal("the search coverage type is nil")
	}
}
