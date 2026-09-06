package api

import (
	"encoding/json"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/settings"
	"github.com/thisnick/agent-gm/internal/store"
)

// The `/v1/admin/*` subtree of spec section 7.7.

// --- settings ---------------------------------------------------------------

func (d *HandlerDeps) adminSettingsList(r *Request) (*Response, error) {
	all, err := d.Settings.All(r.Ctx)
	if err != nil {
		return nil, err
	}
	return &Response{Data: items(all)}, nil
}

func (d *HandlerDeps) adminSettingsGet(r *Request) (*Response, error) {
	eff, err := d.Settings.Get(r.Ctx, r.Path["key"])
	if err != nil {
		if _, ok := settings.AsError(err); ok {
			return nil, apierr.NotFound("setting")
		}
		return nil, err
	}
	return &Response{Data: eff}, nil
}

// adminSettingsPatch validates the WHOLE body: **any invalid key rejects the
// request and changes nothing**. A partial application would leave an
// operator unable to say what is in force without reading every key back.
//
// The body's field names are the settings keys themselves, so they are
// checked against the registry rather than against a fixed list in the route
// inventory -- which is why this route is one of the three the middleware
// chain cannot pre-check for unknown fields (see content.go).
func (d *HandlerDeps) adminSettingsPatch(r *Request) (*Response, error) {
	if len(r.Body) == 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest, "name at least one setting to change")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &fields); err != nil {
		return nil, apierr.MalformedBody("the body must be a JSON object of setting keys")
	}
	changes, err := d.Settings.Patch(r.Ctx, fields)
	if err != nil {
		if serr, ok := settings.AsError(err); ok {
			e := apierr.New(apierr.CodeInvalidRequest, serr.Error())
			e.Details = map[string]any{"field": serr.Key}
			return nil, e
		}
		return nil, err
	}
	for _, c := range changes {
		d.writeAudit(r.Ctx, audit.Event{
			Kind: audit.KindSettingsChanged, AuthorizationID: r.Auth.ID,
			TargetType: "setting", TargetID: c.Key,
			Result: audit.ResultOK, Source: r.Source,
			Payload: map[string]any{"key": c.Key, "requires_restart": c.RequiresRestart},
		})
	}
	out := make([]settingChangeDTO, 0, len(changes))
	for _, c := range changes {
		out = append(out, settingChangeDTO{
			Key: c.Key, From: c.From, To: c.To, RequiresRestart: c.RequiresRestart,
		})
	}
	return &Response{Data: items(out)}, nil
}

// settingChangeDTO is one accepted key in a PATCH, so an operator can see
// which of the keys they just changed is not yet in force.
type settingChangeDTO struct {
	Key             string         `json:"key"`
	From            settings.Value `json:"from"`
	To              settings.Value `json:"to"`
	RequiresRestart bool           `json:"requires_restart"`
}

// --- backfill ---------------------------------------------------------------

type backfillBody struct {
	AccountID      string `json:"account_id"`
	ConversationID string `json:"conversation_id"`
	Confirm        bool   `json:"confirm"`
}

type backfillDTO struct {
	Accounts      []string `json:"accounts"`
	Conversations []string `json:"conversations"`
	Reopened      int      `json:"reopened"`
}

// adminBackfill re-opens backfill for one conversation, one account, or every
// account.
//
// **With no body it is a full re-backfill of every account -- the most
// expensive operation Agent GM offers -- so it requires `{"confirm": true}`**
// (spec section 5.4). Naming an account or a conversation is a bounded
// request and needs no confirmation, which is the distinction worth drawing:
// the guard is on the blast radius, not on the verb.
func (d *HandlerDeps) adminBackfill(r *Request) (*Response, error) {
	var body backfillBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	if body.AccountID != "" {
		if e := apierr.CheckIDPrefix(body.AccountID, "account_id", store.PrefixAccount); e != nil {
			return nil, e
		}
	}
	if body.ConversationID != "" {
		if e := apierr.CheckIDPrefix(body.ConversationID, "conversation_id", store.PrefixConversation); e != nil {
			return nil, e
		}
	}
	if body.AccountID == "" && body.ConversationID == "" && !body.Confirm {
		e := apierr.New(apierr.CodeInvalidRequest,
			"a backfill of every account re-reads every conversation from every phone; "+
				"send {\"confirm\": true}, or name an account_id or a conversation_id")
		e.Details = map[string]any{"field": "confirm"}
		return nil, e
	}

	out := backfillDTO{Accounts: []string{}, Conversations: []string{}}

	if body.ConversationID != "" {
		c, err := d.Store.Conversation(r.Ctx, body.ConversationID)
		if err != nil {
			return nil, apierr.NotFound("conversation")
		}
		if err := d.reopen(r, c.AccountID, c.ID); err != nil {
			return nil, err
		}
		out.Conversations = append(out.Conversations, c.ID)
		out.Accounts = append(out.Accounts, c.AccountID)
		out.Reopened = 1
		return &Response{Data: out}, nil
	}

	rows, err := d.Store.Accounts(r.Ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if body.AccountID != "" && row.ID != body.AccountID {
			continue
		}
		convs, err := d.Store.Conversations(r.Ctx, store.ConversationFilter{AccountID: row.ID})
		if err != nil {
			return nil, err
		}
		for _, c := range convs {
			if err := d.reopen(r, row.ID, c.ID); err != nil {
				return nil, err
			}
			out.Reopened++
		}
		out.Accounts = append(out.Accounts, row.ID)
	}
	if body.AccountID != "" && len(out.Accounts) == 0 {
		return nil, apierr.NotFound("account")
	}
	return &Response{Data: out}, nil
}

