package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/clock"
)

// Row is one row of section 4.2's `audit_events`, already redacted and ready
// to insert. The nullable columns are pointers so that "no account" is NULL
// and not the empty string: `WHERE account_id = ”` finding rows would be a
// quiet lie about which events belong to an account.
type Row struct {
	ID              string
	Kind            string
	AccountID       *string
	AuthorizationID *string
	TargetType      *string
	TargetID        *string
	Result          string
	Source          string
	PayloadJSON     string
	CreatedAtMs     int64
}

// Appender is the persistence this package writes through, and the whole
// interface it has.
//
// It offers no Update and no Delete, and that is the point rather than an
// omission: section 12.4 says an audit row is never rewritten, by a migration
// or by anything else. A rule stated in prose is checked by reading every
// caller; a rule stated as a missing method is checked by reading one
// interface. The store implements this against `audit_events`.
type Appender interface {
	AppendAudit(ctx context.Context, row Row) error
}

// MemoryAppender is an in-memory Appender for tests. Like the real one it can
// only append.
type MemoryAppender struct {
	mu   sync.Mutex
	rows []Row
}

// NewMemoryAppender builds an empty in-memory appender.
func NewMemoryAppender() *MemoryAppender { return &MemoryAppender{} }

func (a *MemoryAppender) AppendAudit(_ context.Context, row Row) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, row)
	return nil
}

// Rows returns a copy of everything appended so far.
func (a *MemoryAppender) Rows() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Row, len(a.rows))
	copy(out, a.rows)
	return out
}

// Event is what a caller hands the writer.
type Event struct {
	// Kind must be one of the declared kinds.
	Kind Kind
	// AccountID is the acct_ ID, empty for a server-wide event. Section
	// 12.4: every row carries it where the event belongs to an account, so
	// "what happened to this account" stays answerable after the account is
	// removed.
	AccountID string
	// AuthorizationID is the authorization the call was made under, if any.
	AuthorizationID string
	// TargetType and TargetID name what the event was about.
	TargetType string
	TargetID   string
	// Result is ok, refused or failed.
	Result Result
	// Source is the resolved client source of section 12.3, computed
	// elsewhere and never empty: it is the field every per-source limit and
	// every "who did this" question is answered from.
	Source string
	// Payload carries IDs, counts, codes and outcomes. It is redacted on the
	// way in, and a payload carrying anything from section 12.2's list is
	// refused.
	Payload map[string]any
}

// ErrNoSource is the refusal for an event with an empty client source.
var ErrNoSource = errors.New("audit: source is required and is never the empty string (spec 12.3)")

// Writer appends audit rows. It exposes exactly one method, Append: there is
// no update and no delete on the type, and none on the interface underneath
// it.
type Writer struct {
	appender Appender
	clock    clock.Clock
	redactor *Redactor
	newID    func() string
}

// NewWriter builds a writer over an appender and a clock.
func NewWriter(a Appender, c clock.Clock) *Writer {
	if c == nil {
		c = clock.Real{}
	}
	return &Writer{
		appender: a,
		clock:    c,
		redactor: NewRedactor(),
		newID:    newUUIDv7,
	}
}

func newUUIDv7() string {
	id, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 fails only if the system entropy source does, which is
		// the same condition the salt panics on.
		panic("audit: cannot mint an audit row ID: " + err.Error())
	}
	return id.String()
}

// Append validates, redacts and stores one event.
//
// It refuses, and stores nothing, when:
//
//   - the kind is not declared (a typo must not become an unfindable row);
//   - an account-shaped kind carries no account_id, or a server-wide kind
//     carries one;
//   - the source is empty;
//   - the result is not ok, refused or failed;
//   - the payload carries anything from section 12.2's list.
func (w *Writer) Append(ctx context.Context, e Event) error {
	rule, declared := Rule(e.Kind)
	if !declared {
		return fmt.Errorf("audit: %q is not a declared audit kind (spec 12.4)", e.Kind)
	}
	switch rule {
	case AccountRequired:
		if e.AccountID == "" {
			return fmt.Errorf("audit: %q belongs to an account and needs an account_id; "+
				"without it \"what happened to this account\" stops being answerable "+
				"once the account is removed (spec 12.4, 4.7)", e.Kind)
		}
	case AccountForbidden:
		if e.AccountID != "" {
			return fmt.Errorf("audit: %q is a server-wide event and its account_id must be NULL "+
				"(spec 12.4)", e.Kind)
		}
	case AccountOptional:
	}
	if e.Source == "" {
		return ErrNoSource
	}
	switch e.Result {
	case ResultOK, ResultRefused, ResultFailed:
	default:
		return fmt.Errorf("audit: result must be %q, %q or %q, not %q",
			ResultOK, ResultRefused, ResultFailed, e.Result)
	}

	payload, err := w.redactor.Payload(e.Payload)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit: cannot encode payload: %w", err)
	}

	row := Row{
		ID:          w.newID(),
		Kind:        string(e.Kind),
		Result:      string(e.Result),
		Source:      e.Source,
		PayloadJSON: string(encoded),
		CreatedAtMs: w.clock.Now().UnixMilli(),
	}
	row.AccountID = optional(e.AccountID)
	row.AuthorizationID = optional(e.AuthorizationID)
	row.TargetType = optional(e.TargetType)
	row.TargetID = optional(e.TargetID)

	return w.appender.AppendAudit(ctx, row)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
