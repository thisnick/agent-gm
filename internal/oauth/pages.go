package oauth

import (
	"bytes"
	"html/template"
	"net/http"
)

// The two browser pages, spec sections 9.4, 9.5 and 9.9.
//
// They are plain HTML with no framework, no external asset and one script,
// served from `/oauth/poll.js` so that `script-src` stays `'self'` and no
// page needs a nonce or `unsafe-inline`. Everything a page interpolates goes
// through html/template's contextual escaping; nothing is assembled with
// string concatenation.

type approvalPageData struct {
	Context     string
	FormToken   string
	ClientID    string
	ClientName  string
	RedirectURI string
	State       string
	Challenge   string
	Method      string
	Resource    string
	// ScopeParam is the requested set as the query parameter spelled it,
	// echoed back so the POST can be checked against the signed context.
	ScopeParam string
	Scopes     []string
	Selected   []string
	// Allowed is the enrollment code's ceiling, shown when a selection went
	// above it.
	Allowed []string
	// Message is the one line a refused submission renders. It is the ONLY
	// place an enrollment failure is reported, and it is the same string for
	// unknown, expired, revoked and consumed.
	Message  string
	Accounts []Account
}

// Disclosure is a method rather than a field so that no code path can render
// this page with a different sentence, or with none.
func (approvalPageData) Disclosure() string { return GlobalScopeDisclosure }

// isSelected drives the checkbox state on a re-render, so a refused
// submission comes back with the owner's choices still made.
func (d approvalPageData) IsSelected(scope string) bool {
	if len(d.Selected) == 0 {
		return true
	}
	for _, s := range d.Selected {
		if s == scope {
			return true
		}
	}
	return false
}

var approvalTemplate = template.Must(template.New("approve").Parse(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize a client &mdash; Agent GM</title>
</head><body>
<main>
<h1>Authorize a client</h1>
<p>{{if .ClientName}}<strong>{{.ClientName}}</strong>{{else}}A client{{end}}
 is asking to use this Agent GM server.</p>
{{if .Message}}<p role="alert">{{.Message}}</p>{{end}}
{{if .Allowed}}<p>That code allows: {{range .Allowed}}<code>{{.}}</code> {{end}}</p>{{end}}

<p>{{.Disclosure}}</p>
{{if .Accounts}}
<ul>{{range .Accounts}}<li>{{if .Label}}{{.Label}} &mdash; {{end}}{{.Address}}</li>{{end}}</ul>
{{else}}
<p>This server holds no accounts yet.</p>
{{end}}

<form method="post" action="/oauth/authorize">
  <input type="hidden" name="context" value="{{.Context}}">
  <input type="hidden" name="form_token" value="{{.FormToken}}">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="code_challenge" value="{{.Challenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.Method}}">
  <input type="hidden" name="resource" value="{{.Resource}}">
  <input type="hidden" name="scope" value="{{.ScopeParam}}">

  <fieldset>
    <legend>Access</legend>
    {{range .Scopes}}
    <label><input type="checkbox" name="scope_selected" value="{{.}}"{{if $.IsSelected .}} checked{{end}}> {{.}}</label>
    {{end}}
  </fieldset>

  <label for="enrollment_code">Enrollment code</label>
  <input id="enrollment_code" name="enrollment_code" type="text" autocomplete="off"
         spellcheck="false" required>

  <button type="submit">Continue</button>
</form>
</main>
</body></html>
`))

func (s *Server) renderApprovalPage(w http.ResponseWriter, status int, data approvalPageData) {
	var buf bytes.Buffer
	if err := approvalTemplate.Execute(&buf, data); err != nil {
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the screen could not be rendered"))
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type waitingPageData struct {
	RequestID string
	Status    string
	FormToken string
	// Done reports whether the page should offer the completion form rather
	// than go on polling.
	Done bool
	// Terminal reports a state no amount of waiting will change: expired,
	// completed, or denied.
	Terminal bool
	Message  string
}

var waitingTemplate = template.Must(template.New("waiting").Parse(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Waiting for approval &mdash; Agent GM</title>
</head><body>
<main>
<h1>Waiting for the owner</h1>
<p id="agm-status" data-request-id="{{.RequestID}}" data-status="{{.Status}}">{{.Message}}</p>
<form id="agm-complete" method="post" action="/oauth/requests/{{.RequestID}}/complete"{{if not .Done}} hidden{{end}}>
  <input type="hidden" name="form_token" value="{{.FormToken}}">
  <button type="submit">Continue</button>
</form>
{{if not .Terminal}}<script src="/oauth/poll.js" defer></script>{{end}}
</main>
</body></html>
`))

func (s *Server) renderWaitingPage(w http.ResponseWriter, status int, data waitingPageData) {
	var buf bytes.Buffer
	if err := waitingTemplate.Execute(&buf, data); err != nil {
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the screen could not be rendered"))
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// pollScript is served from its own path rather than inlined, which is the
// whole reason `script-src` can stay `'self'` (spec section 9.5).
//
// It clamps the server's `poll_interval_seconds` to 1-60 seconds. The server
// decides the interval; the page refuses to believe a value that would either
// hammer it or appear to hang.
const pollScript = `(function () {
  "use strict";
  var el = document.getElementById("agm-status");
  if (!el) { return; }
  var id = el.getAttribute("data-request-id");
  var form = document.getElementById("agm-complete");
  function clamp(n) {
    if (typeof n !== "number" || !isFinite(n)) { return 2; }
    return Math.min(60, Math.max(1, Math.round(n)));
  }
  function poll() {
    fetch("/oauth/requests/" + encodeURIComponent(id) + "/status", {
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    }).then(function (r) {
      if (!r.ok) { throw new Error("status " + r.status); }
      return r.json();
    }).then(function (body) {
      el.setAttribute("data-status", body.status);
      if (body.status === "approved") {
        el.textContent = "Approved. Continue to finish.";
        if (form) { form.hidden = false; }
        return;
      }
      if (body.status === "denied") {
        el.textContent = "The owner denied this request.";
        if (form) { form.hidden = false; }
        return;
      }
      if (body.status === "completed" || body.status === "expired") {
        el.textContent = "This request is closed.";
        return;
      }
      window.setTimeout(poll, clamp(body.poll_interval_seconds) * 1000);
    }).catch(function () {
      window.setTimeout(poll, 5000);
    });
  }
  window.setTimeout(poll, 1000);
})();
`

func (s *Server) pollScript(w http.ResponseWriter, _ *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(pollScript))
}
