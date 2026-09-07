package cli

import (
	"sort"
	"strings"
	"testing"
)

// The inventory in commands.go is what section 16 Slice 2 test 27 walks
// against the route table; the command table in dispatch.go is what actually
// runs. Two tables describing one surface drift unless something says they
// must not, so this is that something.

func TestEveryInventoryCommandIsImplemented(t *testing.T) {
	implemented := map[string]bool{}
	for _, c := range commandTable() {
		if c.inventory != "" {
			implemented[c.inventory] = true
		}
	}
	for _, c := range Commands {
		if c.Name == "pair --refresh-cookies" {
			// One typed command, two inventory entries: `agm pair` covers it
			// with the --refresh-cookies flag.
			continue
		}
		if !implemented[c.Name] {
			t.Errorf("the inventory declares %s and no command in the table implements it", c)
		}
	}
}

func TestEveryImplementedCommandIsInTheInventory(t *testing.T) {
	for _, c := range commandTable() {
		if c.inventory == "" {
			// `agm completion`, `agm version` and the three `agm profiles`
			// commands drive no route and are absent from the inventory on
			// purpose (commands.go): the inventory is walked against the
			// route table, and these touch only the local credentials file.
			local := map[string]bool{
				"completion": true, "version": true,
				"profiles list": true, "profiles use": true, "profiles remove": true,
			}
			if !local[c.Name()] {
				t.Errorf("`agm %s` names no inventory entry", c.Name())
			}
			continue
		}
		if _, ok := CommandByName(c.inventory); !ok {
			t.Errorf("`agm %s` claims to implement %q, which is not in the inventory",
				c.Name(), c.inventory)
		}
	}
}

// Every command in the table drives a route this package knows the wire form
// of, and every route in the table is addressable.
func TestEveryCommandsRouteExists(t *testing.T) {
	for _, c := range commandTable() {
		if c.route == "" {
			continue
		}
		if _, ok := routeByName(c.route); !ok {
			t.Errorf("`agm %s` drives the route %q, which the route table does not know",
				c.Name(), c.route)
		}
	}
}

// A flag the inventory describes must be a flag the command actually takes.
// This is what stops "supplied by --account" in a Supplies description from
// being satisfied by a flag nothing parses.
func TestEveryFlagTheInventoryNamesIsParsed(t *testing.T) {
	for _, inv := range Commands {
		var cmd *command
		for _, c := range commandTable() {
			if c.inventory == inv.Name || (inv.Name == "pair --refresh-cookies" && c.Name() == "pair") {
				cmd = c
				break
			}
		}
		if cmd == nil {
			continue // reported by TestEveryInventoryCommandIsImplemented
		}
		accepted := map[string]bool{}
		for _, f := range cmd.allFlags() {
			accepted[f.name] = true
		}
		for _, f := range inv.FlagsMentioned() {
			for _, part := range strings.Split(f, "/") {
				part = strings.TrimSuffix(part, "'s")
				part = strings.Trim(part, "`'\",.;:")
				if !strings.HasPrefix(part, "--") || len(part) <= 2 {
					continue
				}
				if !accepted[part] {
					t.Errorf("the inventory says %s takes %s, and `agm %s` does not parse it",
						inv, part, cmd.Name())
				}
			}
		}
	}
}

// Every local flag the implementation takes is declared in the inventory's
// LocalFlags, so docs/cli.md is checked against the flags that exist rather
// than the ones somebody remembered.
func TestEveryLocalFlagIsDeclared(t *testing.T) {
	// The global flags of section 11.1 are declared once, in GlobalFlags().
	global := map[string]bool{}
	for _, f := range GlobalFlags() {
		global[f] = true
	}

	for _, cmd := range commandTable() {
		if cmd.inventory == "" {
			continue
		}
		inv, ok := CommandByName(cmd.inventory)
		if !ok {
			continue
		}
		declared := map[string]bool{}
		for f := range inv.LocalFlags {
			declared[f] = true
		}
		// A flag named in a Supplies description is declared too: it is how
		// a route parameter is supplied, which is the other half of the
		// inventory.
		for _, f := range inv.FlagsMentioned() {
			declared[f] = true
		}
		// `agm pair` carries --refresh-cookies, which the second inventory
		// entry declares.
		if cmd.Name() == "pair" {
			if refresh, ok := CommandByName("pair --refresh-cookies"); ok {
				for f := range refresh.LocalFlags {
					declared[f] = true
				}
			}
		}
		var missing []string
		for _, f := range cmd.localFlagNames() {
			if !global[f] && !declared[f] {
				missing = append(missing, f)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("`agm %s` takes %v, which the inventory's LocalFlags does not declare",
				cmd.Name(), missing)
		}
	}
}

// Every command is reachable by the words it is declared with, and no two
// commands claim the same words.
func TestLookupFindsEveryCommand(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commandTable() {
		if seen[c.Name()] {
			t.Errorf("`agm %s` is declared twice", c.Name())
		}
		seen[c.Name()] = true

		found, rest, ok := lookup(append(append([]string{}, c.words...), "trailing"))
		if !ok {
			t.Errorf("lookup does not find `agm %s`", c.Name())
			continue
		}
		if found.Name() != c.Name() {
			t.Errorf("lookup of %v found `agm %s`", c.words, found.Name())
		}
		if len(rest) != 1 || rest[0] != "trailing" {
			t.Errorf("lookup of %v left %v", c.words, rest)
		}
	}
}

// The destructive commands of section 11.3 are destructive in the table too,
// and each has a sentence to show before the call.
func TestDestructiveCommandsCarryASentence(t *testing.T) {
	for _, c := range commandTable() {
		if !c.destructive {
			continue
		}
		if strings.TrimSpace(c.confirm) == "" {
			t.Errorf("`agm %s` is destructive and has no sentence to confirm", c.Name())
		}
	}
	for _, name := range []string{
		"accounts remove", "accounts sign-out", "conversations delete",
		"messages delete", "auth logout", "admin backfill",
	} {
		var found *command
		for _, c := range commandTable() {
			if c.inventory == name {
				found = c
				break
			}
		}
		if found == nil {
			t.Errorf("there is no command implementing %q", name)
			continue
		}
		if !found.destructive {
			t.Errorf("`agm %s` is not marked destructive; section 11.3 lists it", found.Name())
		}
	}
}
