package audit

import "context"

// FieldsAppender adapts this package's Event API to the `(kind, fields)`
// shape `internal/accounts` and `internal/authz` append through.
//
// Those two packages deliberately do not import this one: they declare a
// one-method Auditor interface so they can be reasoned about, and tested,
// without the audit store. Something has to join the two, and until this
// existed nothing did -- `serve` built its supervisor with a nil Auditor, so
// **`account.paired`, `account.resumed`, `account.signed_out` and
// `account.state_changed` were written by every test that injected a recorder
// and by no running server**. A Slice 3 review found the shipped binary's
// `audit_events` had no `account.*` kind in it at all.
//
// The adapter is thin on purpose. It does two things the Event API needs and
// the field map does not carry:
//
//   - it lifts `account_id` out of the payload into the column, because
//     section 12.4's "every audit row carries account_id" is a column and not
//     a JSON key, and `--account-id` filters on the column;
//   - it supplies the client source, which the supervisor does not know: a
//     pairing is driven by a REST call but completed on a background
//     goroutine minutes later, so the source recorded is the server's own.
//
// Everything else -- the declared-kind check, the account rule, redaction --
// is the Writer's, which is the point of routing through it rather than
// writing rows directly.
type FieldsAppender struct {
	w      *Writer
	source string
}

// NewFieldsAppender builds an appender over a writer. `source` is the client
// source recorded on every row it writes; it is never empty, because the
// Writer refuses a row without one (section 12.3).
func NewFieldsAppender(w *Writer, source string) *FieldsAppender {
	if source == "" {
		source = "server"
	}
	return &FieldsAppender{w: w, source: source}
}

// Append writes one row. A nil appender, or one over a nil writer, does
// nothing and reports no error: that is the same "simply does not audit"
// contract the supervisor's nil Auditor has.
func (a *FieldsAppender) Append(ctx context.Context, kind string, fields map[string]any) error {
	if a == nil || a.w == nil {
		return nil
	}
	accountID, _ := fields["account_id"].(string)
	payload := make(map[string]any, len(fields))
	for k, v := range fields {
		if k == "account_id" {
			continue
		}
		payload[k] = v
	}
	e := Event{
		Kind:      Kind(kind),
		AccountID: accountID,
		Result:    ResultOK,
		Source:    a.source,
		Payload:   payload,
	}
	if accountID != "" {
		e.TargetType, e.TargetID = "account", accountID
	}
	return a.w.Append(ctx, e)
}
