package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/config"
)

// kind is what a flag takes.
type kind int

const (
	kBool kind = iota
	kString
	kInt
	kDuration
)

// where says what a flag or positional supplies on the wire.
type where int

const (
	wLocal where = iota // the CLI's own behaviour; nothing goes on the wire
	wPath               // a path parameter
	wQuery              // a query parameter
	wBody               // a JSON body field
)

// flagDef is one flag of one command. `param` is the route parameter it
// supplies, which is exactly what internal/cli/commands.go's Supplies map
// names; a flag with no param is local (--wait, --all, --file).
type flagDef struct {
	name  string
	kind  kind
	param string
	where where
	help  string
}

// posDef is one positional argument.
type posDef struct {
	name     string
	param    string
	where    where
	required bool
	variadic bool
	// fromFlag names a flag the positional defaults to when omitted:
	// `agm accounts show <acct-id>` defaults to --account (section 11.3).
	fromFlag string
}

// globalDefs are spec section 11.1's flags, available on every command. They
// are built from GlobalFlags() so the inventory and the parser cannot name a
// different set.
var globalDefs = []flagDef{
	{name: "--server", kind: kString, help: "override the configured server"},
	{name: "--profile", kind: kString, help: "select a saved server and authorization profile"},
	{name: "--json", kind: kBool, help: "emit one stable JSON value on stdout"},
	{name: "--output", kind: kString, help: "table (default) | json | jsonl"},
	{name: "--timeout", kind: kDuration, help: "bound the request, and any operation wait"},
	{name: "--quiet", kind: kBool, help: "suppress non-result output"},
	{name: "--verbose", kind: kBool, help: "diagnostic detail on stderr"},
	{name: "--idempotency-key", kind: kString, help: "resume one logical state-changing request"},
	{name: "--yes", kind: kBool, help: "skip the confirmation prompt on a destructive command"},
	{name: "--credentials-file", kind: kString, help: "override the credentials file"},
}

// invocation is one parsed command line.
type invocation struct {
	cmd    *command
	values map[string]string
	set    map[string]bool
	pos    []string
}

func (i *invocation) has(flag string) bool { return i.set[flag] }

func (i *invocation) str(flag string) string { return i.values[flag] }

func (i *invocation) boolean(flag string) bool {
	if !i.set[flag] {
		return false
	}
	v := i.values[flag]
	return v == "" || v == "true"
}

// duration reads a duration flag under the grammar of spec section 11.1:
// an integer plus a unit of s, m, h or d. Anything else is exit 2 NAMING THE
// FLAG, which is the part that makes the message actionable.
func (i *invocation) duration(flag string) (time.Duration, error) {
	if !i.set[flag] {
		return 0, nil
	}
	d, err := config.ParseDuration(i.values[flag])
	if err != nil {
		return 0, &UsageError{Flag: flag, Msg: err.Error()}
	}
	return d, nil
}

func (i *invocation) integer(flag string) (int, error) {
	if !i.set[flag] {
		return 0, nil
	}
	n, err := strconv.Atoi(i.values[flag])
	if err != nil {
		return 0, &UsageError{Flag: flag, Msg: fmt.Sprintf("%q is not a whole number", i.values[flag])}
	}
	return n, nil
}

// positional returns the nth positional, or "".
func (i *invocation) positional(n int) string {
	if n < len(i.pos) {
		return i.pos[n]
	}
	return ""
}

// parseArgs parses one command's arguments.
//
// It is hand-written rather than flag.FlagSet because section 11.3's commands
// interleave positionals and flags -- `agm messages send conv_x --text hi`
// and `agm messages send --text hi conv_x` are the same command -- and
// flag.FlagSet stops at the first non-flag argument. An unknown flag is exit
// 2 naming it, for the same reason section 7.1 refuses an unknown query
// parameter: a misspelled flag that is quietly ignored produces a result that
// looks exactly like a correct one.
func parseArgs(args []string, defs []flagDef) (map[string]string, map[string]bool, []string, error) {
	byName := map[string]flagDef{}
	for _, d := range defs {
		byName[d.name] = d
	}

	values := map[string]string{}
	set := map[string]bool{}
	var pos []string

	for idx := 0; idx < len(args); idx++ {
		arg := args[idx]
		switch {
		case arg == "--":
			pos = append(pos, args[idx+1:]...)
			return values, set, pos, nil
		case arg == "-h" || arg == "--help":
			set["--help"] = true
			continue
		case strings.HasPrefix(arg, "--"):
			name, inline, hasInline := strings.Cut(arg, "=")
			d, ok := byName[name]
			if !ok {
				return nil, nil, nil, &UsageError{
					Flag: name,
					Msg:  "is not a flag of this command; " + suggestFlags(defs),
				}
			}
			if d.kind == kBool {
				if hasInline {
					if inline != "true" && inline != "false" {
						return nil, nil, nil, &UsageError{Flag: name,
							Msg: fmt.Sprintf("takes no value, or true or false, not %q", inline)}
					}
					values[name] = inline
					set[name] = inline == "true"
					continue
				}
				values[name] = "true"
				set[name] = true
				continue
			}
			if hasInline {
				values[name] = inline
				set[name] = true
				continue
			}
			if idx+1 >= len(args) {
				return nil, nil, nil, &UsageError{Flag: name, Msg: "needs a value"}
			}
			idx++
			values[name] = args[idx]
			set[name] = true
		default:
			pos = append(pos, arg)
		}
	}
	return values, set, pos, nil
}

func suggestFlags(defs []flagDef) string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.name)
	}
	sort.Strings(names)
	return "it takes " + strings.Join(names, ", ")
}

// globals are spec section 11.1's flags, resolved.
type globals struct {
	server string
	// timeoutSet distinguishes an explicit --timeout 0s from an absent
	// flag: the first is a deadline that has already passed, the second is
	// the default.
	timeoutSet      bool
	profile         string
	format          string
	timeout         time.Duration
	quiet           bool
	verbose         bool
	idempotencyKey  string
	yes             bool
	credentialsFile string
}

// resolveGlobals reads section 11.1's flags off a parsed invocation.
//
// `--json` is a synonym for `--output json`. `--output` is validated here
// rather than at the point of use, so a misspelled format is exit 2 before a
// request is sent. There is NO exception: `--output` is a format on every
// command, and the file `agm attachments download` writes is `--out`
// (section 11.1).
func resolveGlobals(inv *invocation) (globals, error) {
	var g globals
	g.server = strings.TrimRight(inv.str("--server"), "/")
	g.profile = inv.str("--profile")
	g.quiet = inv.boolean("--quiet")
	g.verbose = inv.boolean("--verbose")
	g.idempotencyKey = inv.str("--idempotency-key")
	g.yes = inv.boolean("--yes")
	g.credentialsFile = inv.str("--credentials-file")

	timeout, err := inv.duration("--timeout")
	if err != nil {
		return g, err
	}
	g.timeout = timeout
	g.timeoutSet = inv.has("--timeout")

	g.format = FormatTable
	if inv.has("--output") {
		switch v := inv.str("--output"); v {
		case FormatTable, FormatJSON, FormatJSONL:
			g.format = v
		default:
			return g, &UsageError{Flag: "--output",
				Msg: fmt.Sprintf("must be table, json or jsonl, not %q", v)}
		}
	}
	if inv.boolean("--json") {
		g.format = FormatJSON
	}
	return g, nil
}
