package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The origin check is the one refusal an operator has to be able to read in a
// production log: a browser that will not enroll looks, from the server side,
// exactly like a cross-site attempt, and the difference is the value in the
// header. These tests drive the check directly because the log line is the
// subject, and the fake `LogWarn` here is the same one `serve` wires to
// zerolog's warn level.

type capturedLine struct {
	msg string
	kv  map[string]any
}

func newOriginServer(warn *[]capturedLine) *Server {
	return &Server{cfg: Config{
		PublicURL: "https://gm.example.test",
		LogWarn: func(msg string, kv ...any) {
			line := capturedLine{msg: msg, kv: map[string]any{}}
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					line.kv[k] = kv[i+1]
				}
			}
			*warn = append(*warn, line)
		},
	}}
}

func postWithOrigin(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(
		"client_id=client_abc"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func clientIDOf(r *http.Request) func() string {
	return func() string { return r.PostFormValue("client_id") }
}

// TestCheckOriginRefusesNullAndSaysSo is the whole bug in one assertion:
// `Origin: null` is refused (a sandboxed frame and a cross-origin redirect
// both send it, so accepting it would be a hole), and the refusal names the
// value, at warn, with the request id the answer carries.
//
// Plant: return the old bare message from checkOrigin and this fails at "the
// description is"; drop the s.warnf call and it fails at "logged 0 lines".
func TestCheckOriginRefusesNullAndSaysSo(t *testing.T) {
	var warn []capturedLine
	s := newOriginServer(&warn)
	w := httptest.NewRecorder()
	r := postWithOrigin("null")

	oerr := s.checkOrigin(w, r, clientIDOf(r))
	if oerr == nil {
		t.Fatal("Origin: null was accepted. It is not an exemption: a sandboxed " +
			"frame posts with exactly that value")
	}
	if oerr.Status != http.StatusForbidden {
		t.Errorf("the refusal is %d, want 403", oerr.Status)
	}
	if !strings.Contains(oerr.Description, `received Origin "null"`) {
		t.Errorf("the description is %q, want it to name the Origin it received",
			oerr.Description)
	}
	if len(warn) != 1 {
		t.Fatalf("logged %d lines, want 1 warn line for the refusal", len(warn))
	}
	line := warn[0]
	if got := line.kv["received_origin"]; got != "null" {
		t.Errorf("the warn line's received_origin is %v, want null", got)
	}
	if got := line.kv["client_id"]; got != "client_abc" {
		t.Errorf("the warn line's client_id is %v, want client_abc", got)
	}
	id, _ := line.kv["request_id"].(string)
	if id == "" {
		t.Error("the warn line carries no request_id")
	}
	if got := w.Header().Get("X-Request-Id"); got != id {
		t.Errorf("the answer's X-Request-Id is %q and the log line's is %q; "+
			"they have to be one event", got, id)
	}
}

// TestCheckOriginPassesTheMatchingOriginQuietly: the matching origin is
// accepted and logs nothing, so the warn line means what it says.
func TestCheckOriginPassesTheMatchingOriginQuietly(t *testing.T) {
	var warn []capturedLine
	s := newOriginServer(&warn)
	r := postWithOrigin(s.Issuer())
	if oerr := s.checkOrigin(httptest.NewRecorder(), r, clientIDOf(r)); oerr != nil {
		t.Fatalf("the matching Origin was refused: %v", oerr)
	}
	if len(warn) != 0 {
		t.Errorf("a passing request logged %d warn lines", len(warn))
	}
	// An absent Origin is still accepted here: `agm auth login` prints a URL
	// an owner may open in something that is not a browser.
	absent := postWithOrigin("")
	if oerr := s.checkOrigin(httptest.NewRecorder(), absent, clientIDOf(absent)); oerr != nil {
		t.Fatalf("an absent Origin was refused on /oauth/authorize: %v", oerr)
	}
}

// TestRequireSameOriginReportsAnAbsentHeaderAsAbsent: `/complete` refuses an
// absent header, and reports it as absent rather than as an empty string,
// which is a different diagnosis for the reader.
func TestRequireSameOriginReportsAnAbsentHeaderAsAbsent(t *testing.T) {
	var warn []capturedLine
	s := newOriginServer(&warn)
	r := postWithOrigin("")
	oerr := s.requireSameOrigin(httptest.NewRecorder(), r, func() string { return "client_abc" })
	if oerr == nil || oerr.Status != http.StatusForbidden {
		t.Fatalf("an absent Origin was accepted on /complete: %v", oerr)
	}
	if !strings.Contains(oerr.Description, "carries no Origin") {
		t.Errorf("the description is %q, want it to say the header was absent",
			oerr.Description)
	}
	if len(warn) != 1 || warn[0].kv["received_origin"] != "(absent)" {
		t.Fatalf("the warn lines are %v, want one naming an absent Origin", warn)
	}
}

// TestPageHeadersDifferFromApiHeadersOnlyInReferrerPolicy: the pages get
// section 9.9's headers with one value changed, and nothing else moves.
//
// Plant: delete the Referrer-Policy override in setPageSecurityHeaders and
// this fails at "a page's Referrer-Policy".
func TestPageHeadersDifferFromApiHeadersOnlyInReferrerPolicy(t *testing.T) {
	page, api := httptest.NewRecorder(), httptest.NewRecorder()
	setPageSecurityHeaders(page)
	setSecurityHeaders(api)

	if got := page.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("a page's Referrer-Policy is %q, want same-origin: a page served "+
			"no-referrer makes its own same-origin form post arrive as Origin: null",
			got)
	}
	if got := api.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("a non-page answer's Referrer-Policy is %q, want no-referrer", got)
	}
	for header, want := range api.Header() {
		if header == "Referrer-Policy" {
			continue
		}
		if got := page.Header().Get(header); got != want[0] {
			t.Errorf("a page carries %s: %q, want %q", header, got, want[0])
		}
	}
	if len(page.Header()) != len(api.Header()) {
		t.Errorf("a page carries %d headers and an API answer %d; the split is one "+
			"value, not one header set", len(page.Header()), len(api.Header()))
	}
}
