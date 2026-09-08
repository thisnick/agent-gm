package oauth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallbackFormAction(t *testing.T) {
	for _, tc := range []struct{ callback, source string }{
		{"https://client.example/callback?state=ignored", "https://client.example"},
		{"http://127.0.0.1:1234/callback", "http://127.0.0.1:1234"},
		{"com.example.app:/callback", "com.example.app:"},
		{"https://client.example;script-src/callback", ""},
		{"https://client.example,evil.example/callback", ""},
	} {
		t.Run(tc.callback, func(t *testing.T) {
			w := httptest.NewRecorder()
			setPageSecurityHeaders(w)
			original := w.Header().Get("Content-Security-Policy")
			allowCallbackFormAction(w, tc.callback)
			want := original
			if tc.source != "" {
				want = strings.Replace(original, "form-action 'self'", "form-action 'self' "+tc.source, 1)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != want {
				t.Fatalf("CSP = %q, want %q", got, want)
			}
		})
	}
}
