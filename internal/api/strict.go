package api

import (
	"encoding/json"
	"net/url"
	"sort"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// CheckQuery enforces spec section 7.1's strict parameter rejection for this
// route's query string.
//
// **Nothing is allowlisted**, including a cache-busting `_=`. A misspelled
// filter is refused rather than silently ignored, because
// `?directon=incoming` returning a full unfiltered list looks exactly like a
// correct answer, and an agent has no way to tell the difference. A refused
// request has no effect.
//
// The allowed set comes from the inventory rather than from the handler, so
// section 16 Slice 2 test 12 is exhaustive route by route by construction
// rather than by a sample somebody remembered to keep up to date.
func (r Route) CheckQuery(values url.Values) *apierr.Error {
	// Deterministic order: two unknown parameters in one query must always
	// name the same one, or the error a caller sees depends on Go's map
	// iteration and a test of it would be flaky.
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !r.AllowsQuery(name) {
			return apierr.UnknownQueryParameter(name)
		}
	}
	return nil
}

// CheckBodyFields enforces the same rule for a JSON body.
//
// It is a pre-pass over the raw object rather than a `DisallowUnknownFields`
// decode into the handler's struct, for two reasons. The inventory is the
// contract, and a struct's tags are a second place the allowed set could
// live and drift. And a body field that exists on the route but that this
// particular handler ignores would pass a struct decode silently, which is
// the same class of mistake as a misspelled query parameter.
//
// An empty body is fine on every route: `{}` and no body at all are the same
// request, and a route that requires a field says so itself.
func (r Route) CheckBodyFields(raw []byte) *apierr.Error {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		// A body that is not a JSON object at all is malformed, not an
		// unknown field. `null` decodes to a nil map and is treated as an
		// empty body, which is what a client sending an explicit null means.
		if isJSONNull(raw) {
			return nil
		}
		return apierr.MalformedBody("the body must be a JSON object")
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !r.AllowsBody(name) {
			return apierr.UnknownBodyField(name)
		}
	}
	return nil
}

func isJSONNull(raw []byte) bool {
	var v any
	return json.Unmarshal(raw, &v) == nil && v == nil
}

// IdempotencyKeyFrom returns the key for a mutation, from the two transports
// spec section 6.3 gives it and no others: the `Idempotency-Key` header, or
// the `client_request_id` body field.
//
// **Supplying both with different values is `invalid_request`**, because it
// is a contradiction rather than a preference: a server that picked one
// would be guessing which of two things the caller meant, and the cost of
// guessing wrong is a second real text message to a real person.
//
// An empty key, a key over 200 bytes, or a key containing control characters
// is `invalid_request` naming `client_request_id` in `details.field`, and
// writes nothing.
func IdempotencyKeyFrom(header, bodyField string) (string, *apierr.Error) {
	switch {
	case header != "" && bodyField != "" && header != bodyField:
		return "", apierr.ContradictoryIdempotencyKey()
	case header != "":
		return header, validateIdempotencyKey(header)
	case bodyField != "":
		return bodyField, validateIdempotencyKey(bodyField)
	default:
		return "", apierr.MissingIdempotencyKey()
	}
}

// MaxIdempotencyKeyBytes is spec section 6.3's bound.
const MaxIdempotencyKeyBytes = 200

func validateIdempotencyKey(key string) *apierr.Error {
	if len(key) > MaxIdempotencyKeyBytes {
		return apierr.BadIdempotencyKey("it is longer than 200 bytes")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return apierr.BadIdempotencyKey("it contains a control character")
		}
	}
	return nil
}
