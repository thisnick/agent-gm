package api

import (
	"context"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/store"
)

// StoreAppender writes audit rows to `audit_events`.
//
// internal/audit deliberately knows nothing about internal/store: it declares
// a one-method Appender with **no Update and no Delete**, because "an audit
// row is never rewritten" (spec section 12.4) is a rule better stated as a
// missing method than as a paragraph somebody has to read every caller
// against. That leaves exactly one adapter to write, and this is it.
//
// It matters for account removal in particular. `DELETE /v1/accounts/{id}`
// erases the account's rows and **leaves its audit trail standing**, carrying
// the `account_id` of an account that no longer exists -- which is how "what
// happened to this account" stays answerable afterwards. That only works
// because the rows are really in the table rather than in a process's memory,
// so a deployment that wired a memory appender would pass a removal test and
// lose the trail in production.
type StoreAppender struct{ Store *store.Store }

// NewStoreAppender builds the adapter.
func NewStoreAppender(st *store.Store) *StoreAppender { return &StoreAppender{Store: st} }

// AppendAudit implements audit.Appender. The nullable columns arrive as
// pointers so that "no account" is NULL rather than the empty string:
// `WHERE account_id = ”` finding rows would be a quiet lie about which
// events belong to an account.
func (a *StoreAppender) AppendAudit(ctx context.Context, row audit.Row) error {
	_, err := a.Store.AppendAudit(ctx, store.AuditEvent{
		ID:              row.ID,
		Kind:            row.Kind,
		AccountID:       deref(row.AccountID),
		AuthorizationID: deref(row.AuthorizationID),
		TargetType:      deref(row.TargetType),
		TargetID:        deref(row.TargetID),
		Result:          row.Result,
		Source:          row.Source,
		PayloadJSON:     row.PayloadJSON,
		CreatedAtMS:     row.CreatedAtMs,
	})
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// StoreAppender satisfies audit.Appender. The assertion is here rather than
// in a test so a signature change is a compile error at the source.
var _ audit.Appender = (*StoreAppender)(nil)
