package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// session is one authenticated request's worth of context. There is no
// durable session: this server is stateless at the application layer, and
// every request carries its own bearer and its own protocol metadata.
type session struct {
	handler *Handler
	auth    *authz.Authorization
	bearer  string
	source  string
	ctx     context.Context
}

// dispatch runs one JSON-RPC method.
//
// The second return says the call was a notification and has no answer.
func (s *session) dispatch(req jsonrpcRequest) (jsonrpcResponse, bool) {
	if req.isNotification() {
		// `notifications/initialized` and its siblings are acknowledged by
		// accepting them. An unknown notification is also accepted: a
		// notification has no reply, so refusing one would be shouting into
		// a closed channel.
		return jsonrpcResponse{}, true
	}
	switch req.Method {
	case "initialize":
		return rpcResult(req.ID, s.initialize(req.Params)), false
	case "ping":
		return rpcResult(req.ID, map[string]any{}), false
	case "tools/list":
		return rpcResult(req.ID, s.toolsList()), false
	case "tools/call":
		return s.toolsCall(req), false
	case "resources/list":
		// Empty on purpose: attachments are addressed by template, not
		// enumerated. A server that listed every attachment it holds would
		// answer a question nobody asked with a page nobody can use.
		return rpcResult(req.ID, map[string]any{"resources": []any{}}), false
	case "resources/templates/list":
		return rpcResult(req.ID, s.resourceTemplates()), false
	case "resources/read":
		return s.resourcesRead(req), false
	default:
		// An unknown method is one of the four things section 8.2 allows to
		// be a JSON-RPC error: it is a fact about the protocol, not about
		// the owner's messages, and a model cannot correct itself from it.
		return rpcFail(req.ID, codeMethodNotFound, "no such method: "+req.Method), false
	}
}

// --- initialize ---------------------------------------------------------------

func (s *session) initialize(raw json.RawMessage) map[string]any {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &params)
	version := ProtocolVersion
	if supportedProtocol(params.ProtocolVersion) {
		version = params.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name": "agent-gm",
			// The AGPL section 13 obligation of spec section 1.4: every
			// deployment says where its own source is, at the exact commit
			// it is running. An empty commit or source_url here would make
			// the MCP surface a licence gap.
			"version":    s.handler.cfg.Version,
			"commit":     s.handler.cfg.Commit,
			"source_url": s.handler.cfg.SourceURL,
		},
		"instructions": Instructions,
	}
}

// --- tools/list ----------------------------------------------------------------

func (s *session) toolsList() map[string]any {
	visible := VisibleTools(s.auth)
	out := make([]any, 0, len(visible))
	for _, t := range visible {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"inputSchema":  t.InputSchema(),
			"outputSchema": t.OutputSchema(),
			"annotations":  t.Annotations,
		})
	}
	return map[string]any{"tools": out}
}

// --- tools/call ----------------------------------------------------------------

