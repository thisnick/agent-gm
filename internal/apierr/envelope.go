package apierr

import (
	"github.com/google/uuid"
)

// RequestIDPrefix is the typed prefix of the per-request ID (spec section
// 4.1). Unlike every other prefixed ID it is never stored.
const RequestIDPrefix = "req_"

// NewRequestID mints the `req_` UUIDv7 that goes in every envelope. UUIDv7
// so that request IDs sort by time in a log; not stored, so there is nothing
// to derive it from (spec section 4.1).
func NewRequestID() string {
	// uuid.NewV7 fails only if the system entropy source fails, which Go's
	// crypto/rand already treats as unrecoverable.
	id, err := uuid.NewV7()
	if err != nil {
		panic("apierr: system entropy unavailable: " + err.Error())
	}
	return RequestIDPrefix + id.String()
}

// Success is the success envelope of spec section 7.1:
//
//	{ "data": {}, "next_cursor": null, "warnings": [], "request_id": "req_..." }
//
// next_cursor is a pointer so an absent cursor is JSON null rather than an
// empty string, which a client could mistake for a valid cursor. warnings is
// always an array, never null, so a client can range over it unguarded.
type Success struct {
	Data       any      `json:"data"`
	NextCursor *string  `json:"next_cursor"`
	Warnings   []string `json:"warnings"`
	RequestID  string   `json:"request_id"`
}

// NewSuccess builds the success envelope with the field defaults spec
// section 7.1 shows: a null cursor and an empty warnings array.
func NewSuccess(requestID string, data any) Success {
	return Success{
		Data:      data,
		Warnings:  []string{},
		RequestID: requestID,
	}
}

// WithCursor sets next_cursor. An empty string leaves it null, because "" is
// not a cursor.
func (s Success) WithCursor(cursor string) Success {
	if cursor == "" {
		s.NextCursor = nil
		return s
	}
	c := cursor
	s.NextCursor = &c
	return s
}

// WithWarnings appends warnings, keeping the array non-nil (spec section
// 7.1: normalisation is never silent, so warnings is the channel that says
// what was changed).
func (s Success) WithWarnings(warnings ...string) Success {
	out := make([]string, 0, len(s.Warnings)+len(warnings))
	out = append(out, s.Warnings...)
	out = append(out, warnings...)
	s.Warnings = out
	return s
}

// ErrorBody is the inner object of the error envelope.
type ErrorBody struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// Failure is the error envelope of spec section 7.1:
//
//	{ "error": { "code", "message", "retryable", "details" },
//	  "request_id": "req_..." }
type Failure struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"request_id"`
}

// Envelope renders e as the error envelope. retryable and the details map
// come from the error itself, so a handler cannot answer a retryable flag
// that disagrees with spec section 7.2's table.
func (e *Error) Envelope(requestID string) Failure {
	details := e.Details
	if details == nil {
		// The spec's example shows "details": {}. An absent map would
		// serialise as null and force every client to nil-check.
		details = map[string]any{}
	}
	return Failure{
		Error: ErrorBody{
			Code:      e.Code,
			Message:   e.Message,
			Retryable: e.Retryable(),
			Details:   details,
		},
		RequestID: requestID,
	}
}
