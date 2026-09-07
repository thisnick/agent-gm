package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/media"
)

// The protocol layer is the official MCP Go SDK (owner decision D36).
//
// What the SDK decides from here on: JSON-RPC framing and error objects,
// session lifecycle and resumability, protocol-version negotiation, the
// `tools/list` and `tools/call` wire shapes, `resources/list`,
// `resources/templates/list` and `resources/read`, and the RFC 9728 bearer
// challenge. What stays ours: the catalogue in tools.go, every schema and
// description in it, the translation onto the REST routes through
// api.Server.Invoke, the isError envelope of section 8.2, scope gating, the
// section 8.1 Origin check, and the concurrency budget.
//
// Nothing here reimplements the API. Every tool call still runs the REST
// handler, so the filters, the validation, the error codes and the DTOs are
// the same on both surfaces by construction.

// MetaKeyCommit and MetaKeySourceURL carry the AGPL section 13 obligation of
// spec section 1.4 on the MCP surface.
//
// They live in the `initialize` result's `_meta` rather than in `serverInfo`
// because the SDK's own `serverInfo` type -- mcp.Implementation -- has fields
// for a name, a version, a title, a description, icons and a website URL, and
// none for a build commit. Inventing a `serverInfo.commit` would mean
// hand-rolling the one result the SDK exists to render. `_meta` is the
// protocol's own extension point and takes reverse-DNS keys, so this is where
// a server puts a fact the schema does not have a field for.
const (
	MetaKeyCommit    = "app.agent-gm/commit"
	MetaKeySourceURL = "app.agent-gm/source_url"
)

// ServerName is `serverInfo.name`.
const ServerName = "agent-gm"

// ServerVersion is `serverInfo.version`: the release version with the built
// commit as semver build metadata, `0.1.0+abc1234`.
//
// The commit rides in the version string because there is nowhere else it can
// go that a real client will read. `mcp.Implementation` -- the SDK's
// `serverInfo` -- has no field for a build commit, and `_meta` does not
// survive: from protocol 2026-07-28 a client discovers the server with
// `server/discover`, and the SDK's own client throws away every `_meta` key
// of that result except `io.modelcontextprotocol/serverInfo`
// (mcp/client.go:428-441, `decodeMetaValue`). So a build fact placed in
// `_meta` reaches a raw HTTP caller and no reference client at all, which
// would leave section 1.4's AGPL obligation unmet on the surface that
// matters. Build metadata after a `+` is what semver has for exactly this,
// and `serverInfo.websiteUrl` carries the source URL beside it.
//
// The `_meta` keys are still written, for a caller reading the wire directly.
func ServerVersion(version, commit string) string {
	if commit == "" {
		return version
	}
	return version + "+" + commit
}

// servers caches one SDK server per scope set.
//
// `tools/list` returns only the tools the calling authorization may use
// (section 8.2), and the SDK holds its tool set on the server rather than
// deciding it per request -- so the scope set picks the server. There are at
// most seven of them, one per non-empty subset of the three messaging scopes,
// and they are built once each.
type servers struct {
	mu sync.Mutex
	by map[string]*sdk.Server
}

// serverFor returns the SDK server that serves exactly the tools this
// authorization may use.
func (h *Handler) serverFor(auth *authz.Authorization) *sdk.Server {
	key := scopeKey(auth)
	h.servers.mu.Lock()
	defer h.servers.mu.Unlock()
	if h.servers.by == nil {
		h.servers.by = map[string]*sdk.Server{}
	}
	if s, ok := h.servers.by[key]; ok {
		return s
	}
	s := h.buildServer(auth)
	h.servers.by[key] = s
	return s
}

// scopeKey is the identity of a scope set: the sorted messaging scopes the
// authorization holds, and nothing else. Two authorizations with the same
// scopes see the same tools, so they share a server.
func scopeKey(auth *authz.Authorization) string {
	held := make([]string, 0, len(messagingScopes))
	for _, s := range messagingScopes {
		if auth != nil && auth.Scopes.Has(s) {
			held = append(held, string(s))
		}
	}
	sort.Strings(held)
	return strings.Join(held, " ")
}

