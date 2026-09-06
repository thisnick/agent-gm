// Package api serves the REST surface of spec section 7: handlers, DTOs, the
// error envelope, strict parameter rejection and pagination. It contains no
// business logic -- that is internal/core -- and it is the only package that
// knows a route from a tool.
package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Scope is an OAuth scope name (spec section 9.7). It is declared here as a
// plain string so the inventory can name scopes without the API package
// depending on the credential layer.
type Scope string

// The four scopes. `admin` is issued only by the admin bootstrap and is never
// enrollable.
const (
	ScopeNone   Scope = ""
	ScopeRead   Scope = "messages:read"
	ScopeWrite  Scope = "messages:write"
	ScopeDelete Scope = "messages:delete"
	ScopeAdmin  Scope = "admin"
)

// Route is one `/v1` route, declared rather than discovered.
//
// The inventory below is the single source of truth for three separate
// obligations that would otherwise drift apart:
//
//   - **Strict parameter rejection** (spec section 7.1, section 16 Slice 2
//     test 12). Every route rejects an unknown query parameter and an unknown
//     body field with `invalid_request` naming it, and nothing is allowlisted
//     -- including a cache-busting `_=`. A handler that read its parameters
//     ad hoc would have an allowed set that only its own code knew, and the
//     route-by-route test could only sample. Here the allowed set is data,
//     so the test is exhaustive by construction.
//   - **The CLI two-way mapping** (section 16 Slice 2 test 27). Every `/v1`
//     route parameter must be reachable from a CLI flag and every CLI command
//     must map to a route. That test walks this table against the CLI's own
//     command table, so a route added without a flag fails rather than being
//     noticed later.
//   - **The MCP three-way split** (section 8.2, Slice 3). Every route
//     carrying a `messages:*` scope is served as a tool, as a resource, or as
//     a named exclusion. Slice 3's test walks this same table.
//
// A route is therefore never added by writing a handler. It is added here,
// and the handler is hung off it.
type Route struct {
	// Method and Path are the wire form. Path segments in braces are
	// parameters: `/v1/conversations/{conversation_id}`.
	Method string
	Path   string
	// Name is the stable identifier the CLI and the tests use. It is the
	// same word on every surface (spec section 11.3): mark-read is mark_read
	// is POST .../read.
	Name string
	// Scope is what a caller must hold. ScopeNone means the route
	// authenticates by something other than a bearer token -- the admin
	// secret, a refresh token, or a media ticket -- and says so in Notes.
	Scope Scope
	// Query is every query parameter this route accepts. Anything else is
	// invalid_request naming it. Path parameters are NOT listed here.
	Query []string
	// Body is every JSON body field this route accepts. Anything else is
	// invalid_request naming it.
	Body []string
	// IdempotencyKey marks a mutation, which requires a key by way of the
	// Idempotency-Key header or the client_request_id body field (spec
	// section 6.3). There is no `?client_request_id=` query parameter on any
	// route, and one presented as a query parameter is invalid_request like
	// any other unknown parameter (spec section 7.1) -- which is true here
	// by construction, because Query never contains it.
	IdempotencyKey bool
	// Paginated marks a listing: `cursor` and `limit`, newest first, limit
	// defaulting to 50 and capping at 100 (spec section 7.4).
	Paginated bool
	// Notes records anything about the route a reader would otherwise have
	// to reconstruct from the spec.
	Notes string
}

// paging is the two parameters every listing carries.
var paging = []string{"cursor", "limit"}

func withPaging(params ...string) []string {
	return append(append([]string{}, params...), paging...)
}

// messageFilters are the filters shared by the two message listings
// (spec section 7.6).
var messageFilters = []string{
	"direction", "sender", "after", "before",
	"has_attachment", "delivery_state", "include_system",
}

