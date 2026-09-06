package api

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/store"
)

// Parameter reading, pagination and the ID prefix rule.
//
// Nothing here decides whether a parameter is *allowed* -- the inventory and
// the middleware settled that before a handler ran (spec section 7.1). These
// functions only turn an allowed parameter's text into a value, and refuse a
// value that is not one.
//
// The refusals matter as much as the parsing. `?unread_only=yes` is
// `invalid_request` rather than false, because a filter that quietly did not
// apply returns a full unfiltered list that looks exactly like a correct
// answer -- the same hazard section 7.1's strict rejection exists for, one
// level down.

// DefaultLimit and MaxLimit are spec section 7.4's bounds, on every listing.
const (
	DefaultLimit = 50
	MaxLimit     = 100
	// DefaultContext and MaxContext bound GET /v1/messages/{id}/context.
	DefaultContext = 5
	MaxContext     = 100
)

// --- scalars ----------------------------------------------------------------

// boolParam reads a boolean filter. Only `true` and `false` are accepted:
// `1`, `yes` and `on` are refused rather than guessed at, because a caller
// who wrote one of them has a client that will keep writing it.
func boolParam(q map[string]string, name string) (*bool, *apierr.Error) {
	raw, ok := q[name]
	if !ok || raw == "" {
		return nil, nil
	}
	switch raw {
	case "true":
		v := true
		return &v, nil
	case "false":
		v := false
		return &v, nil
	}
	return nil, apierr.WrongTypeForField(name, "either \"true\" or \"false\"")
}

// boolFlag is boolParam for a filter whose absence means false.
func boolFlag(q map[string]string, name string) (bool, *apierr.Error) {
	v, err := boolParam(q, name)
	if err != nil || v == nil {
		return false, err
	}
	return *v, nil
}

// timeParam reads an RFC 3339 instant and returns epoch milliseconds. A
// timestamp Agent GM cannot parse is refused rather than treated as the zero
// time, which would silently select everything.
func timeParam(q map[string]string, name string) (int64, *apierr.Error) {
	raw, ok := q[name]
	if !ok || raw == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, apierr.WrongTypeForField(name, "an RFC 3339 timestamp such as \"2026-09-06T09:41:02Z\"")
	}
	return t.UTC().UnixMilli(), nil
}

// intParam reads a bounded integer.
func intParam(q map[string]string, name string, def, max int) (int, *apierr.Error) {
	raw, ok := q[name]
	if !ok || raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, apierr.WrongTypeForField(name, "a non-negative integer")
	}
	if n == 0 {
		return def, nil
	}
	if n > max {
		// Capping rather than refusing is what section 7.4 says a limit
		// does; the caller asked for more than the page size and gets the
		// page size.
		n = max
	}
	return n, nil
}

// enumParam reads a value from a closed set, naming the alternatives.
func enumParam(q map[string]string, name string, allowed ...string) (string, *apierr.Error) {
	raw, ok := q[name]
	if !ok || raw == "" {
		return "", nil
	}
	for _, a := range allowed {
		if raw == a {
			return raw, nil
		}
	}
	return "", apierr.WrongTypeForField(name, "one of "+strings.Join(allowed, ", "))
}

// --- identifiers ------------------------------------------------------------

// pathID reads a path parameter and enforces spec section 4.1's typed prefix.
//
// **A wrong-prefix ID is `invalid_request` naming the parameter and the
// expected prefix -- never `not_found`** (section 16 Slice 2 test 13). A
// `msg_` ID where a `conv_` is expected is a caller mistake with a fix;
// `not_found` would say the thread is gone and send them looking for a
// deleted conversation. A raw Google ID lands here too, because it carries no
// Agent GM prefix at all.
//
// The check happens BEFORE any lookup, so a wrong-prefix ID never reaches a
// query that could answer not_found first.
func pathID(r *Request, name, prefix string) (string, *apierr.Error) {
	value := r.Path[name]
	if e := apierr.CheckIDPrefix(value, name, prefix); e != nil {
		return "", e
	}
	return value, nil
}

// queryID enforces the same rule for an optional query parameter.
func queryID(q map[string]string, name, prefix string) (string, *apierr.Error) {
	value, ok := q[name]
	if !ok || value == "" {
		return "", nil
	}
	if e := apierr.CheckIDPrefix(value, name, prefix); e != nil {
		return "", e
	}
	return value, nil
}

// --- pagination -------------------------------------------------------------

// page is one listing's resolved pagination.
type page struct {
	Limit  int
	Cursor *store.Cursor
	// endpoint and fingerprint are carried so the next cursor is minted with
	// the same binding the presented one was checked against.
	endpoint    string
	fingerprint string
}

// filterFingerprint hashes the query **as the caller wrote it**, minus the
// two pagination controls (spec section 7.4). Nothing is defaulted,
// lowercased or dropped first: a cursor issued without `folder` is not valid
// when replayed with `folder=active`, although the two select the same rows.
// A client that walks a listing sends the same query string on every page
// anyway, so the strict form costs a correct caller nothing and catches an
// incorrect one on its first page.
func filterFingerprint(q map[string]string) string {
	params := map[string][]string{}
	for k, v := range q {
		if k == "cursor" || k == "limit" {
			continue
		}
		params[k] = []string{v}
	}
	return store.FilterFingerprint(params)
}

// paging resolves `cursor` and `limit` for one endpoint.
//
// A tampered cursor and a cursor replayed against a different filter set are
// both `invalid_request`, with different sentences: one is somebody editing a
// token, the other is a client that changed its query mid-walk, and an
// operator reading the logs wants to tell them apart even though the caller
// gets the same code either way.
func (d *HandlerDeps) paging(r *Request, endpoint string) (page, *apierr.Error) {
	limit, err := intParam(r.Query, "limit", DefaultLimit, MaxLimit)
	if err != nil {
		return page{}, err
	}
	p := page{
		Limit:       limit,
		endpoint:    endpoint,
		fingerprint: filterFingerprint(r.Query),
	}
	raw, ok := r.Query["cursor"]
	if !ok || raw == "" {
		return p, nil
	}
	c, decodeErr := store.DecodeCursor(d.DataKey, endpoint, p.fingerprint, raw)
	switch {
	case errors.Is(decodeErr, store.ErrCursorFilterMismatch):
		return page{}, apierr.WrongTypeForField("cursor",
			"a cursor issued for this endpoint with these same filters; "+
				"send the same query string on every page")
	case decodeErr != nil:
		return page{}, apierr.WrongTypeForField("cursor", "a cursor this server issued")
	}
	p.Cursor = &c
	return p, nil
}

// hasCursorPosition is what a row must offer to be a page boundary.
type hasCursorPosition interface{ CursorPosition() store.Cursor }

// nextCursor mints the cursor for the page after these rows, or "" when the
// page was not full and there is therefore nothing after it.
//
// The cursor encodes `(sent_at_ms, id)` rather than an offset, so it is
// stable across the equal millisecond timestamps Google produces in a burst
// -- the same property spec section 16 Slice 2 test 1 asserts survives a
// restart.
func nextCursor[T hasCursorPosition](d *HandlerDeps, p page, rows []T) (string, *apierr.Error) {
	if len(rows) < p.Limit || len(rows) == 0 {
		return "", nil
	}
	token, err := store.EncodeCursor(d.DataKey, p.endpoint, p.fingerprint,
		rows[len(rows)-1].CursorPosition())
	if err != nil {
		return "", apierr.Internal(err)
	}
	return token, nil
}
