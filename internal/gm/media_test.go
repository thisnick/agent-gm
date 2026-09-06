package gm_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Agent GM validates a MIME type at RESERVATION time with exactly the rule
// libgm.UploadMedia applies at SEND time (spec section 10.2), so a
// reservation that would be accepted here and refused at the send -- or the
// reverse -- is impossible. This walks upstream's own table rather than a
// sample, so a pin bump that removes a type fails here rather than at a
// live send.
func TestSupportedMIMEAcceptsEveryTypeUpstreamNames(t *testing.T) {
	types := gm.SupportedMIMETypes()
	if len(types) < 10 {
		t.Fatalf("upstream's table has %d entries, which is not plausible", len(types))
	}
	for _, mime := range types {
		if !gm.SupportedMIME(mime) {
			t.Errorf("upstream's table names %q and SupportedMIME refuses it", mime)
		}
	}
}

// The type-prefix fallback is upstream's, not Agent GM's invention: a type
// the table does not name is retried with the part before the slash
// (pkg/libgm/media.go at the pin).
func TestSupportedMIMEFollowsUpstreamsPrefixFallback(t *testing.T) {
	// image is a prefix upstream's table carries, so an unnamed image type
	// is accepted through it.
	if !gm.SupportedMIME("image/heif") {
		t.Error("image/heif was refused; the type-prefix fallback is upstream's own")
	}
	if !gm.SupportedMIME("image/jpeg") {
		t.Error("image/jpeg was refused")
	}
	for _, unsupported := range []string{
		"", "notamime", "/", "chemical/x-pdb", "x-conference/x-cooltalk",
	} {
		if gm.SupportedMIME(unsupported) {
			t.Errorf("%q was accepted", unsupported)
		}
	}
}

// A parameterised type must find its exact row rather than being downgraded
// to its prefix. "text/plain; charset=utf-8" is text/plain, and reading it as
// bare "text" would pick a different media type than the send will.
func TestParametersAreStrippedBeforeTheLookup(t *testing.T) {
	if !gm.SupportedMIME("text/plain; charset=utf-8") {
		t.Error("a parameterised text/plain was refused")
	}
	if !gm.SupportedMIME("  IMAGE/JPEG  ") {
		t.Error("a type differing only in case and whitespace was refused")
	}
}

// The refusal message can say how many types are supported without a number
// that would go stale at the next pin bump.
func TestSupportedMIMETypesIsUsableInAMessage(t *testing.T) {
	types := gm.SupportedMIMETypes()
	joined := strings.Join(types, " ")
	for _, want := range []string{"image", "video", "audio"} {
		if !strings.Contains(joined, want) {
			t.Errorf("upstream's table names no %s type at all: %v", want, types)
		}
	}
}
