package mcp

import (
	"sort"

	"github.com/thisnick/agent-gm/internal/authz"
)

// The twenty-one tools of spec section 8.2: eleven reads, eight writes, two
// deletes.
//
// Each is a facade over the REST route that serves the same data. The
// catalogue below says which route, and where each argument goes on it -- the
// path, the query string or the body -- and nothing else: the filters, the
// validation, the error codes and the DTOs come from the handler, through
// api.Server.Invoke, so the two surfaces cannot answer differently.
//
// **MCP serves the messaging surface, not the whole API.** A `/v1` route has
// a tool if and only if it is something an agent does with messages. The
// exclusions are named in exclusions.go, and section 16 Slice 3 test 16 walks
// the route inventory against tools, resources and that list, so a route in
// none of the three fails rather than being noticed later.

// FreshKeySentence closes every write tool's description, byte for byte
// (spec section 8.2).
//
// It is a constant rather than nine copies because a reviewer's planted
// mutation -- putting a wrong sentence back into a served description -- has
// to be caught by a lint test, and a lint can only compare against one
// authority.
const FreshKeySentence = "Repeating this call with the same client_request_id returns the same operation and sends nothing further. A fresh client_request_id is a different call, not a repeat."

// clientRequestIDDescription is the argument's own description. It ends with
// the same sentence the tool description does, so a model that reads only the
// argument still reads the rule.
const clientRequestIDDescription = "An idempotency key you invent for this call -- any string up to 200 bytes; a UUID is the obvious choice. " + FreshKeySentence

// ArgIn is where an argument goes on the REST route.
type ArgIn int

const (
	// InPath binds a `{...}` segment of the route's path.
	InPath ArgIn = iota
	// InQuery becomes a query parameter.
	InQuery
	// InBody becomes a JSON body field.
	InBody
)

// Arg is one tool argument.
type Arg struct {
	// Name is the argument as a model writes it. It is the same word the
	// route uses (spec section 11.3): there is no renaming layer.
	Name string
	// Description is mandatory. Section 16 Slice 3 test 15 fetches
	// `tools/list` from a running server, counts the arguments and counts
	// the descriptions, and requires them equal.
	Description string
	// Schema is the argument's JSON Schema.
	Schema *Schema
	// Required puts the argument in the input schema's `required`.
	Required bool
	// In is where it goes on the route.
	In ArgIn
	// PathParam overrides the route path parameter name when it differs
	// from Name. It differs exactly once, on `remove_reaction`, whose
	// `emoji` is a path segment on one of its two routes.
	PathParam string
}

// pathParam is the route path parameter this argument binds.
func (a Arg) pathParam() string {
	if a.PathParam != "" {
		return a.PathParam
	}
	return a.Name
}

// Annotations are section 8.2's table, accurate rather than conventional.
//
// They are serialised in full -- no `omitempty` -- because `false` is a claim
// and an absent key is not. A client that had to guess whether a missing
// `destructiveHint` meant "no" or "unstated" would guess wrong on the tools
// where it matters.
type Annotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

// readAnnotations is the first row of section 8.2's table: every read,
// `get_operation`, `get_session`, `get_health` and `list_accounts`.
var readAnnotations = Annotations{ReadOnlyHint: true, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false}

// localWriteAnnotations is `create_upload` and `update_conversation`: a
// change with no effect outside the building. A reservation is purely local,
// and archiving or pinning is a change to the owner's own thread list.
var localWriteAnnotations = Annotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false}

// outboundWriteAnnotations is `send_message`, `start_conversation`,
// `mark_read` and `add_reaction`: something leaves for a real phone.
var outboundWriteAnnotations = Annotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: true}

// removeReactionAnnotations is the one destructive open-world row: removing a
// reaction takes back something the owner sent and the recipient sees it go.
var removeReactionAnnotations = Annotations{ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: true, OpenWorldHint: true}

// deleteAnnotations is the two deletes. They are destructive and **not**
// open-world: Google's delete is delete-for-me, so it changes the owner's own
// copy and nothing leaves the building.
var deleteAnnotations = Annotations{ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: true, OpenWorldHint: false}

