package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// D38 on the MCP surface: no tool asks a model for an idempotency key, and
// the sentence that used to tell it to invent one now tells it what to do
// when a result is lost.
//
// The catalogue is checked here in the DECLARATION as well as in the served
// JSON (slice3_tools_test.go), because the two can drift and a model reads
// the served one: a tool added later with a hand-written schema would pass a
// declaration-only check and still ship the argument.

// The banned spellings, over every tool's schema and over every description
// and the instructions block. A description that still says "invent a
// client_request_id" is worse than a schema that still takes one: the schema
// would at least be refused, whereas the sentence just makes the model try.
func TestD38NoToolAsksForAnIdempotencyKey(t *testing.T) {
	banned := []string{"client_request_id", "idempotency_key", "Idempotency-Key"}

	for _, tool := range mcp.Tools {
		for _, arg := range tool.Args {
			for _, b := range banned {
				if strings.EqualFold(arg.Name, b) {
					t.Errorf("%s declares the argument %q; D38 removed it", tool.Name, arg.Name)
				}
			}
		}
		for _, b := range banned {
			if strings.Contains(tool.Description, b) {
				t.Errorf("%s's description still mentions %q", tool.Name, b)
			}
			for _, arg := range tool.Args {
				if strings.Contains(arg.Description, b) {
					t.Errorf("%s.%s's description still mentions %q", tool.Name, arg.Name, b)
				}
			}
		}

		// The rendered schema, not only the declaration, because that is
		// what a client is handed.
		raw, err := json.Marshal(tool.InputSchema())
		if err != nil {
			t.Fatalf("%s: rendering the input schema: %v", tool.Name, err)
		}
		for _, b := range banned {
			if strings.Contains(string(raw), b) {
				t.Errorf("%s's rendered input schema contains %q", tool.Name, b)
			}
		}
	}

	for _, b := range banned {
		if strings.Contains(mcp.Instructions, b) {
			t.Errorf("the instructions block still mentions %q", b)
		}
	}
}

// The replacement sentence, asserted for what it actually says rather than
// only for being present. A model that is told to check by operation id and
// to look before resending is a model that does not send the same text twice
// after a dropped connection -- which is the entire behaviour D38 traded the
// key for, so an empty or vague sentence would be a silent regression.
func TestD38TheLostResultSentenceSaysBothThings(t *testing.T) {
	s := mcp.LostResultSentence
	for _, want := range []string{"operation id", "lost", "before sending again"} {
		if !strings.Contains(s, want) {
			t.Errorf("the lost-result sentence does not contain %q: %q", want, s)
		}
	}
	// The instructions block says the same thing in its own words -- it is
	// prose rather than a tool description -- so what is asserted is the
	// INSTRUCTION, not the wording: do not resend, look first.
	if !strings.Contains(mcp.Instructions, "do not send it again") {
		t.Error("the instructions block does not tell a caller not to resend a lost call")
	}
	if !strings.Contains(mcp.Instructions, "Look\nfirst") &&
		!strings.Contains(mcp.Instructions, "Look first") {
		t.Error("the instructions block does not tell a caller to look first")
	}
	if !strings.Contains(mcp.Instructions, "get_operation") {
		t.Error("the instructions block does not name get_operation, which is how status is now checked")
	}
}
