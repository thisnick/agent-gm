package apierr

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// MaxBodyBytes is the 1 MiB body limit of spec section 7.2. A body over it is
// 413 payload_too_large; a body that fails to read for any other reason is
// 400, because "too large" would be a guess.
const MaxBodyBytes int64 = 1 << 20

// AllowedQuery is the set of query parameters one route defines. Nothing else
// is accepted -- there is no allowlist for cache-busting parameters such as
// `_`, because a route that quietly ignores `?directon=incoming` answers a
// full unfiltered list that looks exactly like a correct answer (spec
// section 7.1).
type AllowedQuery map[string]struct{}

// NewAllowedQuery builds the allowed set for a route.
func NewAllowedQuery(names ...string) AllowedQuery {
	a := make(AllowedQuery, len(names))
	for _, n := range names {
		a[n] = struct{}{}
	}
	return a
}

// DecodeQuery rejects any query parameter the route does not define,
// returning invalid_request naming it in details.parameter. It returns nil
// when every parameter is known.
//
// When several are unknown it names the first in sorted order, so the same
// request always produces the same answer regardless of map iteration.
func DecodeQuery(values url.Values, allowed AllowedQuery) *Error {
	var unknown []string
	for name := range values {
		if _, ok := allowed[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return UnknownQueryParameter(unknown[0])
}

// DecodeBody reads at most MaxBodyBytes of JSON from r into dst, rejecting
// unknown fields (spec section 7.1). An empty body is accepted and leaves dst
// untouched, because the two DELETE routes of spec section 7.7 carry an
// optional body.
//
// The size rule of spec section 7.2 is exact: over the limit is 413
// payload_too_large, and any other read failure is 400, because calling a
// truncated upload "too large" would be a guess.
func DecodeBody(r io.Reader, dst any) *Error {
	// One byte past the limit distinguishes "exactly at the limit" from
	// "over it" without buffering an unbounded body.
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if int64(len(body)) > MaxBodyBytes {
		return PayloadTooLarge("the request body", MaxBodyBytes)
	}
	if err != nil {
		return MalformedBody("the body could not be read")
	}
	return DecodeBodyBytes(body, dst)
}

// DecodeBodyBytes is DecodeBody for a body already in memory, for the callers
// that must hash or log the bytes before decoding them.
func DecodeBodyBytes(body []byte, dst any) *Error {
	if int64(len(body)) > MaxBodyBytes {
		return PayloadTooLarge("the request body", MaxBodyBytes)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second JSON value after the first is not a body this API defines.
	if dec.More() {
		return MalformedBody("the body carried more than one JSON value")
	}
	return nil
}

// unknownFieldPrefix is the exact text encoding/json produces for a field
// DisallowUnknownFields refused. There is no typed error for it, so the
// string is matched here, in one place, and decode_test.go pins the match
// against the real decoder rather than against this constant.
const unknownFieldPrefix = `json: unknown field `

// decodeError turns an encoding/json failure into the spec's answer: an
// unknown field is invalid_request naming the field, a type mismatch names
// the field it happened on, and anything else is a plain invalid_request.
func decodeError(err error) *Error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return WrongTypeForField(typeErr.Field, typeErr.Type.String())
	}

	msg := err.Error()
	if strings.HasPrefix(msg, unknownFieldPrefix) {
		quoted := strings.TrimPrefix(msg, unknownFieldPrefix)
		if name, uerr := strconv.Unquote(quoted); uerr == nil {
			return UnknownBodyField(name)
		}
		return UnknownBodyField(strings.Trim(quoted, `"`))
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return MalformedBody("the body is not valid JSON")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return MalformedBody("the body ended in the middle of a JSON value")
	}
	return MalformedBody("the body is not the JSON this route expects")
}