// Tool is one entry of the catalogue.
type Tool struct {
	Name        string
	Description string
	// Scope is what a caller must hold to see it in `tools/list` and to call
	// it. Visibility is not authorization: `tools/call` checks again.
	Scope authz.Scope
	// Route is the inventory name of the REST route this tool is a facade
	// over. It is empty on `remove_reaction` alone, which addresses one of
	// two routes depending on which arguments arrived.
	Route string
	// Routes is every route this tool can reach. It is what the two-way
	// table test of section 16 Slice 3 test 16 walks, so a tool that picks
	// its route at call time still accounts for both.
	Routes []string
	Args   []Arg
	Annotations
}

// argByName finds an argument.
func (t Tool) argByName(name string) (Arg, bool) {
	for _, a := range t.Args {
		if a.Name == name {
			return a, true
		}
	}
	return Arg{}, false
}

// RequiresClientRequestID reports whether this is a **write tool** in the
// sense section 8.2 uses the phrase: one that requires `client_request_id`,
// and whose description therefore ends with FreshKeySentence.
//
// `get_operation` is gated on `messages:write` and is not one: it creates
// nothing, so there is nothing to repeat.
func (t Tool) RequiresClientRequestID() bool {
	a, ok := t.argByName("client_request_id")
	return ok && a.Required
}

// InputSchema renders the tool's closed input schema.
func (t Tool) InputSchema() *Schema {
	props := map[string]*Schema{}
	var required []string
	for _, a := range t.Args {
		s := *a.Schema
		s.Description = a.Description
		props[a.Name] = &s
		if a.Required {
			required = append(required, a.Name)
		}
	}
	sort.Strings(required)
	return &Schema{
		Type:                 "object",
		Properties:           props,
		Required:             required,
		AdditionalProperties: boolPtr(false),
	}
}

// argNames is the declared argument set, which decodeArgs checks an incoming
// object against. The closed schema is enforced by the decoder, not merely
// declared (section 8.2).
func (t Tool) argNames() map[string]bool {
	out := make(map[string]bool, len(t.Args))
	for _, a := range t.Args {
		out[a.Name] = true
	}
	return out
}

// --- the closed vocabularies -------------------------------------------------

var (
	folderVocabulary = Vocabulary{
		Name: "folder",
		Values: []VocabularyValue{
			{"inbox", "The ordinary thread list: everything not archived and not spam."},
			{"archived", "Archived by the owner. Still readable and still sendable; it just does not appear in the inbox."},
			{"spam", "Marked as spam."},
		},
	}
	conversationTypeVocabulary = Vocabulary{
		Name: "conversation type",
		Values: []VocabularyValue{
			{"rcs", "An RCS thread. Replies, reactions, typing and delivery receipts are available here."},
			{"sms_mms", "An SMS or MMS thread. Replies are refused, delivery usually stops at `sent`, and read receipts do not arrive."},
			{"unknown", "The thread's kind has not been established yet, usually because nothing has been sent or received in it."},
		},
	}
	directionVocabulary = Vocabulary{
		Name: "direction",
		Values: []VocabularyValue{
			{"incoming", "Messages the owner received."},
			{"outgoing", "Messages the owner sent, from any of their devices."},
		},
	}
	deliveryStateVocabulary = Vocabulary{
		Name: "delivery state",
		Values: []VocabularyValue{
			{"pending", "Queued locally and not yet handed to the phone."},
			{"sending", "Handed to the phone, which has not confirmed it went out."},
			{"sent", "The carrier took it. On an SMS thread and on most groups this is as far as it ever gets."},
			{"delivered", "The recipient's device acknowledged it. An RCS and carrier feature; do not wait for it on SMS."},
			{"read", "The recipient read it. An RCS feature, and only when they have read receipts on."},
			{"failed", "It did not go out. The message carries the reason."},
			{"received", "An incoming message, which has no send state of its own."},
		},
	}
	searchModeVocabulary = Vocabulary{
		Name: "search mode",
		Values: []VocabularyValue{
			{"words", "Match the words of `q` in any order, the way a search box does. This is the default."},
			{"exact", "Match the whole of `q` as a literal phrase."},
		},
	}
)

// --- shared arguments --------------------------------------------------------

func accountIDArg(in ArgIn, extra string) Arg {
	return Arg{
		Name: "account_id", In: in,
		Description: "The Google account to work in, as an `acct_` ID from `list_accounts`. " + extra,
		Schema:      nullableStr(""),
	}
}

