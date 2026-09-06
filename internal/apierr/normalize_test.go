package apierr

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestNormalizeReason is spec section 7.1: a reason carrying control
// characters or over its bound is accepted, cleaned, and the change is
// reported. Normalisation is never silent.
func TestNormalizeReason(t *testing.T) {
	long := strings.Repeat("x", MaxReasonRunes+10)

	cases := []struct {
		name         string
		in           string
		want         string
		wantWarnings []string
	}{
		{"clean text passes untouched", "spam", "spam", nil},
		{"empty passes untouched", "", "", nil},
		{"newline is removed", "spam\nnot really", "spamnot really", []string{WarnReasonNormalized}},
		{"carriage return is removed", "spam\r\nX-Injected: yes", "spamX-Injected: yes", []string{WarnReasonNormalized}},
		{"NUL is removed", "spam\x00", "spam", []string{WarnReasonNormalized}},
		{"C1 control is removed", "spam\u0085more", "spammore", []string{WarnReasonNormalized}},
		{"surrounding whitespace is trimmed", "  spam  ", "spam", []string{WarnReasonNormalized}},
		{"over the bound is truncated", long, strings.Repeat("x", MaxReasonRunes), []string{WarnReasonTruncated}},
		{
			"both at once reports both",
			"\t" + long,
			strings.Repeat("x", MaxReasonRunes),
			[]string{WarnReasonNormalized, WarnReasonTruncated},
		},
		{"emoji survive", "🎉 party", "🎉 party", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := NormalizeReason(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !reflect.DeepEqual(warnings, tc.wantWarnings) {
				t.Errorf("warnings = %v, want %v", warnings, tc.wantWarnings)
			}
			if utf8.RuneCountInString(got) > MaxReasonRunes {
				t.Errorf("the result is %d runes, over the %d bound", utf8.RuneCountInString(got), MaxReasonRunes)
			}
		})
	}
}

// TestNormalizeFilename is the filename half of spec section 7.1, plus the
// directory part: a filename is a name, and "../../etc/passwd" reaching the
// media cache would be a path traversal.
func TestNormalizeFilename(t *testing.T) {
	long := strings.Repeat("n", MaxFilenameRunes+10)

	cases := []struct {
		name         string
		in           string
		want         string
		wantWarnings []string
	}{
		{"clean name passes untouched", "IMG_0421.jpg", "IMG_0421.jpg", nil},
		{"newline is removed", "IMG\n0421.jpg", "IMG0421.jpg", []string{WarnFilenameNormalized}},
		{"posix path is stripped to the name", "../../etc/passwd", "passwd", []string{WarnFilenameNormalized}},
		{"windows path is stripped to the name", `C:\Users\o\IMG.jpg`, "IMG.jpg", []string{WarnFilenameNormalized}},
		{"a bare separator becomes a name", "/", "file", []string{WarnFilenameNormalized}},
		{"empty becomes a name", "", "file", []string{WarnFilenameNormalized}},
		{"dot becomes a name", ".", "file", []string{WarnFilenameNormalized}},
		{"over the bound is truncated", long, strings.Repeat("n", MaxFilenameRunes), []string{WarnFilenameTruncated}},
		{
			"both at once reports both",
			"dir/" + long,
			strings.Repeat("n", MaxFilenameRunes),
			[]string{WarnFilenameNormalized, WarnFilenameTruncated},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := NormalizeFilename(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !reflect.DeepEqual(warnings, tc.wantWarnings) {
				t.Errorf("warnings = %v, want %v", warnings, tc.wantWarnings)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("the result %q still carries a path separator", got)
			}
			if got == "" {
				t.Error("a filename must never normalise to the empty string")
			}
		})
	}
}

// TestNormalizeDropsInvalidUTF8ButKeepsALiteralReplacementCharacter proves
// the result is always storable and always renderable, without silently
// rewriting a caller's own U+FFFD.
func TestNormalizeDropsInvalidUTF8ButKeepsALiteralReplacementCharacter(t *testing.T) {
	got, warnings := NormalizeReason("bad\xffbyte")
	if got != "badbyte" {
		t.Errorf("got %q, want %q", got, "badbyte")
	}
	if !utf8.ValidString(got) {
		t.Error("the result is not valid UTF-8")
	}
	if len(warnings) == 0 {
		t.Error("dropping a byte must be reported")
	}

	got, warnings = NormalizeReason("literal \ufffd here")
	if got != "literal \ufffd here" {
		t.Errorf("a literal U+FFFD was rewritten: %q", got)
	}
	if len(warnings) != 0 {
		t.Errorf("nothing changed, so there must be no warning: %v", warnings)
	}
}

// TestWarningStringsAreTheSpecsWords pins the four warning strings of spec
// section 7.1.
func TestWarningStringsAreTheSpecsWords(t *testing.T) {
	pairs := map[string]string{
		WarnReasonNormalized:   "reason_normalized",
		WarnReasonTruncated:    "reason_truncated",
		WarnFilenameNormalized: "filename_normalized",
		WarnFilenameTruncated:  "filename_truncated",
	}
	if len(pairs) != 4 {
		t.Fatalf("two warning constants collided; there must be four distinct strings")
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("warning %q, want %q", got, want)
		}
	}
}
