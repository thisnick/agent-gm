package cli

import (
	"sort"
	"strings"
	"sync"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// command is one typed `agm` command: the words that select it, the flags it
// takes, the positionals it takes, and the route it drives.
//
// The table below is the executable half of internal/cli/commands.go. That
// file is the inventory section 16 Slice 2 test 27 walks against
// internal/api's routes; this one is what actually runs, and
// TestEveryInventoryCommandIsImplemented ties the two together so a command
// cannot be declared without being runnable, or run without being declared.
type command struct {
	// words are the command as typed: {"messages", "add-reaction"}.
	words []string
	// inventory is the Command.Name in commands.go this implements. A local
	// command -- `version`, `completion` -- has none.
	inventory string
	// route is the route the generic executor drives. A command with its own
	// run may name the route it starts with, or none.
	route string
	// createsProfile marks the one command that may name a server this
	// machine holds no profile for: `agm auth login`, which is where a
	// profile comes from. Every other command's --server must match a
	// stored profile (spec section 11.5).
	createsProfile bool
	// connectsItself marks a command that resolves the server and the
	// credential in its own body rather than having dispatch do it first.
	// `agm pair` is the only one: it must diagnose a bad paste before it
	// needs a server, or an owner gets "no server is configured" when what
	// they actually got wrong was the paste (spec section 11.4).
	connectsItself bool
	// flags and pos are what this command accepts, over and above section
	// 11.1's global flags.
	flags []flagDef
	pos   []posDef
	// implied are body fields the SUBCOMMAND supplies rather than the
	// operator: `archive` is folder=archived (section 11.3).
	implied map[string]any
	// destructive prints the effect sentence and requires `y` or --yes.
	destructive bool
	// confirm is the sentence the prompt shows before the call. Where the
	// route returns an `effect`, this is the apierr constant holding the
	// same words -- and the CLI checks the two against each other after the
	// call rather than trusting either alone (section 16 Slice 2 test 28).
	confirm string
	// paginated marks a listing, so --all can walk it.
	paginated bool
	// summary is the one line `agm --help` prints.
	summary string
	// run overrides the generic executor.
	run func(*runner, *invocation) error
}

// Name is the command as typed.
func (c *command) Name() string { return strings.Join(c.words, " ") }

// allFlags is this command's flags plus section 11.1's globals.
func (c *command) allFlags() []flagDef {
	out := make([]flagDef, 0, len(c.flags)+len(globalDefs))
	out = append(out, c.flags...)
	out = append(out, globalDefs...)
	return out
}

// LocalFlagNames are the flags this command takes that supply no route
// parameter, for the help text and for the inventory cross-check.
func (c *command) localFlagNames() []string {
	var out []string
	for _, f := range c.flags {
		if f.param == "" {
			out = append(out, f.name)
		}
	}
	sort.Strings(out)
	return out
}

// Common flag definitions, so a filter that appears on three listings is
// spelled once.
var (
	fAccountQuery = flagDef{name: "--account", kind: kString, param: "account_id", where: wQuery,
		help: "the account to read"}
	fAccountBody = flagDef{name: "--account", kind: kString, param: "account_id", where: wBody,
		help: "the account to act as"}
	fAccountPath = flagDef{name: "--account", kind: kString, param: "account_id", where: wPath,
		help: "the account"}
	fAccountLocal = flagDef{name: "--account", kind: kString,
		help: "the account the positional defaults to"}
	fLimit = flagDef{name: "--limit", kind: kInt, param: "limit", where: wQuery,
		help: "rows per page; 50 by default, 100 at most"}
	fAll = flagDef{name: "--all", kind: kBool,
		help: "walk every page rather than the first"}
	fAfter = flagDef{name: "--after", kind: kString, param: "after", where: wQuery,
		help: "only rows after this point"}
	fBefore = flagDef{name: "--before", kind: kString, param: "before", where: wQuery,
		help: "only rows before this point"}
	fSender = flagDef{name: "--sender", kind: kString, param: "sender", where: wQuery,
		help: "filter by sender"}
	fHasAttachment = flagDef{name: "--has-attachment", kind: kBool, param: "has_attachment", where: wQuery,
		help: "only messages carrying an attachment"}
	fWait = flagDef{name: "--wait", kind: kBool,
		help: "wait for the operation before returning"}
	fWaitFor = flagDef{name: "--wait-for", kind: kString,
		help: "sent (default) | delivered | read | terminal"}
	fQuery = flagDef{name: "--query", kind: kString, param: "query", where: wQuery,
		help: "substring match"}
)

// commandTable is every `agm` command of spec section 11.3. It is built
// lazily because a command's implementation may read the table back --
// `agm completion` prints every command word -- and a package-level variable
// initialised from a function that does so is an initialisation cycle.
var (
	commandTableOnce  sync.Once
	commandTableCache []*command
)

func commandTable() []*command {
	commandTableOnce.Do(func() { commandTableCache = buildCommandTable() })
	return commandTableCache
}

func buildCommandTable() []*command {
	cmds := []*command{
		// --- pairing -------------------------------------------------------
		{
			words: []string{"pair"}, inventory: "pair", route: "pairing_start", connectsItself: true,
			summary: "add an account, or resume one; one command from start to paired",
			flags: []flagDef{
				{name: "--account", kind: kString, param: "account_id", where: wBody,
					help: "resume this account rather than adding one"},
				{name: "--device-index", kind: kInt, param: "device_index", where: wBody,
					help: "which of the account's devices to pair"},
				{name: "--refresh-cookies", kind: kBool,
					help: "re-authenticate an existing pairing without re-pairing"},
				{name: "--paste", kind: kBool,
					help: "read cookies from stdin instead of capturing them"},
				{name: "--paste-file", kind: kString,
					help: "read cookies from a file instead of capturing them"},
			},
			run: (*runner).pair,
		},

		// --- accounts ------------------------------------------------------
		{
			words: []string{"accounts", "list"}, inventory: "accounts list", route: "accounts_list",
			summary: "every account, whatever its state",
			flags:   []flagDef{{name: "--all", kind: kBool, help: "show every column, not the operator view"}},
		},
		{
			words: []string{"accounts", "show"}, inventory: "accounts show", route: "accounts_get",
			summary: "one account plus its google, backfill, sweep and counters blocks",
			pos: []posDef{{name: "<acct-id>", param: "account_id", where: wPath,
				required: true, fromFlag: "--account"}},
			flags: []flagDef{fAccountLocal},
		},
		{
			words: []string{"accounts", "label"}, inventory: "accounts label", route: "accounts_label",
			summary: "give an account a human name",
			pos: []posDef{
				{name: "<acct-id>", param: "account_id", where: wPath, required: true},
				{name: "<label>", param: "label", where: wBody, required: true},
			},
		},
		{
			words: []string{"accounts", "sign-out"}, inventory: "accounts sign-out",
			route: "accounts_sign_out", destructive: true,
			confirm: "shreds this account's Google session file. Every conversation, message and " +
				"attachment Agent GM stored is kept, and writes are refused until you pair again",
			summary: "shred an account's credential, keeping every row",
			pos:     []posDef{{name: "<acct-id>", param: "account_id", where: wPath, required: true}},
			implied: map[string]any{"confirm": true},
		},
		{
			words: []string{"accounts", "remove"}, inventory: "accounts remove",
			route: "accounts_remove", destructive: true,
			confirm: effectAccountRemove,
			summary: "THE ONLY command that deletes an account's history",
			pos:     []posDef{{name: "<acct-id>", param: "account_id", where: wPath, required: true}},
			implied: map[string]any{"confirm": true},
		},
		{
			words: []string{"session"}, inventory: "session", route: "accounts_get",
			summary: "one account's state, or every account's changes with --watch",
			flags: []flagDef{
				fAccountPath,
				{name: "--watch", kind: kBool, help: "stream state changes over SSE"},
			},
			run: (*runner).session,
		},
		{
			words: []string{"reconnect"}, inventory: "reconnect", route: "accounts_reconnect",
			summary: "reconnect an account's long poll",
			flags:   []flagDef{fAccountPath},
		},
		{
			words: []string{"health"}, inventory: "health", route: "health",
			summary: "the server's health; an account in signed_out does not make it unhealthy",
		},

		// --- conversations -------------------------------------------------
		{
			words: []string{"conversations", "list"}, inventory: "conversations list",
			route: "conversations_list", paginated: true,
			summary: "list conversations",
			flags: []flagDef{
				fAccountQuery, fQuery,
				{name: "--participant", kind: kString, param: "participant", where: wQuery,
					help: "filter by a participant's number"},
				{name: "--folder", kind: kString, param: "folder", where: wQuery,
					help: "active | archived | spam_blocked (aliases: inbox | spam)"},
				{name: "--type", kind: kString, param: "type", where: wQuery,
					help: "rcs | sms_mms | unknown"},
				{name: "--unread", kind: kBool, param: "unread_only", where: wQuery,
					help: "only unread conversations"},
				{name: "--group", kind: kBool, param: "group_only", where: wQuery,
					help: "true for groups, false for direct conversations; omit for both"},
				{name: "--pinned", kind: kBool, param: "pinned_only", where: wQuery,
					help: "only pinned conversations"},
				{name: "--after", kind: kString, param: "after", where: wQuery,
					help: "latest activity at or after this RFC 3339 timestamp (inclusive)"},
				{name: "--before", kind: kString, param: "before", where: wQuery,
					help: "latest activity at or before this RFC 3339 timestamp (inclusive)"},
				{name: "--include-deleted", kind: kBool, param: "include_deleted", where: wQuery,
					help: "include conversations deleted from Google Messages"},
				fLimit, fAll,
			},
		},
		{
			words: []string{"conversations", "show"}, inventory: "conversations show",
			route:   "conversations_get",
			summary: "one conversation, the only place peer_typing_until is populated",
			pos:     []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}},
		},
		{
			words: []string{"conversations", "start"}, inventory: "conversations start",
			route:   "conversations_start",
			summary: "start a conversation with one or more numbers",
			pos: []posDef{{name: "<e164>", param: "recipients", where: wBody,
				required: true, variadic: true}},
			flags: []flagDef{
				fAccountBody,
				{name: "--name", kind: kString, param: "name", where: wBody,
					help: "a group name; accepted only for 2+ recipients"},
				fWait, fWaitFor,
			},
			run: (*runner).mutate,
		},
		{
			words: []string{"conversations", "mark-read"}, inventory: "conversations mark-read",
			route:   "conversations_mark_read",
			summary: "mark a conversation read through a message",
			pos:     []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--message", kind: kString, param: "message_id", where: wBody,
					help: "the message to mark read through"},
				fWait, fWaitFor,
			},
			run: (*runner).mutate,
		},
		{
			words: []string{"conversations", "delete"}, inventory: "conversations delete",
			route: "conversations_delete", destructive: true,
			confirm: effectConversationDelete,
			summary: "delete a conversation from your Google Messages account only",
			pos:     []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}},
		},
		{
			words: []string{"conversations", "typing"}, inventory: "conversations typing",
			route:   "conversations_typing",
			summary: "send a typing indicator; fire-and-forget",
			pos:     []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}},
		},

		// --- messages ------------------------------------------------------
		{
			words: []string{"messages", "list"}, inventory: "messages list",
			route: "messages_list", paginated: true,
			summary: "list messages, in one conversation or across every account",
			pos: []posDef{{name: "[<conv-id>]", param: "conversation_id", where: wQuery,
				required: false}},
			flags: []flagDef{
				fAccountQuery,
				{name: "--direction", kind: kString, param: "direction", where: wQuery,
					help: "incoming | outgoing"},
				fSender, fAfter, fBefore, fHasAttachment,
				{name: "--delivery-state", kind: kString, param: "delivery_state", where: wQuery,
					help: "filter by delivery state"},
				{name: "--include-system", kind: kBool, param: "include_system", where: wQuery,
					help: "include system messages"},
				fLimit, fAll,
			},
			run: (*runner).messagesList,
		},
		{
			words: []string{"messages", "show"}, inventory: "messages show", route: "messages_get",
			summary: "one message",
			pos:     []posDef{{name: "<msg-id>", param: "message_id", where: wPath, required: true}},
		},
		{
			words: []string{"messages", "context"}, inventory: "messages context",
			route:   "messages_context",
			summary: "the messages around one message",
			pos:     []posDef{{name: "<msg-id>", param: "message_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--before", kind: kInt, param: "before", where: wQuery,
					help: "how many before; 5 by default, 100 at most"},
				{name: "--after", kind: kInt, param: "after", where: wQuery,
					help: "how many after; 5 by default, 100 at most"},
			},
		},
		{
			words: []string{"messages", "search"}, inventory: "messages search",
			route: "search_messages", paginated: true,
			summary: "search messages",
			pos:     []posDef{{name: "<query>", param: "q", where: wQuery, required: true}},
			flags: []flagDef{
				fAccountQuery,
				{name: "--mode", kind: kString, param: "mode", where: wQuery,
					help: "words (default) | exact"},
				{name: "--conversation", kind: kString, param: "conversation_id", where: wQuery,
					help: "restrict to one conversation"},
				fSender, fAfter, fBefore, fHasAttachment, fLimit, fAll,
			},
		},
		{
			words: []string{"messages", "send"}, inventory: "messages send", route: "messages_send",
			summary: "send a message, with text, one file, or both",
			pos:     []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--text", kind: kString, param: "text", where: wBody, help: "the message text"},
				{name: "--file", kind: kString,
					help: "ONE path; reserve, PUT and send happen in this process"},
				{name: "--reply-to", kind: kString, param: "reply_to_message_id", where: wBody,
					help: "the message this replies to"},
				{name: "--force-rcs", kind: kBool, param: "force_rcs", where: wBody,
					help: "refuse to fall back to SMS"},
				fWait, fWaitFor,
			},
			run: (*runner).messagesSend,
		},
		{
			words: []string{"messages", "delete"}, inventory: "messages delete",
			route: "messages_delete", destructive: true,
			confirm: effectMessageDelete,
			summary: "delete a message from your Google Messages account only",
			pos:     []posDef{{name: "<msg-id>", param: "message_id", where: wPath, required: true}},
		},
		{
			words: []string{"messages", "add-reaction"}, inventory: "messages add-reaction",
			route:   "reactions_add",
			summary: "react to a message, or switch the reaction",
			pos: []posDef{
				{name: "<msg-id>", param: "message_id", where: wPath, required: true},
				{name: "<emoji>", param: "emoji", where: wBody, required: true},
			},
			flags: []flagDef{fWait, fWaitFor},
			run:   (*runner).mutate,
		},
		{
			words: []string{"messages", "remove-reaction"}, inventory: "messages remove-reaction",
			route:   "reactions_remove",
			summary: "remove a reaction, by emoji or by react_ ID",
			pos: []posDef{
				{name: "<msg-id>", param: "message_id", where: wPath, required: false},
				{name: "<emoji>", param: "emoji", where: wPath, required: false},
			},
			flags: []flagDef{
				{name: "--reaction", kind: kString, param: "reaction_id", where: wPath,
					help: "address the reaction by its react_ ID instead"},
				fWait, fWaitFor,
			},
			run: (*runner).removeReaction,
		},

		// --- attachments and contacts --------------------------------------
		{
			words: []string{"attachments", "list"}, inventory: "attachments list",
			route:   "message_attachments_list",
			summary: "one message's attachments, metadata only",
			pos:     []posDef{{name: "<msg-id>", param: "message_id", where: wPath, required: true}},
		},
		{
			words: []string{"attachments", "show"}, inventory: "attachments show",
			route:   "attachments_get",
			summary: "one attachment's metadata and a download ticket",
			pos:     []posDef{{name: "<att-id>", param: "attachment_id", where: wPath, required: true}},
		},
		{
			words: []string{"attachments", "download"}, inventory: "attachments download",
			route:   "attachments_content",
			summary: "download an attachment's bytes",
			pos:     []posDef{{name: "<att-id>", param: "attachment_id", where: wPath, required: true}},
			flags: []flagDef{
				// The file, not the format. It is `--out` and not `--output`
				// because --output is a FORMAT on every command without
				// exception (section 11.1): a global flag whose meaning
				// changes on one command is how a script that set
				// `--output json` once ends up writing an attachment to a
				// file called `json`.
				{name: "--out", kind: kString, help: "the file to write; the attachment's own name by default"},
			},
			run: (*runner).download,
		},
		{
			words: []string{"contacts", "list"}, inventory: "contacts list",
			route: "contacts_list", paginated: true,
			summary: "the contacts Google Messages knows",
			flags: []flagDef{
				fAccountQuery, fQuery,
				{name: "--top", kind: kBool, param: "top", where: wQuery,
					help: "the most-messaged contacts first"},
				fLimit, fAll,
			},
		},

		// --- operations ----------------------------------------------------
		{
			words: []string{"operations", "list"}, inventory: "operations list",
			route: "operations_list", paginated: true,
			summary: "this authorization's operations",
			flags: []flagDef{
				fAccountQuery,
				{name: "--kind", kind: kString, param: "kind", where: wQuery, help: "filter by kind"},
				{name: "--status", kind: kString, param: "status", where: wQuery, help: "filter by status"},
				{name: "--terminal", kind: kBool, param: "terminal", where: wQuery,
					help: "only settled operations"},
				fAfter, fBefore, fLimit, fAll,
			},
		},
		{
			words: []string{"operations", "show"}, inventory: "operations show",
			route:   "operations_get",
			summary: "one operation",
			pos:     []posDef{{name: "<op-id>", param: "operation_id", where: wPath, required: true}},
		},
		{
			words: []string{"operations", "wait"}, inventory: "operations wait",
			route:   "operations_get",
			summary: "poll an operation until it settles",
			pos:     []posDef{{name: "<op-id>", param: "operation_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--for", kind: kString,
					help: "sent (default) | delivered | read | terminal"},
			},
			run: (*runner).operationsWait,
		},

		// --- auth ----------------------------------------------------------
		{
			words: []string{"auth", "login"}, inventory: "auth login",
			route:   "auth_admin_session",
			summary: "exchange the admin secret for a session",
			flags: []flagDef{
				{name: "--admin", kind: kBool, help: "the admin bootstrap path"},
				{name: "--scopes", kind: kString, param: "scopes", where: wBody,
					help: "narrow the session's scopes"},
				{name: "--secret-stdin", kind: kBool,
					help: "read the admin secret from stdin rather than a TTY prompt"},
				{name: "--no-browser", kind: kBool,
					help: "print the URL instead of opening a browser (the OAuth path, Slice 3)"},
			},
			run: (*runner).authLogin, createsProfile: true,
		},
		{
			words: []string{"auth", "logout"}, inventory: "auth logout", route: "auth_logout",
			destructive: true,
			confirm: "revokes this authorization's tokens. It has nothing to do with signing a " +
				"Google account out, which is `agm accounts sign-out`",
			summary: "end this token's session",
			run:     (*runner).authLogout,
		},
		{
			words: []string{"auth", "whoami"}, inventory: "auth whoami", route: "auth_whoami",
			summary: "the authorization this token belongs to, and its scopes",
		},

		// --- admin ---------------------------------------------------------
		{
			words: []string{"admin", "settings", "list"}, inventory: "admin settings list",
			route: "admin_settings_list", summary: "every setting, its value and its source",
		},
		{
			words: []string{"admin", "settings", "get"}, inventory: "admin settings get",
			route: "admin_settings_get", summary: "one setting",
			pos: []posDef{{name: "<key>", param: "key", where: wPath, required: true}},
		},
		{
			words: []string{"admin", "settings", "set"}, inventory: "admin settings set",
			route:   "admin_settings_set",
			summary: "set one or more settings in a single PATCH",
			pos:     []posDef{{name: "<key>=<value>", where: wLocal, required: true, variadic: true}},
			run:     (*runner).settingsSet,
		},
		{
			words: []string{"admin", "audit", "list"}, inventory: "admin audit list",
			route: "admin_audit", paginated: true, summary: "the audit trail",
			flags: []flagDef{
				{name: "--kind", kind: kString, param: "kind", where: wQuery, help: "one audit kind"},
				{name: "--kind-prefix", kind: kString, param: "kind_prefix", where: wQuery,
					help: "every kind under a prefix"},
				fAccountQuery,
				{name: "--authorization", kind: kString, param: "authorization_id", where: wQuery,
					help: "one authorization's rows"},
				fAfter, fBefore, fLimit, fAll,
			},
		},
		{
			words: []string{"admin", "backfill"}, inventory: "admin backfill",
			route: "admin_backfill", destructive: true,
			confirm: "re-backfills EVERY account, which is the most expensive operation Agent GM " +
				"offers. It deletes nothing",
			summary: "re-open backfill for a conversation, an account, or everything",
			flags: []flagDef{
				fAccountBody,
				{name: "--conversation", kind: kString, param: "conversation_id", where: wBody,
					help: "re-backfill one conversation"},
			},
			run: (*runner).backfill,
		},
		{
			words: []string{"admin", "backup"}, inventory: "admin backup", route: "admin_backup",
			summary: "write a standalone SQLite backup; the caller does not choose the path",
		},
		{
			words: []string{"admin", "diagnostics"}, inventory: "admin diagnostics",
			route:   "admin_diagnostics",
			summary: "the raw Google view, the only place raw values appear",
			flags:   []flagDef{fAccountQuery},
		},

		// --- admin: the OAuth surface of section 9.5 -----------------------
		//
		// This is open question OQ-3's answer in code: the owner approves an
		// authorization on a terminal. There is no approval page, because an
		// approval page is a human UI and contradicts non-goal N2.
		{
			words:     []string{"admin", "enrollment-codes", "create"},
			inventory: "admin enrollment-codes create", route: "admin_enrollment_codes_create",
			summary: "issue an enrollment code; it is printed once and never again",
			pos:     []posDef{{name: "<label>", param: "label", where: wBody, required: true}},
			flags: []flagDef{
				{name: "--expires-in", kind: kString, param: "expires_in", where: wBody,
					help: "a duration such as 30m; 1m to 24h, defaulting to oauth.enrollment_default_ttl"},
				{name: "--scopes", kind: kString, param: "scopes", where: wBody,
					help: "replace the default ceiling (messages:read messages:write)"},
				{name: "--allow-scopes", kind: kString, param: "allow_scopes", where: wBody,
					help: "extend the default ceiling; mutually exclusive with --scopes"},
			},
		},
		{
			words:     []string{"admin", "enrollment-codes", "list"},
			inventory: "admin enrollment-codes list", route: "admin_enrollment_codes_list",
			summary: "every enrollment code, without its value",
		},
		{
			words:     []string{"admin", "enrollment-codes", "show"},
			inventory: "admin enrollment-codes show", route: "admin_enrollment_codes_get",
			summary: "one enrollment code",
			pos:     []posDef{{name: "<enroll-id>", param: "enrollment_code_id", where: wPath, required: true}},
		},
		{
			words:     []string{"admin", "enrollment-codes", "revoke"},
			inventory: "admin enrollment-codes revoke", route: "admin_enrollment_codes_revoke",
			destructive: true,
			confirm:     apierr.EffectEnrollmentCodeRevoke,
			summary:     "revoke an enrollment code",
			pos:         []posDef{{name: "<enroll-id>", param: "enrollment_code_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--reason", kind: kString, param: "reason", where: wQuery,
					help: "recorded in the audit row"},
			},
		},
		{
			words:     []string{"admin", "authorization-requests", "list"},
			inventory: "admin authorization-requests list", route: "admin_authorization_requests_list",
			summary: "authorization requests waiting on the owner",
			flags: []flagDef{
				{name: "--status", kind: kString, param: "status", where: wQuery,
					help: "pending | approved | denied | completed"},
			},
		},
		{
			words:     []string{"admin", "authorization-requests", "show"},
			inventory: "admin authorization-requests show", route: "admin_authorization_requests_get",
			summary: "one authorization request",
			pos:     []posDef{{name: "<authreq-id>", param: "authorization_request_id", where: wPath, required: true}},
		},
		{
			words:     []string{"admin", "authorization-requests", "approve"},
			inventory: "admin authorization-requests approve",
			route:     "admin_authorization_requests_approve",
			summary:   "approve an authorization request",
			pos:       []posDef{{name: "<authreq-id>", param: "authorization_request_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--scopes", kind: kString, param: "scopes", where: wBody,
					help: "narrow the browser-selected scopes; widening is refused"},
			},
		},
		{
			words:     []string{"admin", "authorization-requests", "deny"},
			inventory: "admin authorization-requests deny", route: "admin_authorization_requests_deny",
			summary: "deny an authorization request",
			pos:     []posDef{{name: "<authreq-id>", param: "authorization_request_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--reason", kind: kString, param: "reason", where: wBody,
					help: "recorded in the audit row"},
			},
		},
		{
			words:     []string{"admin", "authorizations", "list"},
			inventory: "admin authorizations list", route: "admin_authorizations_list",
			summary: "every credential this server has issued",
			flags: []flagDef{
				{name: "--include-revoked", kind: kBool, param: "include_revoked", where: wQuery,
					help: "include revoked authorizations"},
			},
		},
		{
			words:     []string{"admin", "authorizations", "show"},
			inventory: "admin authorizations show", route: "admin_authorizations_get",
			summary: "one authorization",
			pos:     []posDef{{name: "<auth-id>", param: "authorization_id", where: wPath, required: true}},
		},
		{
			words:     []string{"admin", "authorizations", "revoke"},
			inventory: "admin authorizations revoke", route: "admin_authorizations_revoke",
			destructive: true,
			confirm:     apierr.EffectAuthorizationRevoke,
			summary:     "revoke an authorization",
			pos:         []posDef{{name: "<auth-id>", param: "authorization_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--reason", kind: kString, param: "reason", where: wQuery,
					help: "recorded in the audit row"},
			},
		},
		{
			words:     []string{"admin", "clients", "list"},
			inventory: "admin clients list", route: "admin_clients_list",
			summary: "every dynamically registered client",
		},
		{
			words:     []string{"admin", "clients", "show"},
			inventory: "admin clients show", route: "admin_clients_get",
			summary: "one registration",
			pos:     []posDef{{name: "<client-id>", param: "client_id", where: wPath, required: true}},
		},
		{
			words:     []string{"admin", "clients", "revoke"},
			inventory: "admin clients revoke", route: "admin_clients_revoke",
			destructive: true,
			confirm:     apierr.EffectClientRevoke,
			summary:     "revoke a client registration",
			pos:         []posDef{{name: "<client-id>", param: "client_id", where: wPath, required: true}},
			flags: []flagDef{
				{name: "--reason", kind: kString, param: "reason", where: wQuery,
					help: "recorded in the audit row"},
			},
		},

		// --- local commands, which drive no route --------------------------
		// --- profiles ------------------------------------------------------
		// Local commands: they read and write the credentials file and drive
		// no route, so they answer "what is this machine pointed at" even
		// when the server is unreachable.
		{
			words:   []string{"profiles", "list"},
			summary: "every stored profile, with the active one marked",
			run:     (*runner).profilesList,
		},
		{
			words:   []string{"profiles", "use"},
			summary: "make one profile the active one",
			pos:     []posDef{{name: "<name>", where: wLocal, required: true}},
			run:     (*runner).profilesUse,
		},
		{
			words:   []string{"profiles", "remove"},
			summary: "forget one profile; `agm auth logout` revokes at the server",
			pos:     []posDef{{name: "<name>", where: wLocal, required: true}},
			run:     (*runner).profilesRemove,
		},

		{
			words: []string{"completion"}, summary: "print a shell completion script",
			pos: []posDef{{name: "<shell>", where: wLocal, required: true}},
			run: (*runner).completion,
		},
		{
			words: []string{"version"}, summary: "print the version",
			run: (*runner).version,
		},
	}

	// `agm pair --refresh-cookies` is a second inventory entry over the same
	// typed command, so it is not a row of its own here.

	// The five conversation-state subcommands are one route with the state
	// named as a verb (section 11.3): `archive` is folder=archived. Naming
	// them as verbs is what keeps a `mode` argument off the surface.
	for _, sub := range []struct {
		word    string
		implied map[string]any
		summary string
	}{
		{"archive", map[string]any{"folder": "archived"}, "archive a conversation"},
		{"unarchive", map[string]any{"folder": "active"}, "move a conversation back to active"},
		{"pin", map[string]any{"pinned": true}, "pin a conversation"},
		{"unpin", map[string]any{"pinned": false}, "unpin a conversation"},
		{"mark-unread", map[string]any{"unread": true}, "mark a conversation unread"},
	} {
		cmds = append(cmds, &command{
			words:     []string{"conversations", sub.word},
			inventory: "conversations archive|unarchive|pin|unpin|mark-unread",
			route:     "conversations_update",
			summary:   sub.summary,
			pos: []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath,
				required: true}},
			implied: sub.implied,
			flags:   []flagDef{fWait, fWaitFor},
			run:     (*runner).mutate,
		})
	}

	// Longest first, so `admin settings list` is matched before `admin`.
	sort.SliceStable(cmds, func(i, j int) bool { return len(cmds[i].words) > len(cmds[j].words) })
	return cmds
}

// lookup finds the command the arguments select, and returns the arguments
// that remain. It matches the longest word sequence, so `admin settings get`
// wins over any shorter prefix.
func lookup(args []string) (*command, []string, bool) {
	for _, c := range commandTable() {
		if len(args) < len(c.words) {
			continue
		}
		match := true
		for i, w := range c.words {
			if args[i] != w {
				match = false
				break
			}
		}
		if match {
			return c, args[len(c.words):], true
		}
	}
	return nil, nil, false
}