// Routes is the whole `/v1` surface of spec section 7, plus the two
// unversioned routes. Order is the spec's: health and auth, accounts and
// pairing, reads, writes, admin.
var Routes = []Route{
	// --- health -------------------------------------------------------------
	{
		Method: http.MethodGet, Path: "/healthz", Name: "healthz", Scope: ScopeNone,
		Notes: "liveness. Never touches SQLite, so it answers while a migration runs.",
	},
	{
		Method: http.MethodGet, Path: "/v1/health", Name: "health", Scope: ScopeRead,
		Notes: "status describes the server, not the accounts: an account in signed_out or error does not make Agent GM unhealthy.",
	},

	// --- auth ---------------------------------------------------------------
	{
		Method: http.MethodPost, Path: "/v1/auth/admin-session", Name: "auth_admin_session",
		Scope: ScopeNone, Body: []string{"secret", "scopes"},
		Notes: "presents AGENT_GM_ADMIN_SECRET in the body. Mints admin plus the three messaging scopes; `scopes` may only narrow (9.7).",
	},
	{
		Method: http.MethodPost, Path: "/v1/auth/refresh", Name: "auth_refresh",
		Scope: ScopeNone, Body: []string{"refresh_token", "scopes"},
		Notes: "rotates. `scopes` may only narrow relative to what the session was MINTED with; widening is invalid_scope and does not spend the token (9.6).",
	},
	{
		Method: http.MethodGet, Path: "/v1/auth/whoami", Name: "auth_whoami", Scope: ScopeRead,
	},
	{
		Method: http.MethodPost, Path: "/v1/auth/logout", Name: "auth_logout", Scope: ScopeRead,
		Notes: "ends a TOKEN's session. Nothing to do with signing a Google account out (4.7), which is why it is not called sign-out.",
	},

	// --- accounts -----------------------------------------------------------
	{
		Method: http.MethodGet, Path: "/v1/accounts", Name: "accounts_list", Scope: ScopeRead,
		Notes: "every account, whatever its state. google_account is served because the owner needs to tell their accounts apart; it is never an ID and never in a URL.",
	},
	{
		Method: http.MethodGet, Path: "/v1/accounts/{account_id}", Name: "accounts_get", Scope: ScopeRead,
		Notes: "one account plus its google, backfill, sweep and counters blocks. The list route omits those four deliberately (7.5).",
	},
	{
		Method: http.MethodPatch, Path: "/v1/accounts/{account_id}", Name: "accounts_label",
		Scope: ScopeAdmin, Body: []string{"label"},
		Notes: "a human name for a listing. Nothing else is mutable.",
	},
	{
		Method: http.MethodGet, Path: "/v1/accounts/{account_id}/events", Name: "accounts_events",
		Scope: ScopeRead,
		Notes: "SSE. The only streaming route; it carries no message data, so it needs no replay ring and no cursor.",
	},
	{
		Method: http.MethodGet, Path: "/v1/accounts/events", Name: "accounts_events_all", Scope: ScopeRead,
		Notes: "every account's state changes, each tagged.",
	},
	{
		Method: http.MethodPost, Path: "/v1/accounts/{account_id}/reconnect", Name: "accounts_reconnect",
		Scope: ScopeAdmin,
	},
	{
		Method: http.MethodPost, Path: "/v1/accounts/{account_id}/sign-out", Name: "accounts_sign_out",
		Scope: ScopeAdmin, Body: []string{"confirm"},
		Notes: "shreds the session file, keeps every row (4.7). Requires confirm:true.",
	},
	{
		Method: http.MethodDelete, Path: "/v1/accounts/{account_id}", Name: "accounts_remove",
		Scope: ScopeAdmin, Body: []string{"confirm"},
		Notes: "THE ONLY ROUTE THAT DELETES AN ACCOUNT'S DATA (D30). Requires confirm:true; returns the deleted row counts and the effect sentence.",
	},
	{
		Method: http.MethodPost, Path: "/v1/accounts/{account_id}/refresh-cookies",
		Name: "accounts_refresh_cookies", Scope: ScopeAdmin, Body: []string{"cookies"},
		Notes: "a different Google address is pairing_wrong_account and changes nothing (3.2).",
	},

	// --- pairing ------------------------------------------------------------
	{
		Method: http.MethodPost, Path: "/v1/pairing/start", Name: "pairing_start", Scope: ScopeAdmin,
		Body:  []string{"cookies", "device_index", "account_id"},
		Notes: "adds an account, or resumes an existing one. There is one pairing flow, so there is no `method` field; a body carrying one is invalid_request naming it (D19).",
	},
	{
		Method: http.MethodGet, Path: "/v1/pairing/{pairing_id}", Name: "pairing_get", Scope: ScopeAdmin,
	},
	{
		Method: http.MethodDelete, Path: "/v1/pairing/{pairing_id}", Name: "pairing_abandon", Scope: ScopeAdmin,
		Notes: "abandon an in-flight pairing. Leaves nothing behind (16 Slice 2 test 38).",
	},

	// --- reads --------------------------------------------------------------
	{
		Method: http.MethodGet, Path: "/v1/conversations", Name: "conversations_list", Scope: ScopeRead,
		Query: withPaging("account_id", "query", "participant", "folder", "type",
			"unread_only", "group_only", "include_deleted"),
		Paginated: true,
	},
	{
		Method: http.MethodGet, Path: "/v1/conversations/{conversation_id}", Name: "conversations_get",
		Scope: ScopeRead,
		Notes: "the only route that populates peer_typing_until, which is in-memory live state and is always null in a list (7.6).",
	},
	{
		Method: http.MethodGet, Path: "/v1/conversations/{conversation_id}/messages",
		Name: "conversation_messages_list", Scope: ScopeRead,
		Query: withPaging(messageFilters...), Paginated: true,
	},
	{
		Method: http.MethodGet, Path: "/v1/messages", Name: "messages_list", Scope: ScopeRead,
		Query:     withPaging(append([]string{"account_id", "conversation_id"}, messageFilters...)...),
		Paginated: true,
	},
	{
		Method: http.MethodGet, Path: "/v1/messages/{message_id}", Name: "messages_get", Scope: ScopeRead,
	},
	{
		Method: http.MethodGet, Path: "/v1/messages/{message_id}/context", Name: "messages_context",
		Scope: ScopeRead, Query: []string{"before", "after"},
		Notes: "before and after each default to 5 and cap at 100 (7.4).",
	},
	{
		Method: http.MethodGet, Path: "/v1/messages/{message_id}/attachments",
		Name: "message_attachments_list", Scope: ScopeRead,
		Notes: "metadata only. Folded into get_message on MCP (8.2), so it has no tool.",
	},
	{
		Method: http.MethodGet, Path: "/v1/search/messages", Name: "search_messages", Scope: ScopeRead,
		Query: withPaging("q", "account_id", "mode", "conversation_id", "sender",
			"after", "before", "has_attachment"),
		Paginated: true,
		Notes:     "q is required. mode is words|exact -- a search vocabulary, not a switch between destructive behaviours (13.5).",
	},
	{
		Method: http.MethodGet, Path: "/v1/contacts", Name: "contacts_list", Scope: ScopeRead,
		Query: withPaging("account_id", "query", "top"), Paginated: true,
	},
	{
		Method: http.MethodGet, Path: "/v1/attachments/{attachment_id}", Name: "attachments_get",
		Scope: ScopeRead,
		Notes: "metadata plus a download ticket (10.1).",
	},
	{
		Method: http.MethodGet, Path: "/v1/attachments/{attachment_id}/content",
		Name: "attachments_content", Scope: ScopeRead,
		Notes: "bytes. An access token OR a download ticket; the ticket is accepted only in the Authorization header, never as ?t= (10.3).",
	},
	{
		Method: http.MethodGet, Path: "/v1/operations", Name: "operations_list", Scope: ScopeWrite,
		Query:     withPaging("account_id", "kind", "status", "terminal", "after", "before"),
		Paginated: true,
		Notes:     "the caller's own operations. messages:write because that is the scope that creates them.",
	},
	{
		Method: http.MethodGet, Path: "/v1/operations/{operation_id}", Name: "operations_get",
		Scope: ScopeWrite,
		Notes: "another authorization's ID answers not_found with a body byte-identical to a genuinely absent one (6.5).",
	},
	{
		Method: http.MethodGet, Path: "/v1/uploads/{upload_id}", Name: "uploads_get", Scope: ScopeWrite,
		Notes: "the caller's own reservation. No CLI command and no MCP tool, for the same reason (11.3, 8.2).",
	},

	// --- writes -------------------------------------------------------------
	{
		Method: http.MethodPost, Path: "/v1/conversations", Name: "conversations_start", Scope: ScopeWrite,
		Body:           []string{"account_id", "recipients", "name", "client_request_id"},
		IdempotencyKey: true,
		Notes:          "the ONE write whose target is a phone number rather than an ID, so nothing else can imply the account: account_id is required when more than one account exists (7.3).",
	},
	{
		Method: http.MethodPost, Path: "/v1/conversations/{conversation_id}/messages",
		Name: "messages_send", Scope: ScopeWrite,
		Body:           []string{"text", "upload_ids", "reply_to_message_id", "force_rcs", "client_request_id"},
		IdempotencyKey: true,
		Notes:          "upload_ids is an array but currently accepts exactly one element; two is invalid_request naming the limit (10.2).",
	},
	{
		Method: http.MethodPost, Path: "/v1/conversations/{conversation_id}/typing",
		Name: "conversations_typing", Scope: ScopeWrite,
		Notes: "204. Fire-and-forget: no operation and no idempotency key, because it has no lasting effect (D15).",
	},
	{
		Method: http.MethodPost, Path: "/v1/conversations/{conversation_id}/read",
		Name: "conversations_mark_read", Scope: ScopeWrite,
		Body:           []string{"message_id", "client_request_id"},
		IdempotencyKey: true,
	},
	{
		Method: http.MethodPatch, Path: "/v1/conversations/{conversation_id}",
		Name: "conversations_update", Scope: ScopeWrite,
		Body:           []string{"folder", "pinned", "unread", "client_request_id"},
		IdempotencyKey: true,
		Notes:          "returns operation:null and changed:false when already in the requested state, and calls the backend zero times (16 Slice 2 test 16).",
	},
	{
		Method: http.MethodPost, Path: "/v1/messages/{message_id}/reactions", Name: "reactions_add",
		Scope: ScopeWrite, Body: []string{"emoji", "client_request_id"}, IdempotencyKey: true,
		Notes: "ADD, or SWITCH when the owner already has a different reaction (D23). emoji is canonicalised first (3.7).",
	},
	{
		Method: http.MethodDelete, Path: "/v1/messages/{message_id}/reactions/{emoji}",
		Name: "reactions_remove", Scope: ScopeWrite,
		Body:           []string{"client_request_id"},
		IdempotencyKey: true,
		Notes:          "the path segment is canonicalised before matching, so ❤ and ❤️ address the same reaction.",
	},
	{
		Method: http.MethodDelete, Path: "/v1/reactions/{reaction_id}", Name: "reactions_remove_by_id",
		Scope: ScopeWrite, Body: []string{"client_request_id"}, IdempotencyKey: true,
		Notes: "somebody else's reaction is unsupported_capability with reason not_my_reaction.",
	},
	{
		Method: http.MethodPost, Path: "/v1/uploads", Name: "uploads_create", Scope: ScopeWrite,
		Body:           []string{"filename", "mime_type", "size_bytes", "sha256", "client_request_id"},
		IdempotencyKey: true,
	},
	{
		Method: http.MethodDelete, Path: "/v1/uploads/{upload_id}", Name: "uploads_delete", Scope: ScopeWrite,
		Notes: "204. Drops the reservation and its staged bytes. No CLI command and no MCP tool (11.3, 8.2).",
	},
	{
		Method: http.MethodPut, Path: "/v1/uploads/{upload_id}/content", Name: "uploads_content",
		Scope: ScopeNone,
		Notes: "raw bytes, authenticated by the UPLOAD TOKEN rather than the access token. Its redemption re-checks the issuing authorization's messages:write scope and revocation state (10.3).",
	},

	// --- deletes: messages:delete, and only these two -----------------------
	{
		Method: http.MethodDelete, Path: "/v1/messages/{message_id}", Name: "messages_delete",
		Scope: ScopeDelete, Body: []string{"client_request_id"}, IdempotencyKey: true,
		Notes: "carries the effect sentence. There is no other delete and no delete option: a request carrying an action or scope switch is invalid_request naming it (D14).",
	},
	{
		Method: http.MethodDelete, Path: "/v1/conversations/{conversation_id}",
		Name: "conversations_delete", Scope: ScopeDelete,
		Body: []string{"client_request_id"}, IdempotencyKey: true,
		Notes: "carries the effect sentence.",
	},

	// --- admin --------------------------------------------------------------
	{
		Method: http.MethodGet, Path: "/v1/admin/settings", Name: "admin_settings_list", Scope: ScopeAdmin,
		Notes: "effective value, source (default|environment|database), mutability, restart requirement.",
	},
	{
		Method: http.MethodGet, Path: "/v1/admin/settings/{key}", Name: "admin_settings_get", Scope: ScopeAdmin,
	},
	{
		Method: http.MethodPatch, Path: "/v1/admin/settings", Name: "admin_settings_set", Scope: ScopeAdmin,
		Notes: "validates the WHOLE body: any invalid key rejects the request and changes nothing. Its body fields are the settings keys themselves, so they are validated against the settings registry rather than against a fixed list here.",
	},
	{
		Method: http.MethodPost, Path: "/v1/admin/backfill", Name: "admin_backfill", Scope: ScopeAdmin,
		Body:  []string{"account_id", "conversation_id", "confirm"},
		Notes: "with no body it is a full re-backfill of every account -- the most expensive operation Agent GM offers -- so it requires confirm:true (5.4).",
	},
	{
		Method: http.MethodPost, Path: "/v1/admin/backup", Name: "admin_backup", Scope: ScopeAdmin,
		Notes: "the caller does not choose the path (15.2).",
	},
	{
		Method: http.MethodGet, Path: "/v1/admin/audit", Name: "admin_audit", Scope: ScopeAdmin,
		Query:     withPaging("kind", "kind_prefix", "account_id", "authorization_id", "after", "before"),
		Paginated: true,
	},
	{
		Method: http.MethodGet, Path: "/v1/admin/diagnostics", Name: "admin_diagnostics", Scope: ScopeAdmin,
		Query: []string{"account_id"},
		Notes: "the ONLY place raw Google values appear: delivery_state_raw, operations.google_status_raw, CurrentSessionID, the chosen gaia device, compiled and live ConfigVersion (4.1, 7.7).",
	},
}

