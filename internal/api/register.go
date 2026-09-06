package api

import (
	"fmt"
	"sort"
)

// RegisterAll hangs a handler off every route in the inventory.
//
// A route is never added by writing a handler: it is added to `Routes` in
// routes.go, and the handler is hung off it here, by name. Two things follow
// from that, and both are the point:
//
//   - `Server.Handle` refuses a name the inventory does not have, so a
//     handler for a route nobody declared is a startup failure rather than an
//     endpoint nobody documented;
//   - this function is checked against the inventory in both directions by
//     TestEveryRouteHasAHandler, so **a route declared and left unserved is a
//     test failure, not a 500 in production**. That is the direction that
//     actually goes wrong: adding a route to a table is easy, and the 500 it
//     produces surfaces on the day an agent first calls it.
//
// The map below is written out one line per route rather than derived from
// anything, so the compiler names an unregistered route's handler and the
// test names an unhandled route's name.
func RegisterAll(s *Server, d *HandlerDeps) error {
	if d.pairings == nil {
		d.pairings = newPairingManager()
	}

	handlers := map[string]Handler{
		// health
		"healthz": d.healthz,
		"health":  d.health,

		// auth
		"auth_admin_session": d.authAdminSession,
		"auth_refresh":       d.authRefresh,
		"auth_whoami":        d.authWhoami,
		"auth_logout":        d.authLogout,

		// accounts
		"accounts_list":            d.accountsList,
		"accounts_get":             d.accountsGet,
		"accounts_label":           d.accountsLabel,
		"accounts_events":          d.accountsEvents,
		"accounts_events_all":      d.accountsEventsAll,
		"accounts_reconnect":       d.accountsReconnect,
		"accounts_sign_out":        d.accountsSignOut,
		"accounts_remove":          d.accountsRemove,
		"accounts_refresh_cookies": d.accountsRefreshCookies,

		// pairing
		"pairing_start":   d.pairingStart,
		"pairing_get":     d.pairingGet,
		"pairing_abandon": d.pairingAbandon,

		// reads
		"conversations_list":         d.conversationsList,
		"conversations_get":          d.conversationsGet,
		"conversation_messages_list": d.conversationMessagesList,
		"messages_list":              d.messagesList,
		"messages_get":               d.messagesGet,
		"messages_context":           d.messagesContext,
		"message_attachments_list":   d.messageAttachmentsList,
		"search_messages":            d.searchMessages,
		"contacts_list":              d.contactsList,
		"attachments_get":            d.attachmentsGet,
		"attachments_content":        d.attachmentContent,
		"operations_list":            d.operationsList,
		"operations_get":             d.operationsGet,
		"uploads_get":                d.uploadsGet,

		// writes
		"conversations_start":     d.conversationsStart,
		"messages_send":           d.messagesSend,
		"conversations_typing":    d.conversationsTyping,
		"conversations_mark_read": d.conversationsMarkRead,
		"conversations_update":    d.conversationsUpdate,
		"reactions_add":           d.reactionsAdd,
		"reactions_remove":        d.reactionsRemove,
		"reactions_remove_by_id":  d.reactionsRemoveByID,
		"uploads_create":          d.uploadsCreate,
		"uploads_delete":          d.uploadsDelete,
		"uploads_content":         d.uploadContent,

		// deletes
		"messages_delete":      d.messagesDelete,
		"conversations_delete": d.conversationsDelete,

		// admin
		"admin_settings_list": d.adminSettingsList,
		"admin_settings_get":  d.adminSettingsGet,
		"admin_settings_set":  d.settingsPatch,
		"admin_backfill":      d.adminBackfill,
		"admin_backup":        d.adminBackup,
		"admin_audit":         d.adminAudit,
		"admin_diagnostics":   d.adminDiagnostics,
	}

	// A deterministic order, so a failure names the same route on every run.
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.Handle(name, handlers[name]); err != nil {
			return fmt.Errorf("registering %q: %w", name, err)
		}
	}

	var missing []string
	for _, route := range Routes {
		if _, ok := handlers[route.Name]; !ok {
			missing = append(missing, route.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("these routes are declared in the inventory and have no handler: %v", missing)
	}
	return nil
}
