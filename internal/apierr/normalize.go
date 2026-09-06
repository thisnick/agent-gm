package apierr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The warning strings of spec section 7.1. Text normalisation is never
// silent: a reason or a filename that had to be cleaned or shortened is
// accepted and the change is reported in the envelope's warnings array, so a
// caller never has to diff what it sent against what came back to discover
// that something changed.
const (
	WarnReasonNormalized   = "reason_normalized"
	WarnReasonTruncated    = "reason_truncated"
	WarnFilenameNormalized = "filename_normalized"
	WarnFilenameTruncated  = "filename_truncated"
)

// The length bounds. Spec section 7.1 requires a bound and a warning without
// naming a number, so the numbers are fixed here, once, and every surface
// reads them from here rather than choosing its own.
const (
	// MaxReasonRunes bounds a human-supplied reason, which is stored and
	// shown but never parsed.
	MaxReasonRunes = 500
	// MaxFilenameRunes is the conventional filesystem name bound; the name
	// reaches a filename on disk and a Content-Disposition header.
	MaxFilenameRunes = 255
)

// NormalizeReason cleans a caller-supplied reason and returns it with the
// warnings that describe what changed. Control characters are removed --
// a newline in a reason that is later logged is a log-injection vector --
// and surrounding whitespace is trimmed.
func NormalizeReason(reason string) (string, []string) {
	var warnings []string

	cleaned := strings.TrimSpace(removeControl(reason))
	if cleaned != reason {
		warnings = append(warnings, WarnReasonNormalized)
	}

	truncated := truncateRunes(cleaned, MaxReasonRunes)
	if truncated != cleaned {
		warnings = append(warnings, WarnReasonTruncated)
	}
	return truncated, warnings
}

// NormalizeFilename cleans a caller-supplied filename and returns it with the
// warnings that describe what changed. Besides control characters it strips
// any directory part: a filename is a name, and "../../etc/passwd" reaching
// the media cache would be a path traversal. An empty result becomes
// "file", because a nameless attachment cannot be served.
func NormalizeFilename(filename string) (string, []string) {
	var warnings []string

	cleaned := removeControl(filename)
	// Both separators, since the name may have been produced on Windows.
	if i := strings.LastIndexAny(cleaned, `/\`); i >= 0 {
		cleaned = cleaned[i+1:]
	}
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		cleaned = "file"
	}
	if cleaned != filename {
		warnings = append(warnings, WarnFilenameNormalized)
	}

	truncated := truncateRunes(cleaned, MaxFilenameRunes)
	if truncated != cleaned {
		warnings = append(warnings, WarnFilenameTruncated)
	}
	return truncated, warnings
}

// removeControl drops every control rune, including the C1 block, and every
// byte that is not valid UTF-8. Invalid UTF-8 is dropped rather than replaced
// with U+FFFD so the result is always storable and always renderable.
func removeControl(s string) string {
	if isClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		if r == utf8.RuneError && width == 1 {
			// A decoding failure. A literal U+FFFD in the input decodes with
			// width 3 and is kept; the two are told apart by the width alone.
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isClean(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// truncateRunes cuts s to at most n runes, never mid-rune.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
