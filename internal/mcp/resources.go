package mcp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/media"
)

// Resources (spec section 8.2).
//
// `GET /v1/attachments/{id}/content` is served as a **resource**, not as a
// tool: bytes belong in a resource so a client can fetch them without putting
// them through the model's context. The limit and the cache path are the REST
// route's, because it IS the REST route -- `resources/read` runs the same
// handler through api.Server.Invoke.
//
// `resources/list` is empty on purpose. Attachments are addressed by template,
// not enumerated: a server that listed every attachment it holds would answer
// a question nobody asked with a page nobody can page through.

// AttachmentURIPrefix is the resource scheme and path.
const AttachmentURIPrefix = "agm://attachments/"

// AttachmentURITemplate is what `resources/templates/list` offers.
const AttachmentURITemplate = "agm://attachments/{attachment_id}"

func (s *session) resourceTemplates() map[string]any {
	// Offered **only** to a caller holding `messages:read`. A template a
	// caller may not read is an invitation to a refusal.
	if s.auth == nil || !s.auth.Scopes.Has(authz.ScopeMessagesRead) {
		return map[string]any{"resourceTemplates": []any{}}
	}
	return map[string]any{"resourceTemplates": []any{
		map[string]any{
			"uriTemplate": AttachmentURITemplate,
			"name":        "attachment",
			"title":       "Message attachment bytes",
			"description": "The bytes of one attachment, addressed by the `att_` ID that appears on a message. " +
				"Text media comes back as text and everything else as base64. " +
				"Fetching bytes this way keeps them out of the model's context; `get_attachment` returns the metadata and a download ticket.",
			"mimeType": "application/octet-stream",
		},
	}}
}

func (s *session) resourcesRead(req jsonrpcRequest) jsonrpcResponse {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcFail(req.ID, codeInvalidParams, "the params of resources/read are malformed")
	}
	if s.auth == nil || !s.auth.Scopes.Has(authz.ScopeMessagesRead) {
		return rpcResult(req.ID, errorResult(scopeRefusal(authz.ScopeMessagesRead)))
	}
	id, ok := strings.CutPrefix(params.URI, AttachmentURIPrefix)
	if !ok || id == "" {
		return rpcResult(req.ID, errorResult(apierr.NotFound("resource")))
	}

	body, contentType, e := s.attachmentBytes(id)
	if e != nil {
		return rpcResult(req.ID, errorResult(e))
	}

	contents := map[string]any{"uri": params.URI, "mimeType": contentType}
	if isTextMedia(contentType) {
		contents["text"] = string(body)
	} else {
		contents["blob"] = base64.StdEncoding.EncodeToString(body)
	}
	return rpcResult(req.ID, map[string]any{"contents": []any{contents}})
}

// attachmentBytes runs the REST content route in process, so the media limit,
// the cache path, the ticket accounting and the sanitised content type are the
// ones section 10.3 already fixes rather than a second copy of them.
func (s *session) attachmentBytes(attachmentID string) ([]byte, string, *apierr.Error) {
	invoked, e := s.handler.cfg.API.Invoke(api.Invocation{
		Ctx:       s.ctx,
		RouteName: "attachments_content",
		Path:      map[string]string{"attachment_id": attachmentID},
		Auth:      s.auth,
		Bearer:    s.bearer,
		Source:    s.source,
		RequestID: apierr.NewRequestID(),
	})
	if e != nil {
		return nil, "", e
	}
	if invoked.Response.Raw == nil {
		return nil, "", apierr.New(apierr.CodeInternalError, "the attachment route answered no bytes")
	}
	rec := &recorder{header: http.Header{}}
	invoked.Response.Raw(rec)
	contentType := invoked.Response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = media.OctetStream
	}
	return rec.body, contentType, nil
}

// isTextMedia reports whether the SERVED content type is text. It is the
// served type rather than the sender's, because SVG, HTML and every
// unrecognised type are served as `application/octet-stream` (section 10.3) --
// a message from anybody can carry an attachment, and returning one as text
// because its sender called it `text/html` would put a remote party's markup
// into a model's context as prose.
func isTextMedia(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(media)), "text/")
}

// recorder captures a Raw response's bytes. The REST transport writes them to
// a socket; here there is no socket.
type recorder struct {
	header http.Header
	body   []byte
	status int
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(p []byte) (int, error) {
	r.body = append(r.body, p...)
	return len(p), nil
}
func (r *recorder) WriteHeader(status int) { r.status = status }

// --- get_attachment's content blocks ------------------------------------------

// attachmentBlocks is the size-and-type decision of section 8.2.
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
func (s *session) attachmentBlocks(structured map[string]any) []any {
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
			return []any{map[string]any{
				"type":     "image",
				"data":     base64.StdEncoding.EncodeToString(body),
				"mimeType": contentType,
			}}
		}
		// Falling through to the link is the right answer when the bytes
		// cannot be had: the caller still gets an address and a ticket.
	}
	name := filename
	if name == "" {
		name = id
	}
	return []any{map[string]any{
		"type":     "resource_link",
		"uri":      uri,
		"name":     name,
		"mimeType": served,
		"description": "The bytes of this attachment. Read it as a resource, or run the `curl` command in " +
			"`data.curl`; either way the bytes stay out of the model's context.",
	}}
}
