package cli

import (
	"fmt"
	"sort"
	"strings"
)

// Command is one `agm` command, declared rather than discovered, for the same
// reason the route inventory is (internal/api): section 11.3 promises that
// **every `/v1` route parameter has a flag and every `/v1` route has a
// command**, and section 16 Slice 2 test 27 asserts that in both directions.
// A promise checked by a table test needs a table.
//
// The mapping is deliberately explicit about *how* a route parameter is
// supplied, because "there is a flag for it" is not the only honest answer:
// some parameters are positional, some are implied by the subcommand the
// owner typed (`archive` is `folder=archived`), and some the CLI manages on
// the owner's behalf (`cursor`, and the whole `upl_` dance behind `--file`).
// Recording which of those it is keeps the test from being satisfied by a
// flag that does not exist.
type Command struct {
	// Name is the command as typed, with its subcommands:
	// "messages add-reaction".
	Name string
	// Routes are the route names (internal/api's Route.Name) this command
	// drives, in the order it drives them.
	Routes []string
	// Supplies maps a route parameter to how this command supplies it. Every
	// parameter of every route in Routes must appear here or in Managed.
	Supplies map[string]string
	// LocalFlags are the flags this command takes that supply no route
	// parameter -- --wait, --for, --no-browser and the rest. They are
	// declared here for the same reason Supplies is: docs/cli.md and the
	// implementation are checked against one table rather than against each
	// other's memory. A flag that supplies a route parameter belongs in
	// Supplies and may not appear here as well.
	LocalFlags map[string]string
	// Managed lists route parameters the CLI supplies without asking the
	// owner, with the reason. `cursor` is the archetype: `agm ... --all`
	// walks the pages itself, and a human pasting an opaque signed cursor on
	// a command line is not a workflow anyone wants.
	Managed map[string]string
	// Destructive marks a command that prints the route's exact `effect`
	// sentence and requires an interactive `y` or `--yes` (section 11.3).
	Destructive bool
	// Notes records anything a reader would otherwise reconstruct.
	Notes string
}

// The global flags of section 11.1, available on every command.
var globalFlags = []string{
	"--server", "--profile", "--json", "--output", "--timeout",
	"--quiet", "--verbose", "--idempotency-key", "--yes",
	"--credentials-file",
}

// GlobalFlags are section 11.1's flags.
func GlobalFlags() []string { return append([]string(nil), globalFlags...) }

// managedCursor is the reason `cursor` never reaches a flag.
const managedCursor = "the CLI walks pages itself; --all continues to the end. " +
	"An opaque signed cursor is not something a human types"

