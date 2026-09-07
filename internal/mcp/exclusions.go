package mcp

import "github.com/thisnick/agent-gm/internal/api"

// The named exclusions of spec section 8.2, with the reason for each.
//
// The rule they complete is this: **every `/v1` route carrying a `messages:*`
// scope is served to MCP in exactly one of three ways -- as a tool, as a
// resource, or as a named exclusion here.** Every `admin`-scoped route and
// every credential route is served in none of the three, by design.
//
// Section 16 Slice 3 test 16 is a two-way table test over exactly that
// statement, with three categories rather than two. Writing the exclusions
// down as data rather than as prose is what makes the third category
// checkable: a messaging route that is neither a tool nor a resource must
// appear here, with a reason somebody wrote on purpose, or the test fails.

// Exclusion is one messaging route deliberately not served as a tool.
type Exclusion struct {
	// Route is the inventory name.
	Route string
	// Reason is section 8.2's reason, in its own words.
	Reason string
}

// Exclusions is section 8.2's table, restricted to the routes that carry a
// messaging scope -- the `admin` and credential rows of that table are covered
// by the "none of the three" half of the rule and need no entry here.
var Exclusions = []Exclusion{
	{
		Route:  "conversations_typing",
		Reason: "no lasting effect and no result a model can act on",
	},
	{
		Route:  "message_attachments_list",
		Reason: "its data is already inside `get_message`; a tool would only add a round trip",
	},
	{
		Route:  "uploads_get",
		Reason: "an agent that has just called `create_upload` already holds everything it would return",
	},
	{
		Route:  "uploads_delete",
		Reason: "an agent that has just called `create_upload` already holds everything it would return",
	},
	{
		Route: "accounts_events",
		Reason: "a stream; MCP tools are request/response. `get_session` and `list_accounts` " +
			"answer the same question at a point in time",
	},
	{
		Route:  "accounts_events_all",
		Reason: "the all-accounts form of the same stream",
	},
	{
		Route: "operations_list",
		Reason: "`get_operation` covers the ID-addressed case, the only one a model reaches: " +
			"it holds the `operation_id` the write returned. Listing operations is an owner's " +
			"audit question, served by `agm operations list`",
	},
	{
		Route: "conversation_messages_list",
		Reason: "the same rows as `list_messages` with `conversation_id` set, which is the argument " +
			"shape a model already has; two tools for one listing would be two places a filter could drift",
	},
}

// ExcludedRoutes is the exclusion set, by route name.
func ExcludedRoutes() map[string]string {
	out := make(map[string]string, len(Exclusions))
	for _, e := range Exclusions {
		out[e.Route] = e.Reason
	}
	return out
}

// ResourceRoutes is the routes served as an MCP **resource** rather than as a
// tool: bytes belong in a resource so a client can fetch them without putting
// them through the model's context (spec section 8.2).
func ResourceRoutes() map[string]string {
	return map[string]string{
		"attachments_content": "served as the `agm://attachments/{attachment_id}` resource",
	}
}

// ToolRoutes is every route reachable through a tool, mapped to the tool that
// reaches it.
func ToolRoutes() map[string]string {
	out := map[string]string{}
	for _, t := range Tools {
		for _, r := range t.routes() {
			out[r] = t.Name
		}
	}
	return out
}

// IsMessagingScope reports whether a route's scope is one of the three
// messaging scopes -- the routes the three-way rule is about.
func IsMessagingScope(s api.Scope) bool {
	switch s {
	case api.ScopeRead, api.ScopeWrite, api.ScopeDelete:
		return true
	default:
		return false
	}
}