// toolResult is the shape a tool call answers with.
type toolResult struct {
	Content           []any          `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError"`
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// errorResult is section 8.2's isError semantics: **a domain failure is a
// result, not a JSON-RPC error**, carrying the REST error envelope's error in
// `structuredContent.error`.
//
// The split matters because many MCP clients surface a JSON-RPC error as a
// transport failure and never hand it to the model. A `not_found` reported
// that way is a fact the model never learns and cannot correct itself from.
func errorResult(e *apierr.Error) toolResult {
	body := e.Envelope("").Error
	details := body.Details
	if details == nil {
		details = map[string]any{}
	}
	return toolResult{
		Content: []any{textBlock(e.Message)},
		StructuredContent: map[string]any{
			"error": map[string]any{
				"code":      string(body.Code),
				"message":   body.Message,
				"retryable": body.Retryable,
				"details":   details,
			},
		},
		IsError: true,
	}
}

// scopeRefusal is the refusal a tool call gets when the caller's
// authorization does not cover it.
//
// `insufficient_scope` appears on both sides of section 8.2 deliberately,
// addressed to different readers. The transport's 401/403 is addressed to the
// CLIENT, which can go and get a wider credential. This one is addressed to
// the MODEL, which cannot -- so it names `required_scope`, which the model
// can act on by choosing a different tool.
func scopeRefusal(scope authz.Scope) *apierr.Error {
	e := apierr.InsufficientScope(string(scope))
	e.Details["required_scope"] = string(scope)
	return e
}

func (s *session) toolsCall(req jsonrpcRequest) jsonrpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcFail(req.ID, codeInvalidParams, "the params of tools/call are malformed")
	}
	tool, ok := ToolByName(params.Name)
	if !ok {
		// An unknown tool NAME is a JSON-RPC error (section 8.2): the model
		// asked for something that does not exist, which is a protocol fact
		// rather than an answer about the owner's messages.
		return rpcFail(req.ID, codeInvalidParams, "no such tool: "+params.Name)
	}

	// The call-time scope re-check. `tools/list` already hid this tool from
	// a caller that may not use it, but visibility is not authorization and
	// a client may call a name it learned elsewhere.
	//
	// It is a RESULT rather than a transport refusal, because this one is
	// addressed to the model: it can act on it by choosing a different tool.
	if s.auth == nil || !s.auth.Scopes.Has(tool.Scope) {
		return rpcResult(req.ID, errorResult(scopeRefusal(tool.Scope)))
	}

	result, e := s.callTool(tool, params.Arguments)
	if e != nil {
		return rpcResult(req.ID, errorResult(e))
	}
	return rpcResult(req.ID, result)
}

// callTool decodes the arguments, invokes the route, and renders the result.
func (s *session) callTool(tool Tool, raw json.RawMessage) (toolResult, *apierr.Error) {
	args, e := decodeArgs(tool, raw)
	if e != nil {
		return toolResult{}, e
	}

	in, e := s.buildInvocation(tool, args)
	if e != nil {
		return toolResult{}, e
	}

	invoked, callErr := s.handler.cfg.API.Invoke(*in)
	if callErr != nil {
		return toolResult{}, callErr
	}

	structured, e := envelopeOf(invoked)
	if e != nil {
		return toolResult{}, e
	}

	summary := summarise(tool, structured)
	content := []any{textBlock(summary)}
	// `get_attachment` decides its content form by size and type, not
	// preference, and the summary text is the FIRST content block either
	// way -- so a client that reads only the first block reads a sentence
	// rather than a megabyte of base64.
	if tool.Name == "get_attachment" {
		content = append(content, s.attachmentBlocks(structured)...)
	}
	return toolResult{Content: content, StructuredContent: structured}, nil
}

// decodeArgs enforces the closed input schema **by rejecting unknown fields**
// rather than by declaring `additionalProperties: false` and hoping (spec
// section 8.2). A model that invents an argument name is told which one, in a
// result it can read.
func decodeArgs(tool Tool, raw json.RawMessage) (map[string]json.RawMessage, *apierr.Error) {
	args := map[string]json.RawMessage{}
	if len(raw) > 0 && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, apierr.MalformedBody("the tool arguments must be a JSON object")
		}
	}
	declared := tool.argNames()
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	// Deterministic: two unknown arguments in one call must always name the
	// same one, or the error a caller sees depends on Go's map iteration.
	sort.Strings(names)
	for _, name := range names {
		if !declared[name] {
			return nil, apierr.UnknownBodyField(name)
		}
	}
	for _, a := range tool.Args {
		if !a.Required {
			continue
		}
		if v, ok := args[a.Name]; !ok || isJSONNull(v) {
			e := apierr.Newf(apierr.CodeInvalidRequest, "%s is required", a.Name)
			e.Details = map[string]any{"field": a.Name}
			return nil, e
		}
	}
	return args, nil
}

