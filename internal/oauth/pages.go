package oauth

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
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

func (d approvalPageData) EnrollmentCommand() string {
	return "agm admin enrollment-codes create oauth-client --scopes " + strings.Join(d.Scopes, ",")
}

var approvalTemplate = template.Must(template.New("approve").Parse(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize a client &mdash; Agent GM</title>
<link rel="stylesheet" href="/oauth/style.css">
</head><body>
<main>
<header><span class="brand">AGENT GM</span></header>
<p class="eyebrow">STEP 1 OF 2 · ENROLL</p>
<h1>Connect your client</h1>
<p>{{if .ClientName}}<strong>{{.ClientName}}</strong>{{else}}A client{{end}}
 is asking to use this Agent GM server.</p>
{{if .Message}}<p role="alert">{{.Message}}</p>{{end}}
{{if .Allowed}}<p>That code allows: {{range .Allowed}}<code>{{.}}</code> {{end}}</p>{{end}}

<section class="access-summary"><h2>Review the access</h2><p>{{.Disclosure}}</p>
{{if .Accounts}}
<ul>{{range .Accounts}}<li>{{if .Label}}{{.Label}} &mdash; {{end}}{{.Address}}</li>{{end}}</ul>
{{else}}
<p>This server holds no accounts yet.</p>
{{end}}

</section>
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
    <legend>Permissions to grant</legend>
    {{range .Scopes}}
    <label><input type="checkbox" name="scope_selected" value="{{.}}"{{if $.IsSelected .}} checked{{end}}> {{.}}</label>
    {{end}}
  </fieldset>

  <section class="instructions">
    <h2>Get an enrollment code</h2>
    <p>In a terminal with an authorized <code>agm</code> admin session for this server, run:</p>
    <div class="command">
<pre><code id="agm-enrollment-command">{{.EnrollmentCommand}}</code></pre>
<button type="button" class="copy-command" data-copy="agm-enrollment-command" aria-label="Copy enrollment command" hidden>Copy</button>
<span class="copy-feedback" role="status" aria-live="polite"></span>
</div>
    <p>Copy the returned <code>code</code> below. It is shown only once. If you are not the server owner, ask them to create and share a code with you.</p>
  </section>
  <label for="enrollment_code">Enrollment code</label>
  <input id="enrollment_code" name="enrollment_code" type="text" autocomplete="off"
         spellcheck="false" autocapitalize="characters" placeholder="XXXX-XXXX-XXXX-XXXX" aria-describedby="enrollment-help" required>
  <p id="enrollment-help" class="muted">Submitting the code creates a request. The owner will approve it in the CLI next.</p>

  <button type="submit">Request approval <span aria-hidden="true">→</span></button>
</form>
<script src="/oauth/poll.js" defer></script>
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
	setPageSecurityHeaders(w)
	allowCallbackFormAction(w, data.RedirectURI)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type waitingPageData struct {
	RequestID   string
	RedirectURI string
	Status      string
	FormToken   string
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
<link rel="stylesheet" href="/oauth/style.css">
</head><body>
<main>
<header><span class="brand">AGENT GM</span></header>
<p class="eyebrow">STEP 2 OF 2 · APPROVE</p>
<h1 id="agm-heading">{{if eq .Status "approved"}}You're approved{{else if eq .Status "pending"}}Approve your connection{{else}}Request {{.Status}}{{end}}</h1>
<p role="status" aria-live="polite" id="agm-status" data-request-id="{{.RequestID}}" data-status="{{.Status}}">{{.Message}}</p>
<section id="agm-instructions" class="instructions"{{if ne .Status "pending"}} hidden{{end}}>
<h2>Finish in your terminal</h2>
<p>Using an authorized <code>agm</code> admin session for this server, review this request:</p>
<div class="command">
<pre><code id="agm-review-command">agm admin authorization-requests show {{.RequestID}}</code></pre>
<button type="button" class="copy-command" data-copy="agm-review-command" aria-label="Copy review command" hidden>Copy</button>
<span class="copy-feedback" role="status" aria-live="polite"></span>
</div>
<p>Check the client and permissions, then approve it:</p>
<div class="command">
<pre><code id="agm-approve-command">agm admin authorization-requests approve {{.RequestID}}</code></pre>
<button type="button" class="copy-command" data-copy="agm-approve-command" aria-label="Copy approval command" hidden>Copy</button>
<span class="copy-feedback" role="status" aria-live="polite"></span>
</div>
<p class="muted">Not the server owner? Share these commands with them. Keep this page open; it checks for approval automatically.</p>
</section>
<p id="agm-connection" class="muted" role="status"></p>
<noscript><p>JavaScript is disabled. After CLI approval, <a href="/oauth/requests/{{.RequestID}}">refresh the request status</a>.</p></noscript>
<form id="agm-complete" method="post" action="/oauth/requests/{{.RequestID}}/complete"{{if not .Done}} hidden{{end}}>
  <input type="hidden" name="form_token" value="{{.FormToken}}">
  <button type="submit">Continue to client <span aria-hidden="true">→</span></button>
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
	setPageSecurityHeaders(w)
	allowCallbackFormAction(w, data.RedirectURI)
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
  document.querySelectorAll("[data-copy]").forEach(function (button) {
    var command = document.getElementById(button.getAttribute("data-copy"));
    var feedback = button.parentElement.querySelector(".copy-feedback");
    var reset;
    button.hidden = false;
    button.addEventListener("click", async function () {
      window.clearTimeout(reset);
      button.textContent = "Copy";
      button.disabled = true;
      feedback.textContent = "";
      try {
        await navigator.clipboard.writeText(command.textContent);
        button.textContent = "Copied";
        feedback.textContent = "Command copied.";
      } catch (_) {
        var range = document.createRange();
        range.selectNodeContents(command);
        var selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        feedback.textContent = "Copy unavailable. Command selected; press Ctrl+C or ⌘C to copy.";
      } finally {
        button.disabled = false;
        reset = window.setTimeout(function () {
          button.textContent = "Copy";
          feedback.textContent = "";
        }, 5000);
      }
    });
  });
  var el = document.getElementById("agm-status");
  if (!el) { return; }
  var id = el.getAttribute("data-request-id");
  var form = document.getElementById("agm-complete");
  var heading = document.getElementById("agm-heading");
  var instructions = document.getElementById("agm-instructions");
  var connection = document.getElementById("agm-connection");
  if (el.getAttribute("data-status") !== "pending") { return; }
  function finish(status, title, message, canContinue) {
    el.setAttribute("data-status", status);
    el.textContent = message;
    heading.textContent = title;
    document.title = title + " — Agent GM";
    instructions.hidden = true;
    connection.textContent = "";
    form.hidden = !canContinue;
  }
  function clamp(n) {
    if (typeof n !== "number" || !isFinite(n)) { return 2; }
    return Math.min(60, Math.max(1, Math.round(n)));
  }
  function poll() {
    fetch("/oauth/requests/" + encodeURIComponent(id) + "/status", {
      credentials: "same-origin",
      cache: "no-store",
      headers: { "Accept": "application/json" }
    }).then(function (r) {
      if (r.status === 404) {
        finish("closed", "Session unavailable", "This browser session is no longer available. Restart the connection from your client.", false);
        return null;
      }
      if (!r.ok) { throw new Error("status " + r.status); }
      return r.json();
    }).then(function (body) {
      if (!body) { return; }
      connection.textContent = "Checking automatically. You can leave this page open.";
      el.setAttribute("data-status", body.status);
      if (body.status === "approved") {
        finish("approved", "You're approved", "The owner approved your request. Click Continue to client to finish connecting.", true);
        return;
      }
      if (body.status === "denied") {
        finish("denied", "Request denied", "The owner denied this request. Continue to return to your client.", true);
        return;
      }
      if (body.status === "completed" || body.status === "expired") {
        finish(body.status, "Request " + body.status, "This request is closed. Restart the connection from your client if needed.", false);
        return;
      }
      window.setTimeout(poll, clamp(body.poll_interval_seconds) * 1000);
    }).catch(function () {
      connection.textContent = "Unable to check approval. Retrying automatically…";
      window.setTimeout(poll, 5000);
    });
  }
  window.setTimeout(poll, 1000);
})();
`

func (s *Server) pollScript(w http.ResponseWriter, _ *http.Request) {
	setPageSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(pollScript))
}
