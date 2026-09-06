package authz

import (
	"context"
	"encoding/json"
	"sync"
)

// The audit kinds this package writes (spec section 12.4).
const (
	// AuditAdminSessionMinted carries source, granted scopes and whether the
	// mint was narrowed -- never the secret.
	AuditAdminSessionMinted = "auth.admin_session_minted"
	// AuditAdminSessionNarrowed is written when a refresh narrows the session.
	AuditAdminSessionNarrowed = "auth.admin_session_narrowed"
	// AuditAdminSessionRefreshed carries scopes before and after.
	AuditAdminSessionRefreshed = "auth.admin_session_refreshed"
	// AuditAdminSecretFailed carries source and the resulting cooldown state
	// -- never the presented value.
	AuditAdminSecretFailed = "auth.admin_secret_failed"
	// AuditRefreshTokenReuse is spec section 12.4's refresh-token reuse
	// detection.
	AuditRefreshTokenReuse = "auth.refresh_token_reuse_detected"
	// AuditRefreshTokenFailed records an unknown or invalid presented token at
	// POST /v1/auth/refresh, with the resulting durable cooldown state.
	AuditRefreshTokenFailed = "auth.refresh_token_failed"
)

// AuditWriter is the minimal view of the audit log this package needs. The
// audit subsystem itself lives elsewhere; this package only ever appends, and
// only ever appends metadata.
//
// A payload carries IDs, counts, codes and outcomes. It never carries a token,
// a secret, a message body or a phone number (spec section 12.4) -- and the
// tests of section 16 Slice 2 test 24 assert that for every row this package
// writes, by scanning the rendered payload for the presented value.
type AuditWriter interface {
	Append(ctx context.Context, kind string, fields map[string]any) error
}

// AuditRecord is one appended row, as a test sees it.
type AuditRecord struct {
	Kind   string
	Fields map[string]any
}

// RecordingAudit is the in-memory AuditWriter used by tests.
type RecordingAudit struct {
	mu      sync.Mutex
	records []AuditRecord
}

// NewRecordingAudit returns an empty recorder.
func NewRecordingAudit() *RecordingAudit { return &RecordingAudit{} }

// Append implements AuditWriter.
func (r *RecordingAudit) Append(_ context.Context, kind string, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	r.records = append(r.records, AuditRecord{Kind: kind, Fields: copied})
	return nil
}

// Records returns every appended row, in order.
func (r *RecordingAudit) Records() []AuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AuditRecord, len(r.records))
	copy(out, r.records)
	return out
}

// OfKind returns every appended row of one kind.
func (r *RecordingAudit) OfKind(kind string) []AuditRecord {
	var out []AuditRecord
	for _, rec := range r.Records() {
		if rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out
}

// JSON renders every appended row as JSON, which is what the sentinel scan of
// spec section 12.2 greps.
func (r *RecordingAudit) JSON() (string, error) {
	b, err := json.Marshal(r.Records())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var _ AuditWriter = (*RecordingAudit)(nil)

// renderPayload turns audit fields into the payload_json column. It is a plain
// marshal: nothing is redacted here, because nothing that needs redacting is
// ever put in.
func renderPayload(fields map[string]any) string {
	if len(fields) == 0 {
		return "{}"
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(b)
}