// reopen clears one conversation's backfill cursor so the next backfill pass
// starts from the top. It writes no messages: re-ingesting is idempotent
// (spec section 5.4), so re-opening is safe and re-reading is the whole point.
func (d *HandlerDeps) reopen(r *Request, accountID, conversationID string) error {
	return d.Store.SetBackfillState(r.Ctx, store.BackfillState{
		ConversationID: conversationID,
		AccountID:      accountID,
		Complete:       false,
	})
}

// --- backup -----------------------------------------------------------------

type backupDTO struct {
	Path   string   `json:"path"`
	Kept   []string `json:"kept"`
	Pruned []string `json:"pruned"`
}

// adminBackup writes a snapshot and then prunes.
//
// **The caller does not choose the path** (spec section 15.2): the snapshot
// goes to `<data_dir>/backups/agent-gm-<timestamp>-<id>.sqlite3` and is taken
// inside the database's own transaction, so it is a consistent point in time
// with no `-wal` sidecar and it opens on its own -- which is the property a
// restore depends on.
//
// After a successful backup the newest `backup.keep` snapshots are kept and
// older ones removed, **each removal audited as `admin.backup_pruned`**. A
// file that will not delete is logged and left rather than failing a backup
// that had already succeeded: the snapshot the caller asked for exists, and a
// permission problem on an old one is a separate matter.
func (d *HandlerDeps) adminBackup(r *Request) (*Response, error) {
	path, err := d.Store.Backup(r.Ctx, d.DataDir)
	if err != nil {
		return nil, err
	}
	d.writeAudit(r.Ctx, audit.Event{
		Kind: audit.KindAdminBackup, AuthorizationID: r.Auth.ID,
		TargetType: "backup", TargetID: path,
		Result: audit.ResultOK, Source: r.Source,
	})

	keep := int(d.settingInt(r.Ctx, "backup.keep", 7))
	pruned, err := d.Store.PruneBackups(d.DataDir, keep)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(pruned))
	for _, p := range pruned {
		result := audit.ResultOK
		if p.Err != nil {
			result = audit.ResultFailed
			if d.Log != nil {
				d.Log.Warn("pruning a backup failed", "path", p.Path, "error", p.Err.Error())
			}
		} else {
			removed = append(removed, p.Path)
		}
		d.writeAudit(r.Ctx, audit.Event{
			Kind: audit.KindAdminBackupPruned, AuthorizationID: r.Auth.ID,
			TargetType: "backup", TargetID: p.Path,
			Result: result, Source: r.Source,
		})
	}
	kept, err := d.Store.ListBackups(d.DataDir)
	if err != nil {
		return nil, err
	}
	return &Response{Data: backupDTO{Path: path, Kept: kept, Pruned: removed}}, nil
}

// --- audit ------------------------------------------------------------------

type auditDTO struct {
	ID              string         `json:"id"`
	Kind            string         `json:"kind"`
	AccountID       *string        `json:"account_id"`
	AuthorizationID *string        `json:"authorization_id"`
	TargetType      *string        `json:"target_type"`
	TargetID        *string        `json:"target_id"`
	Result          string         `json:"result"`
	Source          string         `json:"source"`
	Payload         map[string]any `json:"payload"`
	CreatedAt       *string        `json:"created_at"`
}

func auditFrom(e store.AuditEvent) auditDTO {
	payload := map[string]any{}
	if e.PayloadJSON != "" {
		_ = json.Unmarshal([]byte(e.PayloadJSON), &payload)
	}
	return auditDTO{
		ID:              e.ID,
		Kind:            e.Kind,
		AccountID:       nullable(e.AccountID),
		AuthorizationID: nullable(e.AuthorizationID),
		TargetType:      nullable(e.TargetType),
		TargetID:        nullable(e.TargetID),
		Result:          e.Result,
		Source:          e.Source,
		Payload:         payload,
		CreatedAt:       rfc3339(e.CreatedAtMS),
	}
}