// buildServer registers the visible tools and, under `messages:read`, the
// attachment resource template.
func (h *Handler) buildServer(auth *authz.Authorization) *sdk.Server {
	impl := &sdk.Implementation{
		Name:    ServerName,
		Version: ServerVersion(h.cfg.Version, h.cfg.Commit),
		// WebsiteURL is the SDK's own field for "where this implementation
		// lives", which is exactly what section 1.4 asks a deployment to say,
		// and in a stamped build it is the tree at the built commit.
		WebsiteURL: h.cfg.SourceURL,
	}
	srv := sdk.NewServer(impl, &sdk.ServerOptions{
		Instructions: Instructions,
	})

	// The build facts ride in `_meta` on the initialize result. A receiving
	// middleware is the SDK's hook for adding to a result it renders itself.
	commit, sourceURL := h.cfg.Commit, h.cfg.SourceURL
	srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			// Both handshakes, because there are two. From protocol
			// 2026-07-28 a client discovers the server with `server/discover`
			// (SEP-2575) and never calls `initialize` at all -- the SDK's own
			// client does exactly that (mcp/client.go:318). Stamping only
			// `initialize` would put section 1.4's build facts on the
			// handshake nobody uses.
			switch r := res.(type) {
			case *sdk.InitializeResult:
				r.Meta = stampBuild(r.Meta, commit, sourceURL)
			case *sdk.DiscoverResult:
				r.Meta = stampBuild(r.Meta, commit, sourceURL)
			}
			return res, nil
		}
	})

	srv.AddReceivingMiddleware(scopeGate(auth))

	// `tools/list` returns only the tools this authorization may use, and it
	// is the SDK's own list method that does it: an unheld tool is not
	// registered on this server at all, so a model is never invited to
	// attempt something that will be refused.
	for _, tool := range VisibleTools(auth) {
		srv.AddTool(sdkTool(tool), h.toolHandler(tool))
	}

	// The attachment template is registered on every server, including the
	// ones whose caller may not read it, and then hidden from
	// `resources/templates/list` by scopeGate.
	//
	// The alternative -- registering it only under `messages:read` -- makes
	// the server advertise no `resources` capability at all to a write-only
	// caller, and an official SDK client then fails a `resources/read`
	// at the capability check with a transport error rather than receiving
	// the refusal. Registering it and hiding it is what lets the refusal be
	// the clean `resources/read` failure section 8.2 asks for.
	//
	// `resources/list` stays empty because no plain resource is registered:
	// attachments are addressed by template, not enumerated.
	srv.AddResourceTemplate(attachmentTemplate(), h.resourceHandler())
	return srv
}

