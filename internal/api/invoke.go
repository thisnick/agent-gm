package api

import (
	"context"
	"net/url"
	"reflect"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// In-process dispatch into a route handler, for the MCP surface of spec
// section 8.
//
// Spec section 8.2 says each tool "is a facade over the REST route that serves
// the same data, so the filters, the validation, the error codes and the DTOs
// are the same on both surfaces **by construction rather than by
// discipline**". A tool that re-read the store, or re-derived a filter, or
// built its own DTO would be a second implementation of the same contract,
// and the two would drift the first time one of them was fixed.
//
// So a tool call runs the SAME handler the REST route runs, through the same
// strict-parameter checks, with the same scope check, and renders the
// `*Response` it returns. The only thing that differs is the transport: no
// socket, no envelope, no HTTP status.
//
// What Invoke deliberately does NOT skip:
//
//   - the scope check. MCP checks visibility at `tools/list` and again at
//     `tools/call` (section 8.2), and this is the second one: visibility is
//     not authorization, and a client may call a name it learned elsewhere.
//   - `Route.CheckQuery` and `Route.CheckBodyFields`. A tool that invented an
//     argument name must be told which one, and it must be told by the same
//     code that tells a REST caller, or the two surfaces have two different
//     ideas of what is allowed.
//   - the section 12.3 rate-limit bucket. A tool call is a request; spending
//     from a different pool would make the documented budget a fiction on the
//     surface most likely to be driven by a loop.

// Invocation is one in-process request. Every field is the same fact the
// transport would have resolved from the wire.
type Invocation struct {
	Ctx context.Context
	// RouteName is the inventory name -- `messages_send`, not `POST
	// /v1/conversations/{id}/messages`. Routing by name rather than by a
	// synthesised path is what stops the MCP layer from having to build a
	// URL and the router from having to parse one back.
	RouteName string
	// Path binds the route's path parameters by name.
	Path map[string]string
	// Query is the query string as a caller would have written it.
	Query url.Values
	// Body is the raw JSON body, or nil.
	Body []byte
	// Auth is the authenticated caller. It is never nil on a route whose
	// Scope is not ScopeNone: Invoke refuses rather than running a handler
	// with no authorization.
	Auth *authz.Authorization
	// Bearer is the presented access token. It is needed by the one route
	// that authenticates for itself -- `GET /v1/attachments/{id}/content`,
	// which takes an access token or a download ticket (section 10.3).
	Bearer string
	// Source is the resolved client source (section 12.3).
	Source string
	// RequestID is the `req_` ID this call is correlated by.
	RequestID string
	// IdempotencyKeyHeader is the Idempotency-Key a caller presented, if
	// any. MCP has no headers, so it is normally empty and the key arrives
	// as the `client_request_id` body field instead.
	IdempotencyKeyHeader string
}

// Invoked is what a handler answered, plus the two things the envelope would
// otherwise have carried.
type Invoked struct {
	Route    Route
	Response *Response
	Warnings []string
}

// Invoke dispatches one in-process request and returns the handler's response
// or the error envelope's error.
//
// The returned *apierr.Error is the same value a REST caller would have been
// served, which is what lets the MCP layer put it in
// `structuredContent.error` unchanged (section 8.2's isError semantics).
func (s *Server) Invoke(in Invocation) (*Invoked, *apierr.Error) {
	route, ok := RouteByName(in.RouteName)
	if !ok {
		return nil, apierr.New(apierr.CodeInternalError,
			"no such route in the inventory: "+in.RouteName)
	}
	handler, registered := s.handlers[route.Name]
	if !registered {
		return nil, apierr.New(apierr.CodeInternalError,
			"this route is declared but not served by this build")
	}

	// The scope check, again. `tools/list` already hid what this caller may
	// not use; this is the call-time re-check section 8.2 requires.
	if route.Scope != ScopeNone {
		if in.Auth == nil {
			return nil, apierr.InvalidToken("no bearer was presented")
		}
		if err := in.Auth.Require(authz.Scope(route.Scope)); err != nil {
			return nil, apierr.InsufficientScope(string(route.Scope))
		}
	}
	if in.Auth != nil {
		if err := s.deps.Authz.AllowRequest(in.Auth, bucketFor(route)); err != nil {
			return nil, apierr.RateLimited(retryAfterFrom(err))
		}
	}

	query := in.Query
	if query == nil {
		query = url.Values{}
	}
	if e := route.CheckQuery(query); e != nil {
		return nil, e
	}
	if e := route.CheckBodyFields(in.Body); e != nil {
		return nil, e
	}

	flat := map[string]string{}
	for name, values := range query {
		if len(values) > 0 {
			flat[name] = values[0]
		}
	}
	ctx := in.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := in.RequestID
	if requestID == "" {
		requestID = apierr.NewRequestID()
	}
	path := in.Path
	if path == nil {
		path = map[string]string{}
	}

	req := &Request{
		Ctx:                  ctx,
		Route:                route,
		Path:                 path,
		Query:                flat,
		Body:                 in.Body,
		Bearer:               in.Bearer,
		Auth:                 in.Auth,
		Source:               in.Source,
		IdempotencyKeyHeader: in.IdempotencyKeyHeader,
		RequestID:            requestID,
	}
	resp, err := handler(req)
	if err != nil {
		return nil, apierr.From(err)
	}
	if resp == nil {
		resp = &Response{}
	}
	return &Invoked{Route: route, Response: resp, Warnings: req.Warnings}, nil
}

// DataShape describes the Go type a route's envelope `data` renders from.
//
// It exists so that section 8.2's "every tool declares an `outputSchema`
// describing the envelope it really returns, with `data` typed by that tool's
// own DTO" can be true by construction: the MCP layer reflects over the same
// struct the handler serialises, so a field added to a DTO appears in the
// tool's output schema without anybody remembering to add it.
//
// The DTOs are unexported, which is deliberate -- nothing outside this package
// constructs one -- so the shape is handed out as a reflect.Type rather than
// as a value.
type DataShape struct {
	// Type is the struct `data` renders from, or, when Items is true, the
	// struct one row of `data.items` renders from.
	Type reflect.Type
	// Items reports that this route is a listing, whose rows live in
	// `data.items` and never in `data` as a bare array (spec section 7.1).
	Items bool
	// Coverage reports that `data` carries search's `coverage` sibling.
	Coverage bool
}

// dataShapes maps a route name to the DTO its handler renders.
//
// Only the routes MCP serves are listed: the map answers "what does this tool
// return", and a route with no tool has no output schema to describe. The
// values name the very types the handlers return, so the compiler fails if one
// is renamed.
var dataShapes = map[string]DataShape{
	// reads
	"conversations_list": {Type: reflect.TypeOf(conversationDTO{}), Items: true},
	"conversations_get":  {Type: reflect.TypeOf(conversationDTO{})},
	"messages_list":      {Type: reflect.TypeOf(messageDTO{}), Items: true},
	"messages_get":       {Type: reflect.TypeOf(messageDTO{})},
	"messages_context":   {Type: reflect.TypeOf(messageContextDTO{})},
	"search_messages":    {Type: reflect.TypeOf(searchHitDTO{}), Items: true, Coverage: true},
	"attachments_get":    {Type: reflect.TypeOf(attachmentDTO{})},
	"contacts_list":      {Type: reflect.TypeOf(contactDTO{}), Items: true},
	"accounts_get":       {Type: reflect.TypeOf(accountDetailDTO{})},
	"accounts_list":      {Type: reflect.TypeOf(accountListDTO{}), Items: true},
	"health":             {Type: reflect.TypeOf(healthDTO{})},

	// writes
	"messages_send":           {Type: reflect.TypeOf(mutationDTO{})},
	"conversations_start":     {Type: reflect.TypeOf(mutationDTO{})},
	"conversations_mark_read": {Type: reflect.TypeOf(mutationDTO{})},
	"reactions_add":           {Type: reflect.TypeOf(mutationDTO{})},
	"reactions_remove":        {Type: reflect.TypeOf(mutationDTO{})},
	"reactions_remove_by_id":  {Type: reflect.TypeOf(mutationDTO{})},
	"conversations_update":    {Type: reflect.TypeOf(mutationDTO{})},
	"uploads_create":          {Type: reflect.TypeOf(uploadDTO{})},
	"operations_get":          {Type: reflect.TypeOf(operationDTO{})},

	// deletes
	"messages_delete":      {Type: reflect.TypeOf(mutationDTO{})},
	"conversations_delete": {Type: reflect.TypeOf(mutationDTO{})},
}

// DataShapeFor returns the DTO shape a route's `data` renders from.
func DataShapeFor(routeName string) (DataShape, bool) {
	s, ok := dataShapes[routeName]
	return s, ok
}

// SearchCoverageType is the type of search's `coverage` sibling, which is the
// one key any listing puts beside `items`.
func SearchCoverageType() reflect.Type { return reflect.TypeOf(searchCoverage{}) }