func (d *HandlerDeps) adminAudit(r *Request) (*Response, error) {
	p, e := d.paging(r, store.EndpointAudit)
	if e != nil {
		return nil, e
	}
	after, e := timeParam(r.Query, "after")
	if e != nil {
		return nil, e
	}
	before, e := timeParam(r.Query, "before")
	if e != nil {
		return nil, e
	}
	accountID, e := queryID(r.Query, "account_id", store.PrefixAccount)
	if e != nil {
		return nil, e
	}
	rows, err := d.Store.ListAuditEvents(r.Ctx, store.AuditQuery{
		Kind:            r.Query["kind"],
		KindPrefix:      r.Query["kind_prefix"],
		AccountID:       accountID,
		AuthorizationID: r.Query["authorization_id"],
		AfterMS:         after,
		BeforeMS:        before,
		Cursor:          p.Cursor,
		Limit:           p.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]auditDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, auditFrom(e))
	}
	cursor, cErr := nextCursor(d, p, rows)
	if cErr != nil {
		return nil, cErr
	}
	return &Response{Data: items(out), NextCursor: cursor}, nil
}

// --- diagnostics ------------------------------------------------------------

// diagnosticsAccountDTO is the raw Google view of one account.
//
// **This is the ONLY place raw Google values appear** (spec sections 4.1,
// 7.7): `delivery_state_raw`, `operations.google_status_raw`,
// `CurrentSessionID`, the gaia device that was chosen, and the compiled and
// live `ConfigVersion`. Every one of them is a `gmproto` internal that means
// nothing to a caller and everything to an owner diagnosing a fault, which is
// why they are here and admin-only rather than on the public DTOs.
type diagnosticsAccountDTO struct {
	AccountID             string             `json:"account_id"`
	State                 string             `json:"state"`
	CurrentSessionID      string             `json:"current_session_id"`
	GaiaDeviceRegUUID     string             `json:"gaia_dest_reg_uuid"`
	GaiaDeviceLastSeenAt  *string            `json:"gaia_device_last_seen_at"`
	ConfigVersionCompiled string             `json:"config_version_compiled"`
	ConfigVersionLive     string             `json:"config_version_live"`
	DroppedEvents         uint64             `json:"dropped_events"`
	UnknownEvents         uint64             `json:"unknown_events"`
	RecentMessages        []diagMessageDTO   `json:"recent_messages"`
	RecentOperations      []diagOperationDTO `json:"recent_operations"`
}

type diagMessageDTO struct {
	ID               string `json:"id"`
	DeliveryState    string `json:"delivery_state"`
	DeliveryStateRaw int32  `json:"delivery_state_raw"`
	Kind             string `json:"kind"`
}

type diagOperationDTO struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	GoogleStatusRaw int32  `json:"google_status_raw"`
}

// diagnosticsSampleSize is the "last 100" of spec section 7.7.
const diagnosticsSampleSize = 100

func (d *HandlerDeps) adminDiagnostics(r *Request) (*Response, error) {
	accountID, e := queryID(r.Query, "account_id", store.PrefixAccount)
	if e != nil {
		return nil, e
	}
	rows, err := d.Store.Accounts(r.Ctx)
	if err != nil {
		return nil, err
	}
	out := make([]diagnosticsAccountDTO, 0, len(rows))
	for _, row := range rows {
		if accountID != "" && row.ID != accountID {
			continue
		}
		dto := diagnosticsAccountDTO{
			AccountID:             row.ID,
			State:                 string(row.State),
			GaiaDeviceRegUUID:     row.GaiaDestRegUUID,
			GaiaDeviceLastSeenAt:  rfc3339(row.GaiaDeviceLastSeenMS),
			ConfigVersionCompiled: d.ConfigVersionCompiled,
		}
		if a, err := d.Supervisor.Get(row.ID); err == nil {
			dto.CurrentSessionID = a.Backend.SessionID()
			if counter, ok := a.Backend.(gm.DroppedEventCounter); ok {
				dto.DroppedEvents = counter.DroppedEvents()
				dto.UnknownEvents = counter.UnknownEvents()
			}
			if cfg, err := a.Backend.FetchConfig(r.Ctx); err == nil {
				dto.ConfigVersionLive = cfg.Live.String()
			}
		}
		msgs, err := d.Store.ListMessages(r.Ctx, store.MessageQuery{
			AccountID: row.ID, IncludeSystem: true, Limit: diagnosticsSampleSize,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			dto.RecentMessages = append(dto.RecentMessages, diagMessageDTO{
				ID: m.ID, DeliveryState: m.DeliveryState,
				DeliveryStateRaw: m.DeliveryStateRaw, Kind: m.Kind,
			})
		}
		ops, err := d.Store.ListOperations(r.Ctx, store.OperationQuery{
			AccountID: row.ID, Limit: diagnosticsSampleSize,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range ops {
			dto.RecentOperations = append(dto.RecentOperations, diagOperationDTO{
				ID: o.ID, Kind: o.Kind, Status: string(o.Status),
				GoogleStatusRaw: o.GoogleStatusRaw,
			})
		}
		out = append(out, dto)
	}
	if accountID != "" && len(out) == 0 {
		return nil, apierr.NotFound("account")
	}
	return &Response{Data: items(out)}, nil
}