// scopeGate is the half of section 8.2's scope gating the SDK's registration
// cannot express.
//
// Two things happen here. A `tools/call` naming a REAL tool this caller may
// not use is answered with the isError RESULT of section 8.2 -- naming
// `required_scope`, which the model can act on by choosing a different tool --
// instead of the SDK's `unknown tool` JSON-RPC error, which the model would
// never see. A name that is in no catalogue at all still falls through to the
// SDK, because that one really is a protocol fact. And
// `resources/templates/list` is emptied for a caller without `messages:read`,
// because section 8.2 offers the template **only** to a caller holding it: a
// template a caller may not read is an invitation to a refusal.
func scopeGate(auth *authz.Authorization) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			switch method {
			case "tools/call":
				// The SERVER side sees CallToolParamsRaw, whose Arguments
				// are still json.RawMessage; CallToolParams is the client's
				// type. Asserting the wrong one here fails silently -- the
				// gate would simply never fire -- so the assertion is
				// checked rather than ignored.
				params, ok := req.GetParams().(*sdk.CallToolParamsRaw)
				if !ok {
					return nil, fmt.Errorf("tools/call params are %T, not *mcp.CallToolParamsRaw", req.GetParams())
				}
				tool, known := ToolByName(params.Name)
				if known && (auth == nil || !auth.Scopes.Has(tool.Scope)) {
					return errorResult(scopeRefusal(tool.Scope)), nil
				}
			case "resources/templates/list":
				if auth == nil || !auth.Scopes.Has(authz.ScopeMessagesRead) {
					return &sdk.ListResourceTemplatesResult{
						ResourceTemplates: []*sdk.ResourceTemplate{},
					}, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

// stampBuild puts the AGPL section 13 facts of section 1.4 into a handshake
// result's `_meta`.
func stampBuild(meta sdk.Meta, commit, sourceURL string) sdk.Meta {
	if meta == nil {
		meta = sdk.Meta{}
	}
	meta[MetaKeyCommit] = commit
	meta[MetaKeySourceURL] = sourceURL
	return meta
}

// sdkTool renders one catalogue entry as the SDK's Tool.
//
// The schemas are ours verbatim: the input schema built from the catalogue's
// arguments and the output schema reflected off the DTO the REST handler
// really serialises. The SDK carries them through to the wire unchanged,
// which is what keeps section 8.2's schema rules -- the closed object, the
// per-argument description, the `oneOf` of `const`s that is the only place
// JSON Schema lets a per-value description live -- expressible at all.
func sdkTool(t Tool) *sdk.Tool {
	return &sdk.Tool{
		Name:         t.Name,
		Description:  t.Description,
		InputSchema:  t.InputSchema(),
		OutputSchema: t.OutputSchema(),
		Annotations:  sdkAnnotations(t.Annotations),
	}
}

// sdkAnnotations is section 8.2's table.
//
// All four hints are serialised, never omitted, because `false` is a claim
// and an absent key is not: a client that had to guess whether a missing
// `destructiveHint` meant "no" or "unstated" would guess wrong on the tools
// where it matters. The SDK writes `readOnlyHint` and `idempotentHint`
// always, and the other two whenever the pointer is set, so setting both
// pointers unconditionally is what produces all four.
func sdkAnnotations(a Annotations) *sdk.ToolAnnotations {
	destructive, openWorld := a.DestructiveHint, a.OpenWorldHint
	return &sdk.ToolAnnotations{
		ReadOnlyHint:    a.ReadOnlyHint,
		IdempotentHint:  a.IdempotentHint,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

func attachmentTemplate() *sdk.ResourceTemplate {
	return &sdk.ResourceTemplate{
		URITemplate: AttachmentURITemplate,
		Name:        "attachment",
		Title:       "Message attachment bytes",
		Description: "The bytes of one attachment, addressed by the `att_` ID that appears on a message. " +
			"Text media comes back as text and everything else as base64. " +
			"Fetching bytes this way keeps them out of the model's context; `get_attachment` returns the metadata and a download ticket.",
		MIMEType: media.OctetStream,
	}
}

// --- tools/call ---------------------------------------------------------------

// toolHandler is the SDK-side handler for one tool.
//
// It returns a RESULT for every domain failure and an error only for a
// protocol failure, which is section 8.2's isError rule stated in the SDK's
// own terms: a non-nil error from a ToolHandler becomes a JSON-RPC error, and
// many MCP clients surface one as a transport failure and never hand it to
// the model. A `not_found` reported that way is a fact the model never learns
// and cannot correct itself from.
func (h *Handler) toolHandler(tool Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		s, err := h.sessionFor(ctx, req.Extra)
		if err != nil {
			return nil, err
		}

		// The call-time scope re-check. The server this request reached was
		// chosen by the caller's scopes, so an unheld tool is not registered
		// on it and the SDK has already answered `unknown tool`. This check
		// is the belt: it keeps the refusal correct if a future change ever
		// registers a tool more widely than it gates it, and it is the
		// refusal addressed to the MODEL -- a result naming `required_scope`,
		// which the model can act on by choosing a different tool.
		if s.auth == nil || !s.auth.Scopes.Has(tool.Scope) {
			return errorResult(scopeRefusal(tool.Scope)), nil
		}

		var raw json.RawMessage
		if req.Params != nil {
			raw = req.Params.Arguments
		}
		result, e := s.callTool(tool, raw)
		if e != nil {
			return errorResult(e), nil
		}
		return result, nil
	}
}

// errorResult is section 8.2's isError semantics: **a domain failure is a
// result, not a JSON-RPC error**, carrying the REST error envelope's error in
// `structuredContent.error`.
func errorResult(e *apierr.Error) *sdk.CallToolResult {
	body := e.Envelope("").Error
	details := body.Details
	if details == nil {
		details = map[string]any{}
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: e.Message}},
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

// --- resources/read -------------------------------------------------------------

// resourceHandler serves `agm://attachments/{attachment_id}` (section 8.2).
//
// Every failure here is a JSON-RPC ERROR, not an isError result, and that is
// not an exception to section 8.2's isError rule -- it is the rule's scope.
// The rule is about `tools/call`, whose result type has an `isError` field.
// A `ReadResourceResult` has no such field and MUST carry `contents`, so a
// "result" reporting a failure is not a valid result at all: an official SDK
// client rejects it on schema before the client's own code runs. The model
// learns nothing either way; the only difference is whether the CLIENT gets a
// readable refusal or a parse error.
// A `resources/read` failure is the SDK's own `ResourceNotFoundError`, which
// is -32602 since v1.7.0 per SEP-2164 (it was -32002 before). The SDK is the
// authority for protocol codes, and the reason it can be here is that this
// server does not adopt the SDK's SEP-2575 HTTP status mapping: -32602 would
// otherwise be delivered as HTTP 400, which the SDK's own client treats as a
// connection failure. See envelopeWriter.finish in handler.go.
func (h *Handler) resourceHandler() sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		s, err := h.sessionFor(ctx, req.Extra)
		if err != nil {
			return nil, err
		}
		uri := req.Params.URI
		// A caller without `messages:read` never sees the template, so it
		// reaches this handler only by guessing the URI. To that caller the
		// resource does not exist, and saying so is also the answer that
		// leaks least.
		if s.auth == nil || !s.auth.Scopes.Has(authz.ScopeMessagesRead) {
			return nil, sdk.ResourceNotFoundError(uri)
		}
		id, ok := strings.CutPrefix(uri, AttachmentURIPrefix)
		if !ok || id == "" {
			return nil, sdk.ResourceNotFoundError(uri)
		}

		body, contentType, e := s.attachmentBytes(id)
		if e != nil {
			if e.Code == apierr.CodeNotFound {
				return nil, sdk.ResourceNotFoundError(uri)
			}
			// A coded error, not a bare one. `jsonrpc2.toWireError` gives an
			// uncoded error the code **0**, which is not a JSON-RPC error
			// code at all and tells a client nothing it can branch on.
			return nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeInternalError,
				Message: "the resource could not be read: " + string(e.Code),
			}
		}

		contents := &sdk.ResourceContents{URI: uri, MIMEType: contentType}
		if isTextMedia(contentType) {
			contents.Text = string(body)
		} else {
			contents.Blob = body
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{contents}}, nil
	}
}

