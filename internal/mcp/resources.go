package mcp

import (
	"net/http"
	"strings"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
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
