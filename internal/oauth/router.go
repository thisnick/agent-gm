package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// pathParamsKey carries the bound path parameters through the request
// context, so a handler reads `{id}` without re-parsing the path.
type pathParamsKey struct{}

func paramsOf(r *http.Request) map[string]string {
	p, _ := r.Context().Value(pathParamsKey{}).(map[string]string)
	return p
}

// ServeHTTP resolves one OAuth route.
//
// The order is the contract of spec section 9.1, and every step of it exists
// because the HTTP framework's own answer would have been the wrong SHAPE:
//
//  1. a body over 1 MiB is `413` with the `{error, error_description}` body;
//  2. a path this server does not serve is the REST `not_found` envelope,
//     because an unknown path is not an OAuth protocol failure;
//  3. a wrong method is `405` with the OAuth body AND an `Allow` header;
//  4. only then the handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxBodyBytes {
		s.writeOAuthError(w, statusError(http.StatusRequestEntityTooLarge, ErrInvalidRequest,
			"the request body is larger than 1 MiB"))
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}

	handlers, params, ok := s.lookup(r.URL.Path)
	if !ok {
		s.writeNotFound(w)
		return
	}
	handler, allowed := handlers[r.Method]
	if !allowed {
		w.Header().Set("Allow", methodsFor(handlers))
		s.writeOAuthError(w, statusError(http.StatusMethodNotAllowed, ErrInvalidRequest,
			"the method is not allowed on this endpoint"))
		return
	}
	if params != nil {
		r = r.WithContext(context.WithValue(r.Context(), pathParamsKey{}, params))
	}
	handler(w, r)
}

// lookup resolves a concrete path to its handler table. Patterns are literal
// segments plus `{name}` placeholders, and a literal beats a placeholder at
// the same position -- the same rule the REST router uses, for the same
// reason: a router whose patterns can overlap subtly is a router where "which
// handler ran?" is a question.
func (s *Server) lookup(path string) (map[string]http.HandlerFunc, map[string]string, bool) {
	best := -1
	var bestHandlers map[string]http.HandlerFunc
	var bestParams map[string]string
	found := false
	for _, pattern := range s.paths {
		params, hit := matchPath(pattern, path)
		if !hit {
			continue
		}
		if score := literalSegments(pattern); score > best {
			best, bestHandlers, bestParams, found = score, s.mux[pattern], params, true
		}
	}
	return bestHandlers, bestParams, found
}

func matchPath(pattern, path string) (map[string]string, bool) {
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

func literalSegments(pattern string) int {
	n := 0
	for _, seg := range splitPath(pattern) {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			continue
		}
		n++
	}
	return n
}

// readBody reads a bounded body, turning the MaxBytesReader's refusal into
// this package's 413 rather than the framework's.
func readBody(r *http.Request) ([]byte, *oauthError) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, statusError(http.StatusRequestEntityTooLarge, ErrInvalidRequest,
				"the request body is larger than 1 MiB")
		}
		return nil, badRequest(ErrInvalidRequest, "the body could not be read")
	}
	return body, nil
}