// Commands is the whole `agm` surface of section 11.3.
//
// `agm completion` and `agm version` are absent on purpose: they drive no
// route, and the two-way test treats a command with no routes as a local
// command rather than as an unmapped one.
var Commands = []Command{
	{
		Name:   "pair",
		Routes: []string{"pairing_start", "pairing_get", "pairing_abandon"},
		Supplies: map[string]string{
			"cookies":      "captured over CDP from the short-lived Chrome profile, or read from --paste/--paste-file. Never a flag: a secret never appears in argv (12.1)",
			"device_index": "--device-index",
			"account_id":   "--account",
			"pairing_id":   "held by the running command; GET is the poll and DELETE is what Ctrl-C issues",
		},
		Notes: "one command from start to paired. It covers POST, GET and DELETE /v1/pairing/* (11.3).",
		LocalFlags: map[string]string{
			"--paste":      "read the cookies from stdin instead of capturing them over CDP",
			"--paste-file": "read the cookies from a file instead of capturing them over CDP",
		},
	},
	{
		Name:   "pair --refresh-cookies",
		Routes: []string{"accounts_refresh_cookies"},
		Supplies: map[string]string{
			"account_id": "--account, which --refresh-cookies requires",
			"cookies":    "the same capture, in a fresh short-lived profile (D33)",
		},
		LocalFlags: map[string]string{
			"--refresh-cookies": "re-authenticate an existing pairing rather than adding an account",
		},
	},

	{
		Name: "accounts list", Routes: []string{"accounts_list"},
		Supplies:   map[string]string{},
		Notes:      "the fast operator view (15.3). --all is a display switch here, not a paging one: the route is not paginated.",
		LocalFlags: map[string]string{"--all": "show every column rather than the operator view"},
	},
	{
		Name: "accounts show", Routes: []string{"accounts_get"},
		Supplies: map[string]string{"account_id": "positional <acct-id>, defaulting to --account"},
	},
	{
		Name: "accounts label", Routes: []string{"accounts_label"},
		Supplies: map[string]string{
			"account_id": "positional <acct-id>",
			"label":      "positional <label>",
		},
	},
	{
		Name: "accounts sign-out", Routes: []string{"accounts_sign_out"}, Destructive: true,
		Supplies: map[string]string{
			"account_id": "positional <acct-id>",
			"confirm":    "the interactive y, or --yes",
		},
		Notes: "keeps every row and refuses writes (4.7). Destructive in the sense that it shreds a credential, not in the sense that it deletes data.",
	},
	{
		Name: "accounts remove", Routes: []string{"accounts_remove"}, Destructive: true,
		Supplies: map[string]string{
			"account_id": "positional <acct-id>",
			"confirm":    "the interactive y, or --yes",
		},
		Notes: "THE ONLY command that deletes an account's history (D30).",
	},

	{
		Name:   "session",
		Routes: []string{"accounts_get", "accounts_events", "accounts_events_all"},
		Supplies: map[string]string{
			"account_id": "--account; omitting it with --watch streams every account's changes",
		},
		Notes:      "--watch consumes the SSE route; without --account it is the all-accounts form (11.3).",
		LocalFlags: map[string]string{"--watch": "stream state changes over SSE"},
	},
	{
		Name: "reconnect", Routes: []string{"accounts_reconnect"},
		Supplies: map[string]string{"account_id": "--account"},
	},
	{
		Name: "health", Routes: []string{"health"}, Supplies: map[string]string{},
	},

	{
		Name: "conversations list", Routes: []string{"conversations_list"},
		Supplies: map[string]string{
			"account_id":      "--account",
			"query":           "--query",
			"participant":     "--participant",
			"folder":          "--folder",
			"type":            "--type",
			"unread_only":     "--unread",
			"group_only":      "--group",
			"include_deleted": "--include-deleted",
			"limit":           "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},
	{
		Name: "conversations show", Routes: []string{"conversations_get"},
		Supplies: map[string]string{"conversation_id": "positional <conv-id>"},
	},
	{
		Name: "conversations start", Routes: []string{"conversations_start"},
		Supplies: map[string]string{
			"account_id":        "--account, required when more than one account exists (7.3)",
			"recipients":        "positional <e164>...",
			"name":              "--name, accepted only for 2+ recipients",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		LocalFlags: map[string]string{
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},
	{
		Name:   "conversations archive|unarchive|pin|unpin|mark-unread",
		Routes: []string{"conversations_update"},
		Supplies: map[string]string{
			"conversation_id":   "positional <conv-id>",
			"folder":            "implied by the subcommand: archive is folder=archived, unarchive is folder=active",
			"pinned":            "implied by the subcommand: pin is pinned=true, unpin is pinned=false",
			"unread":            "implied by the subcommand: mark-unread is unread=true",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		Notes: "five subcommands over one route. Naming the state as a verb is what keeps a `mode` argument off the surface (13.5).",
		LocalFlags: map[string]string{
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},
	{
		Name: "conversations mark-read", Routes: []string{"conversations_mark_read"},
		Supplies: map[string]string{
			"conversation_id":   "positional <conv-id>",
			"message_id":        "--message",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		Notes: "mark-read is mark_read is POST .../read: the same word on all three surfaces (11.3).",
		LocalFlags: map[string]string{
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},
	{
		Name: "conversations delete", Routes: []string{"conversations_delete"}, Destructive: true,
		Supplies: map[string]string{
			"conversation_id":   "positional <conv-id>",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		Notes: "prints the effect sentence taken from the route's response, byte for byte (16 Slice 2 test 28).",
	},
	{
		Name: "conversations typing", Routes: []string{"conversations_typing"},
		Supplies: map[string]string{"conversation_id": "positional <conv-id>"},
	},

	{
		Name:   "messages list",
		Routes: []string{"messages_list", "conversation_messages_list"},
		Supplies: map[string]string{
			"account_id":      "--account",
			"conversation_id": "the optional positional <conv-id>, which also selects the per-conversation route",
			"direction":       "--direction",
			"sender":          "--sender",
			"after":           "--after",
			"before":          "--before",
			"has_attachment":  "--has-attachment",
			"delivery_state":  "--delivery-state",
			"include_system":  "--include-system",
			"limit":           "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		Notes:   "with no conversation ID it is GET /v1/messages across every account, or one with --account (11.3).",
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},
	{
		Name: "messages show", Routes: []string{"messages_get"},
		Supplies: map[string]string{"message_id": "positional <msg-id>"},
	},
	{
		Name: "messages context", Routes: []string{"messages_context"},
		Supplies: map[string]string{
			"message_id": "positional <msg-id>",
			"before":     "--before",
			"after":      "--after",
		},
	},
	{
		Name: "messages search", Routes: []string{"search_messages"},
		Supplies: map[string]string{
			"q":               "positional <query>",
			"account_id":      "--account",
			"mode":            "--mode",
			"conversation_id": "--conversation",
			"sender":          "--sender",
			"after":           "--after",
			"before":          "--before",
			"has_attachment":  "--has-attachment",
			"limit":           "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},
	{
		Name:   "messages send",
		Routes: []string{"messages_send", "uploads_create", "uploads_content"},
		Supplies: map[string]string{
			"conversation_id":     "positional <conv-id>",
			"text":                "--text",
			"reply_to_message_id": "--reply-to",
			"force_rcs":           "--force-rcs",
			"client_request_id":   "--idempotency-key, or minted per invocation",
			"filename":            "derived from --file's path",
			"mime_type":           "sniffed from --file's bytes",
			"size_bytes":          "the size of --file",
			"sha256":              "computed over --file",
		},
		Managed: map[string]string{
			"upload_ids": "--file does the whole reserve / PUT / send sequence in one process and never surfaces an upl_ ID for a human to inspect (11.3). It takes ONE path, matching the one-attachment-per-message limit",
			"upload_id":  "held between the reserve and the PUT within one invocation",
		},
		Notes: "--wait defaults to --wait-for sent, and warns on stderr when delivered or read is asked for on an sms_mms conversation (5.5).",
		LocalFlags: map[string]string{
			"--file":     "ONE path; the reserve, PUT and send happen in this process (11.3)",
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},
	{
		Name: "messages delete", Routes: []string{"messages_delete"}, Destructive: true,
		Supplies: map[string]string{
			"message_id":        "positional <msg-id>",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		Notes: "prints the effect sentence taken from the route's response, byte for byte (16 Slice 2 test 28).",
	},
	{
		Name: "messages add-reaction", Routes: []string{"reactions_add"},
		Supplies: map[string]string{
			"message_id":        "positional <msg-id>",
			"emoji":             "positional <emoji>",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		LocalFlags: map[string]string{
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},
	{
		Name:   "messages remove-reaction",
		Routes: []string{"reactions_remove", "reactions_remove_by_id"},
		Supplies: map[string]string{
			"message_id":        "positional <msg-id>",
			"emoji":             "positional <emoji>",
			"reaction_id":       "--reaction, which addresses it by react_ ID instead",
			"client_request_id": "--idempotency-key, or minted per invocation",
		},
		Notes: "there is no `unreact`: remove-reaction is remove_reaction is DELETE .../reactions/{emoji} (11.3).",
		LocalFlags: map[string]string{
			"--wait":     "wait for the operation before returning",
			"--wait-for": "sent (default), delivered, read or terminal (11.3)",
		},
	},

	{
		Name: "attachments list", Routes: []string{"message_attachments_list"},
		Supplies: map[string]string{"message_id": "positional <msg-id>"},
		Notes:    "metadata for one message's attachments. Folded into get_message on MCP (8.2), but a command exists because every /v1 route has one (11.3).",
	},
	{
		Name: "attachments show", Routes: []string{"attachments_get"},
		Supplies: map[string]string{"attachment_id": "positional <att-id>"},
	},
	{
		Name: "attachments download", Routes: []string{"attachments_content"},
		Supplies: map[string]string{"attachment_id": "positional <att-id>"},
		Notes:    "--out names the file, defaulting to the attachment's own. The ticket goes in the Authorization header, never in the URL (10.3).",
		LocalFlags: map[string]string{
			"--out": "the file to write, defaulting to the attachment's own name",
		},
	},

	{
		Name: "contacts list", Routes: []string{"contacts_list"},
		Supplies: map[string]string{
			"account_id": "--account",
			"query":      "--query",
			"top":        "--top",
			"limit":      "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},

	{
		Name: "operations list", Routes: []string{"operations_list"},
		Supplies: map[string]string{
			"account_id": "--account",
			"kind":       "--kind",
			"status":     "--status",
			"terminal":   "--terminal",
			"after":      "--after",
			"before":     "--before",
			"limit":      "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},
	{
		Name: "operations show", Routes: []string{"operations_get"},
		Supplies: map[string]string{"operation_id": "positional <op-id>"},
	},
	{
		Name: "operations wait", Routes: []string{"operations_get"},
		Supplies: map[string]string{"operation_id": "positional <op-id>"},
		Notes:    "polls GET /v1/operations/{id} on the client side and takes its default bound from the server's operations.wait_timeout (11.3).",
		LocalFlags: map[string]string{
			"--for": "sent (default), delivered, read or terminal (11.3)",
		},
	},

	{
		Name:   "auth login",
		Routes: []string{"auth_admin_session", "auth_refresh"},
		Supplies: map[string]string{
			"secret":        "--secret-stdin or a TTY prompt, for --admin. Never argv (12.1)",
			"scopes":        "--scopes",
			"refresh_token": "held in the credentials file and rotated on use (11.5)",
		},
		LocalFlags: map[string]string{
			"--admin":      "the admin bootstrap path, the only one Slice 2 has",
			"--no-browser": "print the URL instead of opening a browser (the OAuth path, Slice 3)",
		},
	},
	{
		Name: "auth logout", Routes: []string{"auth_logout"}, Destructive: true,
		Supplies: map[string]string{},
	},
	{
		Name: "auth whoami", Routes: []string{"auth_whoami"}, Supplies: map[string]string{},
	},

	{
		Name: "admin settings list", Routes: []string{"admin_settings_list"}, Supplies: map[string]string{},
	},
	{
		Name: "admin settings get", Routes: []string{"admin_settings_get"},
		Supplies: map[string]string{"key": "positional <key>"},
	},
	{
		Name: "admin settings set", Routes: []string{"admin_settings_set"},
		Supplies: map[string]string{},
		Notes:    "its body fields are the settings keys themselves, validated against the settings registry (7.7).",
	},
	{
		Name: "admin audit list", Routes: []string{"admin_audit"},
		Supplies: map[string]string{
			"kind":             "--kind",
			"kind_prefix":      "--kind-prefix",
			"account_id":       "--account",
			"authorization_id": "--authorization",
			"after":            "--after",
			"before":           "--before",
			"limit":            "--limit",
		},
		Managed: map[string]string{"cursor": managedCursor},
		LocalFlags: map[string]string{
			"--all": "walk every page rather than the first",
		},
	},
	{
		Name: "admin backfill", Routes: []string{"admin_backfill"},
		Supplies: map[string]string{
			"account_id":      "--account",
			"conversation_id": "--conversation",
			"confirm":         "the interactive y, or --yes, required when neither --account nor --conversation is given",
		},
		Destructive: true,
		Notes:       "with neither flag it walks every account -- the most expensive operation Agent GM offers (5.4).",
	},
	{
		Name: "admin backup", Routes: []string{"admin_backup"}, Supplies: map[string]string{},
	},
	{
		Name: "admin diagnostics", Routes: []string{"admin_diagnostics"},
		Supplies: map[string]string{"account_id": "--account"},
	},
}

// CommandByName looks a command up by its typed name.
func CommandByName(name string) (Command, bool) {
	for _, c := range Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Covers reports whether this command accounts for a route parameter, either
// by supplying it from the owner's input or by managing it on their behalf.
func (c Command) Covers(param string) bool {
	if _, ok := c.Supplies[param]; ok {
		return true
	}
	_, ok := c.Managed[param]
	return ok
}

// RoutesWithoutCommand are the `/v1` routes section 11.3 states have no
// command, with the reason. It is a closed list: a route absent from both
// this and the command table fails section 16 Slice 2 test 27.
var RoutesWithoutCommand = map[string]string{
	"uploads_get": "`agm messages send --file` performs the whole reserve / PUT / send " +
		"sequence in one process and never surfaces an upl_ ID for a human to inspect or cancel (11.3)",
	"uploads_delete": "the same reason as uploads_get",
	"healthz": "not a /v1 route. It is the liveness probe a supervisor calls, and " +
		"`agm health` answers the question a human is asking",
}

// String renders a command the way help text and an error message name it.
func (c Command) String() string { return "agm " + c.Name }

// FlagsMentioned extracts the `--flag` names out of a Supplies description
// and the LocalFlags keys, so the two-way test can assert a described flag
// looks like one and docs/cli.md is checked against both halves. A flag that
// drives no route parameter -- `--for`, `--no-browser` -- is exactly the kind
// that would otherwise drift out of the page unnoticed.
func (c Command) FlagsMentioned() []string {
	seen := map[string]bool{}
	var out []string
	for name := range c.LocalFlags {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, how := range c.Supplies {
		for _, word := range strings.Fields(how) {
			word = strings.Trim(word, ",.;:'`\"")
			if strings.HasPrefix(word, "--") && len(word) > 2 {
				if !seen[word] {
					seen[word] = true
					out = append(out, word)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// Validate reports a command whose own table is internally inconsistent: a
// parameter both supplied and managed, or an empty description.
func (c Command) Validate() error {
	for p, how := range c.Supplies {
		if strings.TrimSpace(how) == "" {
			return fmt.Errorf("%s supplies %q with no description of how", c, p)
		}
		if _, dup := c.Managed[p]; dup {
			return fmt.Errorf("%s both supplies and manages %q", c, p)
		}
	}
	for p, why := range c.Managed {
		if strings.TrimSpace(why) == "" {
			return fmt.Errorf("%s manages %q with no reason", c, p)
		}
	}
	for flag, what := range c.LocalFlags {
		if strings.TrimSpace(what) == "" {
			return fmt.Errorf("%s takes the local flag %q with no description of what it does", c, flag)
		}
		if !strings.HasPrefix(flag, "--") || len(flag) < 3 {
			return fmt.Errorf("%s declares %q as a local flag, which is not a flag name", c, flag)
		}
		// A flag is either how a route parameter is supplied or a local one.
		// It cannot be both, and a flag that became one should stop being
		// the other in the same edit.
		for _, supplier := range c.primarySupplierFlags() {
			if supplier == flag {
				return fmt.Errorf("%s declares %q both as a local flag and as how it supplies "+
					"a route parameter", c, flag)
			}
		}
	}
	return nil
}

// primarySupplierFlags are the flags a Supplies description NAMES FIRST --
// the convention this table uses for "this is the flag that supplies it":
// `"account_id": "--account"`, `"secret": "--secret-stdin or a TTY prompt"`.
//
// It is deliberately narrower than FlagsMentioned. A description may mention
// a local flag in passing -- `agm session`'s account_id says "omitting it
// with --watch streams every account's changes" -- and that mention does not
// make --watch a supplier. Only the flag a description LEADS with is one, so
// only that one may not also be declared local.
func (c Command) primarySupplierFlags() []string {
	var out []string
	for _, how := range c.Supplies {
		fields := strings.Fields(how)
		if len(fields) == 0 {
			continue
		}
		for _, part := range strings.Split(fields[0], "/") {
			part = strings.TrimSuffix(part, "'s")
			part = strings.Trim(part, ",.;:'`\"")
			if strings.HasPrefix(part, "--") && len(part) > 2 {
				out = append(out, part)
			}
		}
	}
	return out
}
