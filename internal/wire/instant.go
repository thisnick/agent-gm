// Package wire holds the renderings every JSON surface must agree on.
//
// It exists because there is more than one such surface. The DTO layer in
// internal/api renders almost everything, and the SSE feed does not: an
// accounts.StateChange is marshalled by the accounts package and written
// straight to the socket. Section 4.4's rule -- RFC 3339, UTC, exactly
// millisecond precision -- was therefore stated twice, and the stream was the
// copy that drifted: Go's default time.Time marshalling is RFC3339Nano, which
// STRIPS trailing zeros, so `.507Z` and `.52Z` both appeared in one stream
// seconds apart. A client parsing a fixed three-digit fraction works until a
// timestamp lands on a multiple of ten milliseconds, which is one frame in
// ten. Intermittent is the worst way to fail.
//
// So the rendering lives in one place that both layers call, and the next
// rule about how an instant looks on the wire is written once.
package wire

import "time"

// layout is section 4.4's: RFC 3339, UTC, exactly three fractional digits.
// The literal `.000` is what fixes the width -- a format built from
// time.RFC3339Nano would not.
const layout = "2006-01-02T15:04:05.000Z"

// Instant renders one moment. The zero time renders as the empty string;
// callers that distinguish "no such moment" from "the Unix epoch" use
// InstantPtr.
func Instant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(layout)
}

// InstantPtr renders a moment, or nil for the zero value -- "no such moment"
// and "the Unix epoch" are different facts and a caller must be able to tell
// them apart.
func InstantPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := Instant(t)
	return &s
}

// InstantMS renders an epoch-millisecond column, or nil for zero.
func InstantMS(ms int64) *string {
	if ms == 0 {
		return nil
	}
	return InstantPtr(time.UnixMilli(ms))
}