func isJSONNull(raw []byte) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// buildInvocation maps the tool's arguments onto the route's path, query and
// body. It is the whole of the translation layer: there is no renaming, no
// defaulting and no filtering here, because every one of those belongs to the
// handler and would otherwise exist twice.
func (s *session) buildInvocation(tool Tool, args map[string]json.RawMessage) (*api.Invocation, *apierr.Error) {
	routeName := tool.Route
	if tool.Name == "remove_reaction" {
		var e *apierr.Error
		routeName, e = removeReactionRoute(args)
		if e != nil {
			return nil, e
		}
	}

	path := map[string]string{}
	query := url.Values{}
	body := map[string]json.RawMessage{}

	for _, a := range tool.Args {
		raw, present := args[a.Name]
		if !present || isJSONNull(raw) {
			continue
		}
		switch a.In {
		case InPath:
			v, e := scalarString(a.Name, raw)
			if e != nil {
				return nil, e
			}
			path[a.pathParam()] = v
		case InQuery:
			v, e := scalarString(a.Name, raw)
			if e != nil {
				return nil, e
			}
			query.Set(a.Name, v)
		case InBody:
			body[a.Name] = raw
		}
	}

	if tool.Name == "remove_reaction" {
		// The two routes take different path parameters, and passing the
		// other one would be an unknown path parameter rather than a
		// harmless extra.
		if routeName == "reactions_remove_by_id" {
			delete(path, "message_id")
			delete(path, "emoji")
		} else {
			delete(path, "reaction_id")
		}
	}

	if tool.Name == "get_session" {
		if _, ok := path["account_id"]; !ok {
			id, e := s.soleAccountID()
			if e != nil {
				return nil, e
			}
			path["account_id"] = id
		}
	}

	var encoded []byte
	if len(body) > 0 {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, apierr.Internal(err)
		}
	}

	return &api.Invocation{
		Ctx:       s.ctx,
		RouteName: routeName,
		Path:      path,
		Query:     query,
		Body:      encoded,
		Auth:      s.auth,
		Bearer:    s.bearer,
		Source:    s.source,
		RequestID: apierr.NewRequestID(),
	}, nil
}

// removeReactionRoute picks between the two routes section 8.2 gives
// `remove_reaction`. **Supplying neither address is `invalid_request`** rather
// than a guess: there is nothing to remove and nothing to infer.
func removeReactionRoute(args map[string]json.RawMessage) (string, *apierr.Error) {
	has := func(name string) bool {
		v, ok := args[name]
		return ok && !isJSONNull(v)
	}
	switch {
	case has("reaction_id"):
		return "reactions_remove_by_id", nil
	case has("message_id") && has("emoji"):
		return "reactions_remove", nil
	case has("message_id"):
		e := apierr.New(apierr.CodeInvalidRequest,
			"message_id names a message but not which reaction to remove; supply emoji as well, or supply reaction_id instead")
		e.Details = map[string]any{"field": "emoji"}
		return "", e
	default:
		e := apierr.New(apierr.CodeInvalidRequest,
			"supply either reaction_id, or message_id together with emoji")
		e.Details = map[string]any{"field": "reaction_id"}
		return "", e
	}
}

// soleAccountID resolves the account a `get_session` with no `account_id`
// means, under the section 7.3 rule: exactly one account is used implicitly,
// and more than one is `invalid_request` **listing the candidates**, so the
// caller can retry without a second round trip.
func (s *session) soleAccountID() (string, *apierr.Error) {
	invoked, e := s.handler.cfg.API.Invoke(api.Invocation{
		Ctx: s.ctx, RouteName: "accounts_list", Auth: s.auth,
		Bearer: s.bearer, Source: s.source, RequestID: apierr.NewRequestID(),
	})
	if e != nil {
		return "", e
	}
	rows, err := itemsOf(invoked.Response.Data)
	if err != nil {
		return "", apierr.Internal(err)
	}
	switch len(rows) {
	case 1:
		id, _ := rows[0]["id"].(string)
		return id, nil
	case 0:
		return "", apierr.New(apierr.CodeNotPaired,
			"this server holds no Google account yet, so there is no session to read")
	default:
		candidates := make([]any, 0, len(rows))
		for _, r := range rows {
			candidates = append(candidates, map[string]any{
				"id":             r["id"],
				"google_account": r["google_account"],
				"state":          r["state"],
			})
		}
		e := apierr.New(apierr.CodeInvalidRequest,
			"this server holds more than one account, so account_id says which session to read")
		e.Details = map[string]any{"field": "account_id", "accounts": candidates}
		return "", e
	}
}

