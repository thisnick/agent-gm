package cli_test

import (
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/cli"
)

// The CLI holds its own table of route method and path, because it is a
// client: it speaks the REST API and nothing else (spec section 11), and
// importing the server's package to learn a URL would make `agm` depend on
// the server compiling. This test is what keeps the duplication a checked one
// -- it walks internal/api's inventory and fails on any drift in either
// direction, so a route renamed or moved fails here rather than at runtime.
func TestTheRouteTableMatchesTheAPI(t *testing.T) {
	table := cli.RouteTableForTest()

	for _, r := range api.Routes {
		got, ok := table[r.Name]
		if !ok {
			t.Errorf("internal/api declares the route %q (%s) and the CLI's table has no "+
				"entry for it", r.Name, r)
			continue
		}
		if got.Method != r.Method || got.Path != r.Path {
			t.Errorf("the CLI addresses %q as %s %s; internal/api serves it at %s %s",
				r.Name, got.Method, got.Path, r.Method, r.Path)
		}
		if got.Idempotent != r.IdempotencyKey {
			t.Errorf("the CLI thinks %q %s an idempotency key; internal/api says it %s",
				r.Name, yesNo(got.Idempotent), yesNo(r.IdempotencyKey))
		}
	}

	known := map[string]bool{}
	for _, r := range api.Routes {
		known[r.Name] = true
	}
	for name := range table {
		if !known[name] {
			t.Errorf("the CLI's route table names %q, which internal/api does not serve", name)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "takes"
	}
	return "does not take"
}
