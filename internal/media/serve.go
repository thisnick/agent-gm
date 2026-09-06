package media

import "strings"

// How attachment bytes are handed to a caller (spec section 10.1).
//
// The rule is stated once, here, because it is the one place a mistake turns
// Agent GM's own origin into a place an attacker can host active content: a
// message from anybody can carry an attachment, the filename and the MIME
// type in it are the sender's to choose, and `GET /v1/attachments/{id}/content`
// serves those bytes from `AGENT_GM_PUBLIC_URL`. If that response ever said
// `image/svg+xml` or `text/html`, a hostile sender would have a stored XSS on
// the owner's own domain, delivered by a route the owner's agent is expected
// to fetch.
//
// So three things travel together on every content response, and each of them
// is load-bearing on its own:
//
//   - `Content-Disposition: attachment`, so a browser saves rather than
//     renders;
//   - `X-Content-Type-Options: nosniff`, so a browser does not overrule the
//     declared type by looking at the bytes;
//   - a sandboxing `Content-Security-Policy`, so anything that does get
//     rendered has no script, no plugins and no origin of its own.
const (
	// ContentDisposition is the disposition every attachment is served with.
	ContentDisposition = "attachment"

	// ContentSecurityPolicy sandboxes the response. `default-src 'none'`
	// forbids every subresource; `sandbox` drops scripts, forms, plugins and
	// same-origin, so even a type that slipped through renders inert.
	ContentSecurityPolicy = "default-src 'none'; sandbox; frame-ancestors 'none'; base-uri 'none'"

	// OctetStream is what every type not on the served list becomes.
	OctetStream = "application/octet-stream"
)

// servedTypes is the closed set of content types Agent GM will repeat back to
// a caller. It is an ALLOWLIST rather than a blocklist of svg and html,
// deliberately: a blocklist is a list of the attacks somebody thought of, and
// the next `image/svg+xml` -- `application/xhtml+xml`, `text/xml`,
// `application/mathml+xml`, a type invented after this was written -- would
// pass it. An allowlist fails closed, and failing closed here costs a caller
// nothing: `application/octet-stream` with the real filename in
// `Content-Disposition` is exactly what a program saving a file wants, and
// the true MIME type is served on the attachment's metadata anyway.
//
// Note what is NOT here: `image/svg+xml`, every `*+xml`, `text/html`,
// `application/pdf` (which carries JavaScript), and anything Google has no
// media type for in the first place.
var servedTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"image/heic": true,
	"image/heif": true,
	"image/avif": true,

	"audio/mpeg":  true,
	"audio/aac":   true,
	"audio/ogg":   true,
	"audio/wav":   true,
	"audio/amr":   true,
	"audio/mp4":   true,
	"audio/3gpp":  true,
	"audio/basic": true,

	"video/mp4":       true,
	"video/3gpp":      true,
	"video/webm":      true,
	"video/quicktime": true,
	"video/mpeg":      true,

	"text/plain": true,
	"text/vcard": true,
}

// ServedContentType is the `Content-Type` the bytes route sends for a stored
// MIME type.
//
// **SVG, HTML and every unrecognised type are served as
// `application/octet-stream`, never with their own type** (spec section 10.1).
// The stored value is not changed and is still served on the metadata route;
// only what this one response claims the bytes are is narrowed.
func ServedContentType(mime string) string {
	if servedTypes[normaliseMIME(mime)] {
		return normaliseMIME(mime)
	}
	return OctetStream
}

// normaliseMIME lowercases and drops parameters, so `IMAGE/JPEG` and
// `image/jpeg; charset=binary` are the row they are, and a caller cannot
// smuggle a second type past the allowlist in a parameter.
func normaliseMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return mime
}

// ContentTypeContradicted reports whether a declared MIME type and the type
// sniffed from the first bytes of a body disagree in a way that means the
// upload is not what it said it was.
//
// It is used at the end of a `PUT .../content` (spec section 10.2:
// "completion verifies the byte count, the declared digest, and the detected
// content type"). The comparison is deliberately coarse -- the top-level type
// only -- because sniffing is a heuristic and Agent GM should refuse a JPEG
// reservation filled with an HTML page, not quibble about `audio/mpeg` versus
// `audio/mp3`. An empty sniff result contradicts nothing: an unrecognisable
// body is not evidence of anything.
func ContentTypeContradicted(declared, sniffed string) bool {
	d, s := normaliseMIME(declared), normaliseMIME(sniffed)
	if d == "" || s == "" || s == OctetStream {
		return false
	}
	if d == s {
		return false
	}
	dTop, _, _ := strings.Cut(d, "/")
	sTop, _, _ := strings.Cut(s, "/")
	return dTop != sTop
}