func cursorArg() Arg {
	return Arg{
		Name: "cursor", In: InQuery,
		Description: "The `next_cursor` a previous page returned, to fetch the page after it. Send the same arguments you sent for the first page; a cursor replayed with different filters is refused.",
		Schema:      nullableStr(""),
	}
}

func limitArg() Arg {
	return Arg{
		Name: "limit", In: InQuery,
		Description: "How many rows to return. Defaults to 50 and caps at 100.",
		Schema:      nullableInt(""),
	}
}

func clientRequestIDArg() Arg {
	return Arg{
		Name: "client_request_id", In: InBody, Required: true,
		Description: clientRequestIDDescription,
		Schema:      str(""),
	}
}

const emojiDescription = "A bare emoji. Google's own picker offers eleven -- 👍 😍 😂 😮 😥 😠 👎 🤔 😢 😡 ❤️ -- and anything else is sent as a custom reaction that may not render on the recipient's phone. Skin-tone and variation selectors are canonicalised before anything else happens, so ❤ and ❤️ address the same reaction."

// Tools is the whole catalogue, in the order section 8.2 tables it.
var Tools = []Tool{
	// --- reads: messages:read -----------------------------------------------
	{
		Name:  "list_conversations",
		Scope: authz.ScopeMessagesRead,
		Route: "conversations_list",
		Description: "List the owner's Google Messages threads, newest activity first. " +
			"This is how you find a thread when you have a phone number or a name rather than a `conv_` ID. " +
			"Leave `account_id` out to look in every account the server holds; every row carries its own `account_id` either way.",
		Annotations: readAnnotations,
		Args: []Arg{
			accountIDArg(InQuery, "Leave it out to list threads from every account."),
			{Name: "query", In: InQuery, Schema: nullableStr(""),
				Description: "A substring matched against the thread name and against every participant's name and number. Use it when you have a name rather than a number."},
			{Name: "participant", In: InQuery, Schema: nullableStr(""),
				Description: "A phone number that must be in the thread. E.164 (`+15105550123`), bare digits, or a national form are all accepted and matched against the same normalised value."},
			{Name: "folder", In: InQuery,
				Schema:      enumSchema("", folderVocabulary, true),
				Description: "Only threads in this folder: " + folderVocabulary.sentence() + ". Omit it, or pass null, to list every folder."},
			{Name: "type", In: InQuery,
				Schema:      enumSchema("", conversationTypeVocabulary, true),
				Description: "Only threads of this kind: " + conversationTypeVocabulary.sentence() + ". Omit it, or pass null, to list every kind."},
			{Name: "unread_only", In: InQuery, Schema: nullableBool(""),
				Description: "True to list only threads with unread messages."},
			{Name: "group_only", In: InQuery, Schema: nullableBool(""),
				Description: "True to list only group threads, false to list only one-to-one threads, omitted for both."},
			{Name: "include_deleted", In: InQuery, Schema: nullableBool(""),
				Description: "True to include threads the owner has deleted from this account. They are excluded by default."},
			cursorArg(),
			limitArg(),
		},
	},
	{
		Name:  "get_conversation",
		Scope: authz.ScopeMessagesRead,
		Route: "conversations_get",
		Description: "Read one thread: its name, participants, folder, unread and pinned flags, and its `capabilities` -- which say whether a reply, a reaction or a media send will be accepted here before you attempt one. " +
			"This is the only call that fills in `peer_typing_until`, which is live state and is always null in a listing.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "conversation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The thread's Agent GM ID, starting `conv_`. Google's own thread IDs are not accepted in its place."},
		},
	},
	{
		Name:  "list_messages",
		Scope: authz.ScopeMessagesRead,
		Route: "messages_list",
		Description: "List messages, newest first, so the first row is the latest. " +
			"Pass `conversation_id` to read one thread; leave it out with an `account_id` to read everything recent in an account, or leave both out to read across every account.",
		Annotations: readAnnotations,
		Args: []Arg{
			accountIDArg(InQuery, "Leave it out to read across every account."),
			{Name: "conversation_id", In: InQuery, Schema: nullableStr(""),
				Description: "Only messages in this thread, as a `conv_` ID. A `conv_` ID already belongs to one account, so you never need to pass `account_id` beside it."},
			cursorArg(),
			limitArg(),
			{Name: "direction", In: InQuery,
				Schema:      enumSchema("", directionVocabulary, true),
				Description: "Only messages in this direction: " + directionVocabulary.sentence() + ". Omit it, or pass null, for both."},
			{Name: "sender", In: InQuery, Schema: nullableStr(""),
				Description: "Only messages from this sender: a phone number, or the literal `me`. `me` means the owner in whichever account a message belongs to, so it works across accounts as well as within one."},
			{Name: "after", In: InQuery, Schema: nullableStr(""),
				Description: "Only messages sent at or after this instant, as an RFC 3339 timestamp such as `2026-01-31T09:00:00Z`."},
			{Name: "before", In: InQuery, Schema: nullableStr(""),
				Description: "Only messages sent strictly before this instant, as an RFC 3339 timestamp."},
			{Name: "has_attachment", In: InQuery, Schema: nullableBool(""),
				Description: "True for only messages carrying at least one attachment, false for only messages carrying none."},
			{Name: "delivery_state", In: InQuery,
				Schema:      enumSchema("", deliveryStateVocabulary, true),
				Description: "Only messages in this delivery state: " + deliveryStateVocabulary.sentence() + ". Omit it, or pass null, for every state."},
			{Name: "include_system", In: InQuery, Schema: nullableBool(""),
				Description: "True to include system entries -- somebody joining or leaving a group, and similar notices -- which are excluded by default."},
		},
	},
	{
		Name:  "get_message",
		Scope: authz.ScopeMessagesRead,
		Route: "messages_get",
		Description: "Read one message in full: its text, its sender, its delivery state, its reactions, and the metadata of every attachment it carries. " +
			"The attachment metadata is already here, so there is no separate call to list a message's attachments.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "message_id", In: InPath, Required: true, Schema: str(""),
				Description: "The message's Agent GM ID, starting `msg_`. Google's own message IDs are not accepted in its place."},
		},
	},
	{
		Name:  "message_context",
		Scope: authz.ScopeMessagesRead,
		Route: "messages_context",
		Description: "Read the messages around one message, so a search hit can be understood in its thread without paging to find it. " +
			"Returns `before`, the message itself, and `after`.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "message_id", In: InPath, Required: true, Schema: str(""),
				Description: "The message to centre on, as a `msg_` ID."},
			{Name: "before", In: InQuery, Schema: nullableInt(""),
				Description: "How many messages before it to return. Defaults to 5 and caps at 100."},
			{Name: "after", In: InQuery, Schema: nullableInt(""),
				Description: "How many messages after it to return. Defaults to 5 and caps at 100."},
		},
	},
	{
		Name:  "search_messages",
		Scope: authz.ScopeMessagesRead,
		Route: "search_messages",
		Description: "Full-text search over message text. `q` is required. " +
			"It searches every account the server holds unless you pass `account_id`. " +
			"The result carries a `coverage` block saying whether the index is complete, so a thin answer during a backfill is visible rather than mistaken for an empty one.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "q", In: InQuery, Required: true, Schema: str(""),
				Description: "What to search for. Required."},
			accountIDArg(InQuery, "Leave it out to search every account."),
			{Name: "mode", In: InQuery,
				Schema:      enumSchema("", searchModeVocabulary, true),
				Description: "How to match `q`: " + searchModeVocabulary.sentence() + ". Omit it, or pass null, for `words`."},
			{Name: "conversation_id", In: InQuery, Schema: nullableStr(""),
				Description: "Only hits inside this thread, as a `conv_` ID."},
			{Name: "sender", In: InQuery, Schema: nullableStr(""),
				Description: "Only hits from this sender: a phone number, or the literal `me`."},
			{Name: "after", In: InQuery, Schema: nullableStr(""),
				Description: "Only hits sent at or after this RFC 3339 instant."},
			{Name: "before", In: InQuery, Schema: nullableStr(""),
				Description: "Only hits sent strictly before this RFC 3339 instant."},
			{Name: "has_attachment", In: InQuery, Schema: nullableBool(""),
				Description: "True for only hits on messages carrying an attachment, false for only those carrying none."},
			cursorArg(),
			limitArg(),
		},
	},
	{
		Name:  "get_attachment",
		Scope: authz.ScopeMessagesRead,
		Route: "attachments_get",
		Description: "Read one attachment: its filename, type, byte length and dimensions, plus a short-lived download ticket and a ready-made `curl` command that fetches the bytes. " +
			"A supported image small enough to inline comes back as image content in the result; anything larger or of another kind comes back as an `agm://attachments/...` resource link the client can fetch without putting the bytes through the model. " +
			"The download ticket is returned either way.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "attachment_id", In: InPath, Required: true, Schema: str(""),
				Description: "The attachment's Agent GM ID, starting `att_`, as it appears on a message."},
		},
	},
	{
		Name:  "list_contacts",
		Scope: authz.ScopeMessagesRead,
		Route: "contacts_list",
		Description: "List the contacts the phone knows, so a name can be turned into a phone number before searching for a thread.",
		Annotations: readAnnotations,
		Args: []Arg{
			accountIDArg(InQuery, "Leave it out to list contacts from every account."),
			{Name: "query", In: InQuery, Schema: nullableStr(""),
				Description: "A substring matched against the contact's display name and number."},
			{Name: "top", In: InQuery, Schema: nullableBool(""),
				Description: "True to list only the people the owner messages most, which is a much shorter list than every contact."},
			cursorArg(),
			limitArg(),
		},
	},
	{
		Name:  "get_session",
		Scope: authz.ScopeMessagesRead,
		Route: "accounts_get",
		Description: "Read one account's pairing in detail: its `state`, whether the phone is answering, how far its history backfill has got, and its counters. " +
			"Call it when a write was refused and you need to know whether the account is still signed in. " +
			"If the server holds exactly one account you can leave `account_id` out.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "account_id", In: InPath, Schema: nullableStr(""),
				Description: "The account to read, as an `acct_` ID. If the server holds exactly one account you may leave this out and that account is used; with more than one, omitting it is `invalid_request` listing the accounts to choose from."},
		},
	},
	{
		Name:  "get_health",
		Scope: authz.ScopeMessagesRead,
		Route: "health",
		Description: "Read the server's own health and one row per account: how far each backfill has got, whether Google Messages is still the phone's default SMS app (`is_default_sms_app`), and whether the pinned configuration has gone stale (`config_version_stale`). " +
			"Those last two are the settings that break sending, so this is the first call to make when a send fails for no obvious reason.",
		Annotations: readAnnotations,
		Args:        []Arg{},
	},
	{
		Name:  "list_accounts",
		Scope: authz.ScopeMessagesRead,
		Route: "accounts_list",
		Description: "List every Google account this server holds, with its `acct_` ID, the Google address it belongs to, the label the owner chose, and its `state`. " +
			"Call this first when you do not already know which account to work in. " +
			"An account whose `state` is not `connected` is still fully readable; only writes to it are refused.",
		Annotations: readAnnotations,
		Args:        []Arg{},
	},

	// --- writes: messages:write ---------------------------------------------
	{
		Name:  "send_message",
		Scope: authz.ScopeMessagesWrite,
		Route: "messages_send",
		Description: "Send a text, an attachment, or both into an existing thread. **This sends a real SMS, MMS or RCS message to a real person. There is no sandbox and no undo.** " +
			"The result carries a `message_id` and an `operation`; watch the message's `delivery.state` to learn whether it arrived, remembering that on SMS and on groups it usually stops at `sent`. " +
			FreshKeySentence,
		Annotations: outboundWriteAnnotations,
		Args: []Arg{
			{Name: "conversation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The thread to send into, as a `conv_` ID. It already names its own account, so there is no `account_id` here."},
			{Name: "text", In: InBody, Schema: nullableStr(""),
				Description: "The message body. Optional only when `upload_ids` is present, in which case it is the caption."},
			{Name: "upload_ids", In: InBody,
				Schema: &Schema{Type: []string{"array", "null"}, Items: &Schema{Type: "string"}, MaxItems: intPtr(1)},
				Description: "The attachment to send, as a one-element array of an `upl_` ID from `create_upload` whose bytes you have already uploaded. Exactly one element is accepted today; two is refused naming the limit. An upload can be sent only once."},
			{Name: "reply_to_message_id", In: InBody, Schema: nullableStr(""),
				Description: "The `msg_` ID this is a reply to. Replies are an RCS feature: on an `sms_mms` thread this argument is refused with `unsupported_capability` and `reason: \"reply_not_supported\"`, so check the thread's `type` or its `capabilities.reply` first."},
			{Name: "force_rcs", In: InBody, Schema: nullableBool(""),
				Description: "True to refuse to fall back to SMS: if the thread cannot carry RCS the send is refused rather than quietly downgraded."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "start_conversation",
		Scope: authz.ScopeMessagesWrite,
		Route: "conversations_start",
		Description: "Start a thread with one or more phone numbers, or find the one that already exists. " +
			"**If a thread with exactly those recipients already exists you get that thread back and nothing is sent** -- starting is safe, sending is not. " +
			"This is the one write whose target is a phone number rather than an ID, so it is the one that needs `account_id` when the server holds more than one account. " +
			FreshKeySentence,
		Annotations: outboundWriteAnnotations,
		Args: []Arg{
			accountIDArg(InBody, "Required when the server holds more than one account; omitting it then is `invalid_request` listing the accounts to choose from. With exactly one account you may leave it out."),
			{Name: "recipients", In: InBody, Required: true,
				Schema:      &Schema{Type: "array", Items: &Schema{Type: "string"}, MinItems: intPtr(1)},
				Description: "The phone numbers to address, in E.164 form such as `+15105550123`. One recipient is a direct chat; two or more is a group."},
			{Name: "name", In: InBody, Schema: nullableStr(""),
				Description: "A name for the new group. Accepted only for a group; supplying it with a single recipient is refused."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "mark_read",
		Scope: authz.ScopeMessagesWrite,
		Route: "conversations_mark_read",
		Description: "Mark a thread read up to and including one message. This tells the other side, on an RCS thread where they have read receipts on, that the owner has seen it. " +
			FreshKeySentence,
		Annotations: outboundWriteAnnotations,
		Args: []Arg{
			{Name: "conversation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The thread to mark, as a `conv_` ID."},
			{Name: "message_id", In: InBody, Schema: nullableStr(""),
				Description: "Mark read up to and including this `msg_` ID. Omit it to mark the whole thread read up to its latest message."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "add_reaction",
		Scope: authz.ScopeMessagesWrite,
		Route: "reactions_add",
		Description: "React to a message with an emoji. One reaction per person per message: adding a second replaces the first, which is a change the recipient sees. " +
			FreshKeySentence,
		Annotations: outboundWriteAnnotations,
		Args: []Arg{
			{Name: "message_id", In: InPath, Required: true, Schema: str(""),
				Description: "The message to react to, as a `msg_` ID."},
			{Name: "emoji", In: InBody, Required: true, Schema: str(""),
				Description: emojiDescription},
			clientRequestIDArg(),
		},
	},
	{
		Name:   "remove_reaction",
		Scope:  authz.ScopeMessagesWrite,
		Routes: []string{"reactions_remove", "reactions_remove_by_id"},
		Description: "Take back a reaction the owner sent. Address it either by `reaction_id`, or by `message_id` plus `emoji`; supplying neither is `invalid_request`. " +
			"Removing somebody else's reaction is refused. " +
			FreshKeySentence,
		Annotations: removeReactionAnnotations,
		Args: []Arg{
			{Name: "reaction_id", In: InPath, Schema: nullableStr(""),
				Description: "The reaction to remove, as a `react_` ID read from the message. Supply this, or `message_id` plus `emoji`."},
			{Name: "message_id", In: InPath, Schema: nullableStr(""),
				Description: "The message the reaction is on, as a `msg_` ID. Supply this with `emoji`, or supply `reaction_id` instead."},
			{Name: "emoji", In: InPath, PathParam: "emoji", Schema: nullableStr(""),
				Description: "Which reaction to remove, as the same bare emoji that was added. " + emojiDescription},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "update_conversation",
		Scope: authz.ScopeMessagesWrite,
		Route: "conversations_update",
		Description: "Change a thread's own filing: which folder it is in, whether it is pinned, and whether it counts as unread. This changes the owner's copy only and nothing leaves the building. " +
			"A change to a state the thread is already in answers `changed: false` and does nothing. " +
			FreshKeySentence,
		Annotations: localWriteAnnotations,
		Args: []Arg{
			{Name: "conversation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The thread to change, as a `conv_` ID."},
			{Name: "folder", In: InBody,
				Schema:      enumSchema("", folderVocabulary, true),
				Description: "Move the thread to this folder: " + folderVocabulary.sentence() + ". Omit it to leave the folder alone."},
			{Name: "pinned", In: InBody, Schema: nullableBool(""),
				Description: "True to pin the thread to the top of the owner's list, false to unpin it. Omit it to leave pinning alone."},
			{Name: "unread", In: InBody, Schema: nullableBool(""),
				Description: "True to mark the thread unread, false to mark it read. Omit it to leave the flag alone."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "create_upload",
		Scope: authz.ScopeMessagesWrite,
		Route: "uploads_create",
		Description: "Reserve an upload for a file you want to send, and get back a ticket and a ready-made `curl` command. " +
			"Run that command with `FILE` replaced by the path, then pass the `upl_` ID to `send_message` as `upload_ids`. " +
			"An upload is not tied to an account -- the thread you send it into decides that -- and it can be sent only once. " +
			"This is the only way to attach a file; base64 through the model is not accepted. Nothing leaves the building until you send it. " +
			FreshKeySentence,
		Annotations: localWriteAnnotations,
		Args: []Arg{
			{Name: "filename", In: InBody, Schema: nullableStr(""),
				Description: "The file's name, which the recipient sees."},
			{Name: "mime_type", In: InBody, Required: true, Schema: str(""),
				Description: "The file's media type, such as `image/jpeg`. It is checked against the types Google Messages accepts at reservation time, so an unsupported type is refused now rather than at send time."},
			{Name: "size_bytes", In: InBody, Required: true, Schema: &Schema{Type: "integer"},
				Description: "The file's exact byte length. Required. A body that is longer or shorter than this spends the reservation and is refused, so measure the file rather than estimating."},
			{Name: "sha256", In: InBody, Schema: nullableStr(""),
				Description: "The file's SHA-256, lowercase hex. Optional; when given, a body whose digest differs is refused."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "get_operation",
		Scope: authz.ScopeMessagesWrite,
		Route: "operations_get",
		Description: "Read one operation a write of yours created, by the `op_` ID that write returned. " +
			"This is how you learn whether a send that timed out with `phone_not_responding` eventually went: the operation is `pending`, not failed, and the phone may still send it when it wakes. **Poll this; do not resend.** " +
			"It is a read, but it is gated on `messages:write` because it exposes only operations the caller created, and `messages:write` is the scope that creates them.",
		Annotations: readAnnotations,
		Args: []Arg{
			{Name: "operation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The operation to read, as an `op_` ID returned by a write. Another authorization's operation answers `not_found`."},
		},
	},

	// --- deletes: messages:delete -------------------------------------------
	{
		Name:  "delete_message",
		Scope: authz.ScopeMessagesDelete,
		Route: "messages_delete",
		Description: "Delete one message from this account only. **The recipient keeps their copy, always** -- there is no delete-for-everyone and nothing to choose. " +
			FreshKeySentence,
		Annotations: deleteAnnotations,
		Args: []Arg{
			{Name: "message_id", In: InPath, Required: true, Schema: str(""),
				Description: "The message to delete, as a `msg_` ID."},
			clientRequestIDArg(),
		},
	},
	{
		Name:  "delete_conversation",
		Scope: authz.ScopeMessagesDelete,
		Route: "conversations_delete",
		Description: "Delete a whole thread and its messages from this account only. **The other person keeps their copy, always.** This cannot be undone from here. " +
			FreshKeySentence,
		Annotations: deleteAnnotations,
		Args: []Arg{
			{Name: "conversation_id", In: InPath, Required: true, Schema: str(""),
				Description: "The thread to delete, as a `conv_` ID."},
			clientRequestIDArg(),
		},
	},
}

// ToolByName looks a tool up.
func ToolByName(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// routes is every REST route this tool can reach.
func (t Tool) routes() []string {
	if len(t.Routes) > 0 {
		return t.Routes
	}
	if t.Route == "" {
		return nil
	}
	return []string{t.Route}
}

// VisibleTools is the subset of the catalogue an authorization may use.
//
// `tools/list` returns only these, so a model is never invited to attempt
// something that will be refused (spec section 8.2). `tools/call` checks
// again: visibility is not authorization, and a client may call a name it
// learned elsewhere.
func VisibleTools(auth *authz.Authorization) []Tool {
	out := make([]Tool, 0, len(Tools))
	for _, t := range Tools {
		if auth != nil && auth.Scopes.Has(t.Scope) {
			out = append(out, t)
		}
	}
	return out
}
