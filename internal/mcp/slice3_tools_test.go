package mcp_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/mcp"
)

// Section 16 Slice 3 tests 14 and 15: the catalogue, as a RUNNING SERVER
// serves it.
//
// Every assertion here reads `tools/list` over HTTP rather than mcp.Tools in
// memory. That is the point of test 15's wording -- "asserted by fetching
// `tools/list` from a running server and counting, so a stale description
// fails the claim". A test that read the table would prove the table is
// consistent with itself.

// the eleven reads, the eight writes and the two deletes of section 8.2.
var (
	readTools = []string{
		"get_attachment", "get_conversation", "get_health", "get_message",
		"get_session", "list_accounts", "list_contacts", "list_conversations",
		"list_messages", "message_context", "search_messages",
	}
	writeTools = []string{
		"add_reaction", "create_upload", "get_operation", "mark_read",
		"remove_reaction", "send_message", "start_conversation", "update_conversation",
	}
	deleteTools = []string{"delete_conversation", "delete_message"}
)

// TestSlice3Test14ScopeGating is acceptance test 14.
//
// `tools/list` under `messages:read` returns exactly the ELEVEN read tools and
// no write or delete tool; under `messages:write` exactly the eight more;
// under `messages:delete` exactly the two. `tools/call` on a non-visible name
// is refused again at call time.
func TestSlice3Test14ScopeGating(t *testing.T) {
	h := newHarness(t)

	readOnly := h.narrowToken("messages:read")
	got := h.toolNames(readOnly)
	sort.Strings(got)
	if !equalStrings(got, readTools) {
		t.Fatalf("messages:read sees %v, want exactly the eleven reads %v", got, readTools)
	}
	if len(got) != 11 {
		t.Fatalf("messages:read sees %d tools, want 11", len(got))
	}

	writeToken := h.narrowToken("messages:read", "messages:write")
	got = h.toolNames(writeToken)
	sort.Strings(got)
	want := append(append([]string{}, readTools...), writeTools...)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("messages:read+write sees %v, want %v", got, want)
	}

	deleteToken := h.narrowToken("messages:read", "messages:write", "messages:delete")
	got = h.toolNames(deleteToken)
	sort.Strings(got)
	want = append(want, deleteTools...)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("all three scopes see %v, want %v", got, want)
	}
	if len(got) != 21 {
		t.Fatalf("all three scopes see %d tools, want the twenty-one of section 8.2", len(got))
	}

	// A delete-only token sees the two deletes and nothing else, which is
	// section 9.7's "and nothing else" said as a test.
	deleteOnly := h.narrowToken("messages:delete")
	got = h.toolNames(deleteOnly)
	sort.Strings(got)
	if !equalStrings(got, deleteTools) {
		t.Fatalf("messages:delete alone sees %v, want %v", got, deleteTools)
	}

	// **Visibility is not authorization.** A client may call a name it
	// learned elsewhere, and the call is refused again -- as a RESULT the
	// model can act on, naming the scope it would have needed.
	result := h.toolWith(readOnly, "delete_message", map[string]any{
		"message_id": "msg_whatever",
	})
	if !isError(result) {
		t.Fatal("delete_message under messages:read alone was not refused at call time")
	}
	e := resultError(t, result)
	if e["code"] != "insufficient_scope" {
		t.Fatalf("the refusal is %q, want insufficient_scope", e["code"])
	}
	details, _ := e["details"].(map[string]any)
	if details["required_scope"] != "messages:delete" {
		t.Fatalf("the refusal does not name required_scope = messages:delete: %v", details)
	}
}