// RouteByName looks a route up by its stable name.
func RouteByName(name string) (Route, bool) {
	for _, r := range Routes {
		if r.Name == name {
			return r, true
		}
	}
	return Route{}, false
}

// PathParams returns the brace-delimited parameters of a route's path, in
// order. They are never in Query: a path parameter is part of the address,
// and presenting one as a query parameter is an unknown parameter like any
// other.
func (r Route) PathParams() []string {
	var out []string
	rest := r.Path
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			return out
		}
		close := strings.Index(rest[open:], "}")
		if close < 0 {
			return out
		}
		out = append(out, rest[open+1:open+close])
		rest = rest[open+close:]
	}
}

// Parameters is every parameter a caller can supply on this route: path,
// query and body, sorted and deduplicated. It is what the CLI two-way mapping
// test (section 16 Slice 2 test 27) walks.
func (r Route) Parameters() []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range [][]string{r.PathParams(), r.Query, r.Body} {
		for _, p := range set {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// AllowsQuery reports whether p is a query parameter this route accepts.
// Nothing is allowlisted globally: a cache-busting `_` is an unknown
// parameter like any other (spec section 7.1).
func (r Route) AllowsQuery(p string) bool {
	for _, q := range r.Query {
		if q == p {
			return true
		}
	}
	return false
}

// AllowsBody reports whether f is a body field this route accepts.
func (r Route) AllowsBody(f string) bool {
	for _, b := range r.Body {
		if b == f {
			return true
		}
	}
	return false
}

// String renders a route the way an error message should name it.
func (r Route) String() string { return fmt.Sprintf("%s %s", r.Method, r.Path) }