// --- the per-request session ------------------------------------------------------

// sessionFor recovers the authorization this request was authenticated with.
//
// It comes from the SDK's own per-request `Extra.TokenInfo`, not from the
// session, and that distinction is the security property: a streamable HTTP
// session spans many POSTs, and each one carries its own bearer. Reading the
// authorization off the session would let the first request's authority
// outlive the credential that established it.
func (h *Handler) sessionFor(ctx context.Context, extra *sdk.RequestExtra) (*session, error) {
	if extra == nil || extra.TokenInfo == nil {
		return nil, fmt.Errorf("this request was not authenticated")
	}
	call, ok := extra.TokenInfo.Extra[callerKey].(*caller)
	if !ok || call == nil {
		return nil, fmt.Errorf("this request was not authenticated")
	}
	return &session{
		handler: h,
		auth:    call.auth,
		bearer:  call.bearer,
		source:  call.source,
		ctx:     ctx,
	}, nil
}

// caller is what the token verifier resolved, handed to the tool and resource
// handlers through the SDK's per-request `TokenInfo.Extra`.
//
// It is a pointer to an in-process struct rather than a set of JSON values
// because the bearer is in it: `TokenInfo.Extra` is `map[string]any` and the
// SDK never serialises it, so nothing here reaches a wire or a log.
type caller struct {
	auth   *authz.Authorization
	bearer string
	source string
}

