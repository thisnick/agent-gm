package cli

import (
	"net/http"
	"strings"
)

// route is the wire form of one `/v1` route as the CLI addresses it: a
// method, a path with brace-delimited parameters, and whether it is a
// mutation that needs an idempotency key (spec section 6.3).
//
// It is a SECOND table, deliberately. internal/api's inventory is the
// server's; this one is the client's, and the CLI is a client -- it speaks
// the REST API and nothing else (spec section 11), so importing the server's
// package to learn a URL would make `agm` depend on the server building.
// The two are checked against each other by TestTheRouteTableMatchesTheAPI
// in internal/cli, which walks api.Routes and fails on any drift in either
// direction, so the duplication is a checked one rather than a remembered
// one.
type route struct {
	method     string
	path       string
	idempotent bool
}

// routeTable is every route of spec section 7, generated from
// internal/api/routes.go and held to it by the test named above.
var routeTable = map[string]route{
	"healthz":                    {method: http.MethodGet, path: "/healthz", idempotent: false},
	"health":                     {method: http.MethodGet, path: "/v1/health", idempotent: false},
	"auth_admin_session":         {method: http.MethodPost, path: "/v1/auth/admin-session", idempotent: false},
	"auth_refresh":               {method: http.MethodPost, path: "/v1/auth/refresh", idempotent: false},
	"auth_whoami":                {method: http.MethodGet, path: "/v1/auth/whoami", idempotent: false},
	"auth_logout":                {method: http.MethodPost, path: "/v1/auth/logout", idempotent: false},
	"accounts_list":              {method: http.MethodGet, path: "/v1/accounts", idempotent: false},
	"accounts_get":               {method: http.MethodGet, path: "/v1/accounts/{account_id}", idempotent: false},
	"accounts_label":             {method: http.MethodPatch, path: "/v1/accounts/{account_id}", idempotent: false},
	"accounts_events":            {method: http.MethodGet, path: "/v1/accounts/{account_id}/events", idempotent: false},
	"accounts_events_all":        {method: http.MethodGet, path: "/v1/accounts/events", idempotent: false},
	"accounts_reconnect":         {method: http.MethodPost, path: "/v1/accounts/{account_id}/reconnect", idempotent: false},
	"accounts_sign_out":          {method: http.MethodPost, path: "/v1/accounts/{account_id}/sign-out", idempotent: false},
	"accounts_remove":            {method: http.MethodDelete, path: "/v1/accounts/{account_id}", idempotent: false},
	"accounts_refresh_cookies":   {method: http.MethodPost, path: "/v1/accounts/{account_id}/refresh-cookies", idempotent: false},
	"pairing_start":              {method: http.MethodPost, path: "/v1/pairing/start", idempotent: false},
	"pairing_get":                {method: http.MethodGet, path: "/v1/pairing/{pairing_id}", idempotent: false},
	"pairing_abandon":            {method: http.MethodDelete, path: "/v1/pairing/{pairing_id}", idempotent: false},
	"conversations_list":         {method: http.MethodGet, path: "/v1/conversations", idempotent: false},
	"conversations_get":          {method: http.MethodGet, path: "/v1/conversations/{conversation_id}", idempotent: false},
	"conversation_messages_list": {method: http.MethodGet, path: "/v1/conversations/{conversation_id}/messages", idempotent: false},
	"messages_list":              {method: http.MethodGet, path: "/v1/messages", idempotent: false},
	"messages_get":               {method: http.MethodGet, path: "/v1/messages/{message_id}", idempotent: false},
	"messages_context":           {method: http.MethodGet, path: "/v1/messages/{message_id}/context", idempotent: false},
	"message_attachments_list":   {method: http.MethodGet, path: "/v1/messages/{message_id}/attachments", idempotent: false},
	"search_messages":            {method: http.MethodGet, path: "/v1/search/messages", idempotent: false},
	"contacts_list":              {method: http.MethodGet, path: "/v1/contacts", idempotent: false},
	"attachments_get":            {method: http.MethodGet, path: "/v1/attachments/{attachment_id}", idempotent: false},
	"attachments_content":        {method: http.MethodGet, path: "/v1/attachments/{attachment_id}/content", idempotent: false},
	"operations_list":            {method: http.MethodGet, path: "/v1/operations", idempotent: false},
	"operations_get":             {method: http.MethodGet, path: "/v1/operations/{operation_id}", idempotent: false},
	"uploads_get":                {method: http.MethodGet, path: "/v1/uploads/{upload_id}", idempotent: false},
	"conversations_start":        {method: http.MethodPost, path: "/v1/conversations", idempotent: true},
	"messages_send":              {method: http.MethodPost, path: "/v1/conversations/{conversation_id}/messages", idempotent: true},
	"conversations_typing":       {method: http.MethodPost, path: "/v1/conversations/{conversation_id}/typing", idempotent: false},
	"conversations_mark_read":    {method: http.MethodPost, path: "/v1/conversations/{conversation_id}/read", idempotent: true},
	"conversations_update":       {method: http.MethodPatch, path: "/v1/conversations/{conversation_id}", idempotent: true},
	"reactions_add":              {method: http.MethodPost, path: "/v1/messages/{message_id}/reactions", idempotent: true},
	"reactions_remove":           {method: http.MethodDelete, path: "/v1/messages/{message_id}/reactions/{emoji}", idempotent: true},
	"reactions_remove_by_id":     {method: http.MethodDelete, path: "/v1/reactions/{reaction_id}", idempotent: true},
	"uploads_create":             {method: http.MethodPost, path: "/v1/uploads", idempotent: true},
	"uploads_delete":             {method: http.MethodDelete, path: "/v1/uploads/{upload_id}", idempotent: false},
	"uploads_content":            {method: http.MethodPut, path: "/v1/uploads/{upload_id}/content", idempotent: false},
	"messages_delete":            {method: http.MethodDelete, path: "/v1/messages/{message_id}", idempotent: true},
	"conversations_delete":       {method: http.MethodDelete, path: "/v1/conversations/{conversation_id}", idempotent: true},
	"admin_settings_list":        {method: http.MethodGet, path: "/v1/admin/settings", idempotent: false},
	"admin_settings_get":         {method: http.MethodGet, path: "/v1/admin/settings/{key}", idempotent: false},
	"admin_settings_set":         {method: http.MethodPatch, path: "/v1/admin/settings", idempotent: false},
	"admin_backfill":             {method: http.MethodPost, path: "/v1/admin/backfill", idempotent: false},
	"admin_backup":               {method: http.MethodPost, path: "/v1/admin/backup", idempotent: false},
	"admin_audit":                {method: http.MethodGet, path: "/v1/admin/audit", idempotent: false},
	"admin_diagnostics":          {method: http.MethodGet, path: "/v1/admin/diagnostics", idempotent: false}}

// routeByName looks a route up.
func routeByName(name string) (route, bool) {
	r, ok := routeTable[name]
	return r, ok
}

// pathParams returns the brace-delimited parameters of a path, in order.
func (r route) pathParams() []string {
	var out []string
	rest := r.path
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			return out
		}
		end := strings.Index(rest[open:], "}")
		if end < 0 {
			return out
		}
		out = append(out, rest[open+1:open+end])
		rest = rest[open+end:]
	}
}

// RouteEntry is one row of the CLI's route table, exported for the test that
// holds it to internal/api's inventory.
type RouteEntry struct {
	Method     string
	Path       string
	Idempotent bool
}

// RouteTableForTest exposes the table to internal/cli's tests. It exists so
// the cross-check against internal/api lives in an external test package,
// which is where a test that imports the server's inventory belongs.
func RouteTableForTest() map[string]RouteEntry {
	out := make(map[string]RouteEntry, len(routeTable))
	for name, r := range routeTable {
		out[name] = RouteEntry{Method: r.method, Path: r.path, Idempotent: r.idempotent}
	}
	return out
}
