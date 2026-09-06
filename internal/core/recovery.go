package core

import (
	"context"
	"time"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/store"
)

// RecoverOperations is the startup sweep of spec section 6.6.
//
//	UPDATE operations SET status='unknown', terminal=1, terminal_at_ms=?,
//	       error_code='crash_recovered'
//	 WHERE status='running';
//
// It runs BEFORE the listener binds and before any account connects, and it
// is process-wide rather than per-account deliberately: a crash is a property
// of the process, so every account's in-flight operations are settled at
// once. Accounts are independent in their NETWORK behaviour; recovery from a
// process death is not an account concern.
//
// Each row is audited as operation.crash_recovered, with its account_id.
// **Nothing is retried.** If the send did reach Google, the remote echo will
// arrive on reconnect and correct the operation to `succeeded` with a
// message_id -- which is exactly why the correction transition out of
// `unknown` exists.
//
// A `pending` operation is left alone here; ReapPendingOperations settles it
// at pending_timeout, measured from created_at_ms.
func RecoverOperations(ctx context.Context, st *store.Store, aud *audit.Writer, source string) ([]store.Operation, error) {
	ops, err := st.RecoverRunningOperations(ctx)
	if err != nil {
		return nil, err
	}
	auditOperations(ctx, aud, ops, audit.KindOperationCrashRecovered, source, store.ErrCrashRecovered)
	return ops, nil
}

// ReapPendingOperations settles every `pending` operation older than
// operations.pending_timeout to `unknown`, measured from created_at_ms and on
// the INJECTED clock -- so a test advances 24 hours rather than waiting them
// out. Like crash recovery it is process-wide.
func ReapPendingOperations(ctx context.Context, st *store.Store, aud *audit.Writer, timeout time.Duration, source string) ([]store.Operation, error) {
	ops, err := st.ReapPendingOperations(ctx, timeout.Milliseconds())
	if err != nil {
		return nil, err
	}
	auditOperations(ctx, aud, ops, audit.KindOperationCrashRecovered, source, store.ErrPendingTimeout)
	return ops, nil
}

func auditOperations(ctx context.Context, aud *audit.Writer, ops []store.Operation, kind audit.Kind, source, errorCode string) {
	if aud == nil {
		return
	}
	if source == "" {
		source = "system"
	}
	for _, op := range ops {
		_ = aud.Append(ctx, audit.Event{
			Kind:            kind,
			AccountID:       op.AccountID,
			AuthorizationID: op.AuthorizationID,
			TargetType:      "operation",
			TargetID:        op.ID,
			Result:          audit.ResultFailed,
			Source:          source,
			Payload: map[string]any{
				"kind":       op.Kind,
				"status":     string(op.Status),
				"error_code": errorCode,
			},
		})
	}
}

// SweepIdempotencyKeys deletes operation rows past
// operations.idempotency_ttl. It never deletes a row that is not terminal.
func SweepIdempotencyKeys(ctx context.Context, st *store.Store, ttl time.Duration) (int64, error) {
	return st.SweepIdempotencyKeys(ctx, ttl.Milliseconds())
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