const callerKey = "app.agent-gm/caller"

// --- get_attachment's content blocks ----------------------------------------------

// attachmentContent is the size-and-type decision of section 8.2.
//
// `get_attachment` decides its content form by **size and type, not
// preference**: a supported image under `settings.media.inline_mcp_image_max_bytes`
// comes back as image content in the result; anything larger or non-inlinable
// comes back as an `agm://attachments/{id}` resource link. **The download
// ticket comes back either way**, in `structuredContent.data`, so a client is
// never left without a way to fetch the bytes.
//
// The summary text is the first content block, which is why this returns the
// blocks that follow it rather than the whole list.
func (s *session) attachmentContent(structured map[string]any) []sdk.Content {
	data, _ := structured["data"].(map[string]any)
	if data == nil {
		return nil
	}
	id, _ := data["attachment_id"].(string)
	uri, _ := data["resource_uri"].(string)
	if uri == "" {
		uri = AttachmentURIPrefix + id
	}
	mimeType, _ := data["mime_type"].(string)
	filename, _ := data["filename"].(string)
	inline, _ := data["inline"].(bool)
	available, _ := data["download_state"].(string)
	served := media.ServedContentType(mimeType)

	if inline && strings.HasPrefix(served, "image/") && available == "available" {
		if body, contentType, e := s.attachmentBytes(id); e == nil {
			return []sdk.Content{&sdk.ImageContent{
				Data:     body,
				MIMEType: contentType,
			}}
		}
		// Falling through to the link is the right answer when the bytes
		// cannot be had: the caller still gets an address and a ticket.
	}
	name := filename
	if name == "" {
		name = id
	}
	return []sdk.Content{&sdk.ResourceLink{
		URI:      uri,
		Name:     name,
		MIMEType: served,
		Description: "The bytes of this attachment. Read it as a resource, or run the `curl` command in " +
			"`data.curl`; either way the bytes stay out of the model's context.",
	}}
}

// --- protocol versions ------------------------------------------------------

// The protocol revisions this server speaks (spec section 8.1).
//
// Negotiation is the SDK's from decision D36 onwards: these constants say what
// the SDK is expected to agree to, and a test drives a real `initialize` to
// prove it still does, rather than a switch here deciding it.
//
// `2026-07-28` is available only because the transport is stateless: the SDK
// refuses every revision from it onwards on a stateful transport
// (`mcp/streamable.go:871`). The SDK also accepts three revisions older than
// section 8.1 names -- `2025-06-18`, `2025-03-26` and `2024-11-05`
// (`mcp/shared.go:58`) -- because supporting the clients still on them is the
// SDK's job and refusing them would be this package overriding the reference
// implementation for no gain.
const (
	// ProtocolVersion is what `initialize` negotiates to when the client asks
	// for it, asks for something newer, or asks for nothing.
	ProtocolVersion = "2026-07-28"
	// ProtocolVersionCompat is section 8.1's named compatibility revision.
	ProtocolVersionCompat = "2025-11-25"
)

// SupportedProtocolVersions is section 8.1's set, newest first.
var SupportedProtocolVersions = []string{ProtocolVersion, ProtocolVersionCompat}
