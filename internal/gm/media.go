package gm

import (
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// SupportedMIME reports whether Google Messages will carry this content type,
// using upstream's own table and upstream's own fallback.
//
// `libgm.UploadMedia` looks a MIME type up in `MimeToMediaType` and, on a
// miss, retries with the part before the slash -- so `image/heif` is accepted
// as `image` even though the table names no such row (`pkg/libgm/media.go`
// at the pin). Agent GM validates at **reservation** time with exactly that
// rule, so a reservation that would be accepted here and refused at send time
// -- or the reverse -- is impossible. An unsupported type is
// `media_unsupported_type` (415) and reserves nothing (spec section 10.2).
//
// This function exists because nothing above `internal/gm` may import `libgm`
// (spec section 2.3): the media layer asks Agent GM's own vocabulary, and the
// answer still comes from upstream's table rather than from a copy of it that
// could drift at the next pin bump.
func SupportedMIME(mime string) bool {
	return canonicalMediaType(mime) != 0
}

// canonicalMediaType returns upstream's numeric media type for a MIME string,
// or 0 when there is none. It follows the same two steps UploadMedia does.
func canonicalMediaType(mime string) int32 {
	mime = strings.ToLower(strings.TrimSpace(mime))
	// A parameterised type ("text/plain; charset=utf-8") is not in the table
	// and its prefix is, which would silently downgrade it. The parameters
	// are dropped first so the exact row is found.
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mt, ok := libgm.MimeToMediaType[mime]; ok && mt.Type != 0 {
		return int32(mt.Type)
	}
	prefix, _, found := strings.Cut(mime, "/")
	if !found || prefix == "" {
		return 0
	}
	if mt, ok := libgm.MimeToMediaType[prefix]; ok {
		return int32(mt.Type)
	}
	return 0
}

// SupportedMIMETypes returns every MIME string upstream's table names, sorted
// by nothing in particular. It is here so a test can walk the real table
// rather than a handful of samples, and so `media_unsupported_type`'s message
// can say how many types are supported without hard-coding a number that
// would go stale at the next pin bump.
func SupportedMIMETypes() []string {
	out := make([]string, 0, len(libgm.MimeToMediaType))
	for k := range libgm.MimeToMediaType {
		out = append(out, k)
	}
	return out
}