// TestSlice3Test15CatalogueClaims is acceptance test 15, fetched from a
// running server and counted.
//
// Every tool has a description; **every argument has a description**; every
// schema is `additionalProperties: false`; every write tool requires
// no idempotency key at all (D38); every write tool's description ends with the lost-result
// sentence of section 8.2.
func TestSlice3Test15CatalogueClaims(t *testing.T) {
	h := newHarness(t)
	listed := h.listedTools(h.Token)
	if len(listed) != 21 {
		t.Fatalf("tools/list served %d tools, want the twenty-one of section 8.2", len(listed))
	}

	arguments, descriptions := 0, 0
	for _, tool := range listed {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		if strings.TrimSpace(description) == "" {
			t.Errorf("%s has no description", name)
		}
		if _, ok := tool["outputSchema"]; !ok {
			t.Errorf("%s declares no outputSchema", name)
		}
		if _, ok := tool["annotations"]; !ok {
			t.Errorf("%s declares no annotations", name)
		}

		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no inputSchema", name)
		}
		if closed, _ := schema["additionalProperties"].(bool); closed {
			t.Errorf("%s declares additionalProperties: true", name)
		} else if _, present := schema["additionalProperties"]; !present {
			t.Errorf("%s does not declare additionalProperties at all", name)
		}

		props, _ := schema["properties"].(map[string]any)
		required := stringsOf(schema["required"])
		for argName, raw := range props {
			arguments++
			arg, _ := raw.(map[string]any)
			if d, _ := arg["description"].(string); strings.TrimSpace(d) != "" {
				descriptions++
			} else {
				t.Errorf("%s.%s has no description", name, argName)
			}
		}

		// D38: no tool takes an idempotency key of any spelling. The server
		// mints the operation ID, and a schema that still asked for one
		// would be asking a model for a value it cannot produce stably.
		for _, banned := range []string{"client_request_id", "idempotency_key", "Idempotency-Key"} {
			if _, has := props[banned]; has {
				t.Errorf("%s still takes %q; D38 removed the idempotency key from every tool", name, banned)
			}
			if contains(required, banned) {
				t.Errorf("%s still requires %q", name, banned)
			}
		}
	}

	// The measurable claim: count the arguments, count the descriptions, and
	// they must be equal. A single stale description fails it.
	if arguments != descriptions {
		t.Fatalf("tools/list served %d arguments and %d descriptions", arguments, descriptions)
	}
	if arguments == 0 {
		t.Fatal("tools/list served no arguments at all, so the count proves nothing")
	}
	t.Logf("tools/list served %d tools and %d arguments, each with a description", len(listed), arguments)

	// The nine write tools of section 8.2, named, so a tool that quietly
	// changed sides is caught by name rather than by a count. Each one's
	// description ends with the lost-result sentence, byte for byte -- the
	// sentence that replaced "invent a key" when D38 removed the key.
	wantWrites := []string{
		"add_reaction", "create_upload", "delete_conversation", "delete_message",
		"mark_read", "remove_reaction", "send_message", "start_conversation",
		"update_conversation",
	}
	var writes []string
	for _, tool := range mcp.Tools {
		if tool.IsWrite() {
			writes = append(writes, tool.Name)
			if !strings.HasSuffix(tool.Description, mcp.LostResultSentence) {
				t.Errorf("%s's description does not end with the lost-result sentence of section 8.2; it ends %q",
					tool.Name, tailOf(tool.Description))
			}
		}
	}
	sort.Strings(writes)
	if !equalStrings(writes, wantWrites) {
		t.Fatalf("the write tools are %v, want %v", writes, wantWrites)
	}
	// `get_operation` is gated on messages:write and is NOT one: it creates
	// nothing, so there is nothing to repeat.
	if contains(writes, "get_operation") {
		t.Fatal("get_operation is classed as a write, which it is not")
	}
}

// TestSlice3Test15Annotations is the annotations table of section 8.2,
// asserted row by row off the running server -- including the deliberate
// `openWorldHint: false` rows, which are the ones a conventional
// implementation gets wrong.
func TestSlice3Test15Annotations(t *testing.T) {
	h := newHarness(t)
	listed := h.listedTools(h.Token)
	got := map[string]map[string]any{}
	for _, tool := range listed {
		name, _ := tool["name"].(string)
		annotations, _ := tool["annotations"].(map[string]any)
		got[name] = annotations
	}

	type row struct{ readOnly, destructive, idempotent, openWorld bool }
	want := map[string]row{}
	for _, n := range append(append([]string{}, readTools...), "get_operation") {
		want[n] = row{true, false, true, false}
	}
	for _, n := range []string{"create_upload", "update_conversation"} {
		want[n] = row{false, false, true, false}
	}
	for _, n := range []string{"send_message", "start_conversation", "mark_read", "add_reaction"} {
		want[n] = row{false, false, true, true}
	}
	want["remove_reaction"] = row{false, true, true, true}
	for _, n := range deleteTools {
		want[n] = row{false, true, true, false}
	}

	for name, w := range want {
		a, ok := got[name]
		if !ok {
			t.Errorf("%s is not in tools/list", name)
			continue
		}
		check := func(key string, expected bool) {
			t.Helper()
			actual, present := a[key].(bool)
			if !present {
				t.Errorf("%s does not declare %s at all; false is a claim and an absent key is not", name, key)
				return
			}
			if actual != expected {
				t.Errorf("%s.%s = %v, want %v", name, key, actual, expected)
			}
		}
		check("readOnlyHint", w.readOnly)
		check("destructiveHint", w.destructive)
		check("idempotentHint", w.idempotent)
		check("openWorldHint", w.openWorld)
	}
	if len(want) != 21 {
		t.Fatalf("the annotations table covers %d tools, want 21", len(want))
	}
}