// itemsOf re-reads a listing's rows through JSON. The DTOs are unexported --
// nothing outside internal/api constructs one -- so the wire form is the only
// shape available here, which is also the shape the caller will see.
func itemsOf(data any) ([]map[string]any, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Items, nil
}

// envelopeOf renders `structuredContent` = `{data, next_cursor, warnings}` --
// the REST envelope minus the request ID (spec section 8.2).
func envelopeOf(invoked *api.Invoked) (map[string]any, *apierr.Error) {
	raw, err := json.Marshal(invoked.Response.Data)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, apierr.Internal(err)
	}
	warnings := invoked.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	out := map[string]any{
		"data":     data,
		"warnings": warnings,
	}
	if invoked.Response.NextCursor != "" {
		out["next_cursor"] = invoked.Response.NextCursor
	} else {
		out["next_cursor"] = nil
	}
	return out, nil
}

// summarise is the one-line text half of a result.
//
// It says **what was returned and whether more exists**, so a model that reads
// only the text is not misled about completeness (spec section 8.2). A
// summary that said "3 conversations" on a page of 3 out of 400 would be
// true and useless.
func summarise(tool Tool, structured map[string]any) string {
	more := ""
	if c, ok := structured["next_cursor"].(string); ok && c != "" {
		more = " There are more; pass next_cursor back as cursor to read the next page."
	} else if tool.paginated() {
		more = " There are no more pages."
	}
	data := structured["data"]
	if obj, ok := data.(map[string]any); ok {
		if rows, ok := obj["items"].([]any); ok {
			return fmt.Sprintf("%s returned %d %s.%s", tool.Name, len(rows), rowNoun(tool, len(rows)), more)
		}
	}
	return tool.Name + " returned one " + objectNoun(tool) + "." + more
}

func rowNoun(tool Tool, n int) string {
	singular := map[string]string{
		"list_conversations": "conversation",
		"list_messages":      "message",
		"search_messages":    "matching message",
		"list_contacts":      "contact",
		"list_accounts":      "account",
	}[tool.Name]
	if singular == "" {
		singular = "row"
	}
	if n == 1 {
		return singular
	}
	return singular + "s"
}

func objectNoun(tool Tool) string {
	if n, ok := map[string]string{
		"get_conversation":    "conversation",
		"get_message":         "message",
		"message_context":     "message with its surrounding messages",
		"get_attachment":      "attachment, with a download ticket",
		"get_session":         "account session",
		"get_health":          "health report",
		"get_operation":       "operation",
		"create_upload":       "upload reservation",
		"send_message":        "send result",
		"start_conversation":  "conversation",
		"mark_read":           "result",
		"add_reaction":        "result",
		"remove_reaction":     "result",
		"update_conversation": "result",
		"delete_message":      "result",
		"delete_conversation": "result",
	}[tool.Name]; ok {
		return n
	}
	return "object"
}

// scalarString renders a JSON scalar as the string a query parameter or a
// path segment carries. An object or an array in a slot that takes a scalar
// is `invalid_request` naming the argument rather than a stringified struct.
func scalarString(name string, raw json.RawMessage) (string, *apierr.Error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", apierr.MalformedBody("the value of " + name + " is not valid JSON")
	}
	switch value := v.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10), nil
		}
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	default:
		e := apierr.Newf(apierr.CodeInvalidRequest, "%s takes a single value, not an object or an array", name)
		e.Details = map[string]any{"parameter": name}
		return "", e
	}
}
