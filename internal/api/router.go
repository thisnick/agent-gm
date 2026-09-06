package api

import (
	"net/http"
	"strings"
)

// match reports whether a concrete request path matches a route pattern, and
// returns the path parameters it bound.
//
// Patterns are the inventory's: literal segments plus `{name}` placeholders.
// There is no wildcard, no optional segment and no regular expression, which
// is deliberate -- a router whose patterns can overlap in subtle ways is a
// router where "which handler ran?" is a question, and every one of these
// routes is addressed by an exact shape.
func match(pattern, path string) (map[string]string, bool) {
	pSegs := splitPath(pattern)
	rSegs := splitPath(path)
	if len(pSegs) != len(rSegs) {
		return nil, false
	}
	var params map[string]string
	for i, p := range pSegs {
		if len(p) > 2 && p[0] == '{' && p[len(p)-1] == '}' {
			if rSegs[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			params[p[1:len(p)-1]] = rSegs[i]
			continue
		}
		if p != rSegs[i] {
			return nil, false
		}
	}
	return params, true
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// specificity ranks a pattern so that a literal segment beats a placeholder
// at the same position.
//
// It exists for exactly one pair of routes in the inventory:
// `/v1/accounts/events` and `/v1/accounts/{account_id}`. Both are three
// segments and both are GET, so an unordered walk would answer the
// all-accounts stream with the single-account handler and an `account_id` of
// the literal string "events" -- a not_found that an owner would spend a long
// time not understanding. Ranking makes the literal win, always, rather than
// depending on the order the inventory happens to be written in.
func specificity(pattern string) int {
	n := 0
	for _, seg := range splitPath(pattern) {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			continue
		}
		n++
	}
	return n
}

// Match resolves a method and path to a route.
//
// The three outcomes are distinguished, because they are three different
// answers to a caller:
//
//   - a route, with its path parameters;
//   - no route at this path at all -> `not_found`;
//   - a route at this path but not for this method -> `405` with `Allow`,
//     which tells a caller that mistyped `GET` for `POST` what to fix rather
//     than sending them looking for a missing object.
func Match(method, path string) (route Route, params map[string]string, allowed []string, ok bool) {
	best := -1
	for _, r := range Routes {
		p, hit := match(r.Path, path)
		if !hit {
			continue
		}
		allowed = appendUnique(allowed, r.Method)
		if r.Method != method {
			continue
		}
		if s := specificity(r.Path); s > best {
			best, route, params, ok = s, r, p, true
		}
	}
	return route, params, allowed, ok
}

func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// AllowHeader renders the Allow header for a 405.
func AllowHeader(methods []string) string {
	// A stable order, so the header is the same on every request rather than
	// depending on the inventory's iteration.
	order := []string{
		http.MethodGet, http.MethodPost, http.MethodPatch,
		http.MethodPut, http.MethodDelete,
	}
	var out []string
	for _, m := range order {
		for _, have := range methods {
			if have == m {
				out = append(out, m)
			}
		}
	}
	return strings.Join(out, ", ")
}
