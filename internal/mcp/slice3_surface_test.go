package mcp_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/mcp"
)

// TestSlice3Test16TheThreeWaySplit is acceptance test 16.
//
// **A two-way table test over the route inventory and the MCP surface**, with
// THREE categories rather than two:
//
//   - every `/v1` route whose scope is `messages:read`, `messages:write` or
//     `messages:delete` is served as a **tool**, as a **resource**, or as a
//     **named exclusion**;
//   - every `admin`-scoped route and every credential route (`/v1/auth/*`) is
//     served as none of the three.
//
// A tool with no route, or a messaging route in none of the three categories,
// fails it. Both directions are walked, because the two failures are
// different: a route added without a tool is a capability nobody can reach,
// and a tool naming a route nobody declared is a 500 the first time it is
// called.
func TestSlice3Test16TheThreeWaySplit(t *testing.T) {
	tools := mcp.ToolRoutes()
	resources := mcp.ResourceRoutes()
	exclusions := mcp.ExcludedRoutes()

	// Direction one: every route in the inventory.
	var messaging, silent int
	for _, route := range api.Routes {
		_, isTool := tools[route.Name]
		_, isResource := resources[route.Name]
		reason, isExcluded := exclusions[route.Name]
		categories := 0
		for _, in := range []bool{isTool, isResource, isExcluded} {
			if in {
				categories++
			}
		}

		switch {
		case isCredentialRoute(route):
			silent++
			if categories != 0 {
				t.Errorf("%s (%s) is a credential route and must be served in NONE of the three; it is in %d",
					route.Name, route, categories)
			}
		case route.Scope == api.ScopeAdmin:
			silent++
			if categories != 0 {
				t.Errorf("%s (%s) is admin-scoped and must be served in NONE of the three; it is in %d",
					route.Name, route, categories)
			}
		case mcp.IsMessagingScope(route.Scope):
			messaging++
			if categories != 1 {
				t.Errorf("%s (%s) carries %s and is served in %d of the three categories, want exactly one "+
					"(tool=%v resource=%v exclusion=%v)",
					route.Name, route, route.Scope, categories, isTool, isResource, isExcluded)
			}
			if isExcluded && strings.TrimSpace(reason) == "" {
				t.Errorf("%s is excluded with no reason; an exclusion nobody wrote down is an omission", route.Name)
			}
		}
	}
	if messaging == 0 {
		t.Fatal("no messaging route was found, so the table proves nothing")
	}
	if silent == 0 {
		t.Fatal("no admin or credential route was found, so the 'none of the three' half proves nothing")
	}
	t.Logf("%d messaging routes served in exactly one category; %d admin/credential routes served in none",
		messaging, silent)

	// Direction two: every tool names a route that exists.
	for _, tool := range mcp.Tools {
		names := routesOf(tool)
		if len(names) == 0 || names[0] == "" {
			t.Errorf("%s names no route at all", tool.Name)
			continue
		}
		for _, name := range names {
			route, ok := api.RouteByName(name)
			if !ok {
				t.Errorf("%s names route %q, which is not in the inventory", tool.Name, name)
				continue
			}
			if !mcp.IsMessagingScope(route.Scope) {
				t.Errorf("%s serves %s, whose scope is %q -- MCP serves the messaging surface, not the whole API",
					tool.Name, route, route.Scope)
			}
			// The tool's scope must be the route's scope. A tool visible
			// under a narrower scope than its route requires would be an
			// invitation to a refusal.
			if string(tool.Scope) != string(route.Scope) {
				t.Errorf("%s is gated on %q but its route %s requires %q",
					tool.Name, tool.Scope, route, route.Scope)
			}
		}
	}

	// Direction three: every named exclusion and every resource names a real
	// messaging route, so the exclusion list cannot rot into a list of names
	// that no longer exist.
	for name := range exclusions {
		route, ok := api.RouteByName(name)
		if !ok {
			t.Errorf("the exclusion %q names no route in the inventory", name)
			continue
		}
		if !mcp.IsMessagingScope(route.Scope) {
			t.Errorf("the exclusion %q names %s, which carries no messaging scope and needs no exclusion", name, route)
		}
	}
	for name := range resources {
		route, ok := api.RouteByName(name)
		if !ok {
			t.Errorf("the resource %q names no route in the inventory", name)
			continue
		}
		if !mcp.IsMessagingScope(route.Scope) {
			t.Errorf("the resource %q names %s, which carries no messaging scope", name, route)
		}
	}
}

// isCredentialRoute is the `/v1/auth/*` family: credential self-management and
// credential issuance.
//
// A model does not choose its own token, cannot act on the answer, and must
// not be able to log its client out mid-conversation. The client owns its
// credential; the model does not (spec section 8.2).
func isCredentialRoute(r api.Route) bool {
	return strings.HasPrefix(r.Path, "/v1/auth/")
}

// TestSlice3ExclusionsAreTheOnesSection82Names guards the exclusion list
// against quiet growth. Adding a route to it is how a capability disappears
// from MCP without anybody deciding to remove it.
func TestSlice3ExclusionsAreTheOnesSection82Names(t *testing.T) {
	want := []string{
		"accounts_events",
		"accounts_events_all",
		"conversation_messages_list",
		"conversations_typing",
		"message_attachments_list",
		"operations_list",
		"uploads_delete",
		"uploads_get",
	}
	var got []string
	for name := range mcp.ExcludedRoutes() {
		got = append(got, name)
	}
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Fatalf("the named exclusions are %v, want %v", got, want)
	}
}
