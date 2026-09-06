package cli_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/cli"
)

// Section 16 Slice 2 test 27, both directions:
//
//	"Every /v1 route parameter is reachable from a CLI flag, and every CLI
//	 command maps to a route -- asserted by a table test over both
//	 inventories, so a route added without a flag fails."
//
// Two tables, written independently: internal/api's route inventory and
// internal/cli's command inventory. Neither is derived from the other, which
// is the only way this test can fail.
//
// Plant: add a query parameter to any route in internal/api/routes.go without
// touching internal/cli/commands.go, and
// TestEveryRouteParameterIsReachableFromTheCLI fails naming it. Delete a
// command's Routes entry and TestEveryRouteHasACommand fails. Planted
// 2026-09-06.
func TestEveryRouteParameterIsReachableFromTheCLI(t *testing.T) {
	// Which commands drive which route.
	byRoute := map[string][]cli.Command{}
	for _, c := range cli.Commands {
		for _, r := range c.Routes {
			byRoute[r] = append(byRoute[r], c)
		}
	}

	for _, r := range api.Routes {
		if _, excused := cli.RoutesWithoutCommand[r.Name]; excused {
			continue
		}
		cmds := byRoute[r.Name]
		if len(cmds) == 0 {
			continue // TestEveryRouteHasACommand reports this
		}
		for _, p := range r.Parameters() {
			// The idempotency key's two transports are the header and the
			// body field; the CLI's --idempotency-key drives the header, and
			// every write command declares it anyway.
			covered := false
			var tried []string
			for _, c := range cmds {
				tried = append(tried, c.Name)
				if c.Covers(p) {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("%s takes %q and no command that drives it supplies or manages it (tried %v)",
					r, p, tried)
			}
		}
	}
}

func TestEveryRouteHasACommand(t *testing.T) {
	driven := map[string]bool{}
	for _, c := range cli.Commands {
		for _, r := range c.Routes {
			driven[r] = true
		}
	}
	for _, r := range api.Routes {
		if driven[r.Name] {
			continue
		}
		why, excused := cli.RoutesWithoutCommand[r.Name]
		if !excused {
			t.Errorf("%s has no CLI command and is not in RoutesWithoutCommand", r)
			continue
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is excused from having a command with no reason given", r)
		}
	}
}

func TestEveryCommandMapsToARealRoute(t *testing.T) {
	for _, c := range cli.Commands {
		if len(c.Routes) == 0 {
			t.Errorf("%s drives no route", c)
			continue
		}
		for _, name := range c.Routes {
			if _, ok := api.RouteByName(name); !ok {
				t.Errorf("%s names route %q, which does not exist", c, name)
			}
		}
	}
}

// The excuse list is closed and honest: it may not excuse a route that a
// command in fact drives, and it may not name a route that does not exist.
func TestTheExcuseListIsAccurate(t *testing.T) {
	driven := map[string]bool{}
	for _, c := range cli.Commands {
		for _, r := range c.Routes {
			driven[r] = true
		}
	}
	for name := range cli.RoutesWithoutCommand {
		if _, ok := api.RouteByName(name); !ok {
			t.Errorf("RoutesWithoutCommand names %q, which is not a route", name)
		}
		if driven[name] {
			t.Errorf("RoutesWithoutCommand excuses %q, but a command drives it", name)
		}
	}
	// Section 11.3 states exactly one exception among /v1 routes -- GET and
	// DELETE /v1/uploads/{id} -- so the list may not grow quietly.
	var v1 []string
	for name := range cli.RoutesWithoutCommand {
		r, _ := api.RouteByName(name)
		if strings.HasPrefix(r.Path, "/v1/") {
			v1 = append(v1, name)
		}
	}
	sort.Strings(v1)
	if strings.Join(v1, ",") != "uploads_delete,uploads_get" {
		t.Errorf("the /v1 routes without a command are %v; section 11.3 states exactly the two upload routes", v1)
	}
}

// Each command's own table must be internally consistent, or the two-way test
// above could be satisfied by an empty string.
func TestCommandTablesAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range cli.Commands {
		if err := c.Validate(); err != nil {
			t.Error(err)
		}
		if seen[c.Name] {
			t.Errorf("%s is declared twice", c)
		}
		seen[c.Name] = true
		if got, ok := cli.CommandByName(c.Name); !ok || got.Name != c.Name {
			t.Errorf("CommandByName(%q) did not find it", c.Name)
		}
		// A flag mentioned in a description must look like a flag: this is
		// what stops "supplied by magic" from passing the two-way test.
		for _, f := range c.FlagsMentioned() {
			if strings.HasPrefix(f, "---") || strings.Contains(f, "=") {
				t.Errorf("%s mentions a malformed flag %q", c, f)
			}
		}
	}
}

// Names are the same words on all three surfaces (section 11.3): mark-read is
// mark_read is POST .../read, and there is no `unreact` and no `read`
// subcommand that could be misread as "read the conversation".
func TestCommandNamesUseTheSameWordsAsTheRoutes(t *testing.T) {
	for _, c := range cli.Commands {
		last := c.Name
		if i := strings.LastIndex(c.Name, " "); i >= 0 {
			last = c.Name[i+1:]
		}
		for _, banned := range []string{"unreact", "react"} {
			if last == banned {
				t.Errorf("%s uses %q; section 11.3 names them add-reaction and remove-reaction", c, banned)
			}
		}
	}
	for _, want := range []string{
		"conversations mark-read", "messages add-reaction", "messages remove-reaction",
	} {
		if _, ok := cli.CommandByName(want); !ok {
			t.Errorf("there is no command %q", want)
		}
	}
	if _, ok := cli.CommandByName("conversations read"); ok {
		t.Error("there is a `conversations read` command, which reads as 'read the conversation'")
	}
}

// Every destructive command in section 11.3's list is marked, so the
// confirmation and the effect sentence are not a per-command decision.
func TestDestructiveCommandsAreMarked(t *testing.T) {
	want := []string{
		"accounts remove", "accounts sign-out", "conversations delete",
		"messages delete", "auth logout", "admin backfill",
	}
	for _, name := range want {
		c, ok := cli.CommandByName(name)
		if !ok {
			t.Errorf("there is no command %q", name)
			continue
		}
		if !c.Destructive {
			t.Errorf("%s is not marked destructive; section 11.3 lists it", c)
		}
	}
}

// Section 11.1's global flags exist and are spelled as the spec spells them.
func TestGlobalFlags(t *testing.T) {
	have := map[string]bool{}
	for _, f := range cli.GlobalFlags() {
		have[f] = true
	}
	for _, want := range []string{
		"--server", "--profile", "--json", "--output", "--timeout",
		"--quiet", "--verbose", "--idempotency-key", "--yes",
	} {
		if !have[want] {
			t.Errorf("global flag %s is missing", want)
		}
	}
}