// TestSlice3ClosedSchemasAreEnforcedByTheDecoder is the other half of
// "closed input schemas, enforced by the Go decoder rejecting unknown fields,
// not merely declared" (section 8.2).
//
// A declaration nothing checks is a comment. This calls a tool with an
// invented argument and requires the refusal to NAME it, in a result the
// model can read.
func TestSlice3ClosedSchemasAreEnforcedByTheDecoder(t *testing.T) {
	h := newHarness(t)
	result := h.tool("list_conversations", map[string]any{"folderr": "inbox"})
	if !isError(result) {
		t.Fatal("an invented argument was accepted")
	}
	e := resultError(t, result)
	if e["code"] != "invalid_request" {
		t.Fatalf("the refusal is %q, want invalid_request", e["code"])
	}
	if !strings.Contains(e["message"].(string), "folderr") {
		t.Fatalf("the refusal does not name the argument: %q", e["message"])
	}
}

// TestSlice3EveryToolArgumentIsARouteParameter keeps the facade honest in the
// direction the two-way route table cannot reach: a tool argument that no
// route accepts would be refused by the strict parameter check at call time,
// which is a runtime failure rather than a startup one.
func TestSlice3EveryToolArgumentIsARouteParameter(t *testing.T) {
	for _, tool := range mcp.Tools {
		for _, arg := range tool.Args {
			var reachable bool
			for _, name := range routesOf(tool) {
				route, ok := api.RouteByName(name)
				if !ok {
					t.Fatalf("%s names route %q, which is not in the inventory", tool.Name, name)
				}
				for _, p := range route.Parameters() {
					if p == arg.Name || p == argPathName(arg) {
						reachable = true
					}
				}
			}
			if !reachable {
				t.Errorf("%s.%s is accepted by no route the tool can reach", tool.Name, arg.Name)
			}
		}
	}
}

func routesOf(tool mcp.Tool) []string {
	if len(tool.Routes) > 0 {
		return tool.Routes
	}
	return []string{tool.Route}
}

func argPathName(a mcp.Arg) string {
	if a.PathParam != "" {
		return a.PathParam
	}
	return a.Name
}

// --- helpers -----------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func stringsOf(raw any) []string {
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func tailOf(s string) string {
	if len(s) <= 80 {
		return s
	}
	return "..." + s[len(s)-80:]
}

var _ = json.Marshal

// TestSlice3OutputSchemaAdmitsARefusal is a regression test with a date.
//
// The official `@modelcontextprotocol/sdk` client validates a result's
// `structuredContent` against the tool's declared `outputSchema` and RAISES a
// JSON-RPC error when it does not match. Driving a real client against a real
// server on 2026-09-06 turned every section 8.2 refusal -- which carries
// `{error}` and no `data` -- into `MCP error -32602: Structured content does
// not match the tool's output schema`.
//
// That is precisely the outcome the isError rule exists to prevent: the model
// never sees the `not_found` it could have corrected itself from, because the
// client turned it into a transport failure. So the schema names both shapes.
func TestSlice3OutputSchemaAdmitsARefusal(t *testing.T) {
	for _, tool := range mcp.Tools {
		schema := tool.OutputSchema()
		if len(schema.OneOf) != 2 {
			t.Fatalf("%s's outputSchema does not offer both a success and a refusal shape", tool.Name)
		}
		if _, ok := schema.Properties["error"]; !ok {
			t.Fatalf("%s's outputSchema has no `error` property, so a refusal violates it", tool.Name)
		}
		var sawSuccess, sawRefusal bool
		for _, branch := range schema.OneOf {
			sort.Strings(branch.Required)
			if equalStrings(branch.Required, []string{"data", "next_cursor", "warnings"}) {
				sawSuccess = true
			}
			if equalStrings(branch.Required, []string{"error"}) {
				sawRefusal = true
			}
		}
		if !sawSuccess || !sawRefusal {
			t.Fatalf("%s's outputSchema branches are wrong: success=%v refusal=%v", tool.Name, sawSuccess, sawRefusal)
		}
	}
}

// TestSlice3ResultsSatisfyTheDeclaredOutputSchema checks the same claim from
// the other end: a real success result and a real refusal, both from a running
// server, carry exactly the keys one branch of the schema requires and no
// others.
func TestSlice3ResultsSatisfyTheDeclaredOutputSchema(t *testing.T) {
	h := newHarness(t)
	h.addAccount(addressA)

	success := structured(t, h.tool("list_accounts", nil))
	assertKeys(t, "a successful result", success, []string{"data", "next_cursor", "warnings"})

	refusal := structured(t, h.tool("get_message", map[string]any{"message_id": "msg_nope"}))
	assertKeys(t, "a refusal", refusal, []string{"error"})
}

func assertKeys(t *testing.T, what string, obj map[string]any, want []string) {
	t.Helper()
	var got []string
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("%s carries %v, want exactly %v -- a strict client validates structuredContent against the outputSchema",
			what, got, want)
	}
}
