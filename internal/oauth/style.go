package oauth

import "net/http"

func (s *Server) stylesheet(w http.ResponseWriter, _ *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(pageStyles))
}

const pageStyles = `
:root { color-scheme: light; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: #172b28; background: #eef3f0; }
* { box-sizing: border-box; }
body { margin: 0; padding: 28px 20px; min-height: 100vh; }
main { max-width: 720px; margin: auto; background: #fff; border: 1px solid #d5e1da; border-radius: 10px; padding: 28px; }
header { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 24px; }
.brand { font-size: 14px; font-weight: 800; letter-spacing: .14em; }
.badge { background: #edf5ef; color: #35634b; font-size: 12px; border-radius: 20px; padding: 7px 10px; }
h1 { font-size: clamp(26px, 5vw, 36px); line-height: 1.15; letter-spacing: -.035em; margin: 10px 0 18px; }
h2, legend { font-size: 16px; font-weight: 700; margin: 0 0 10px; }
p, li { font-size: 15px; line-height: 1.65; overflow-wrap: anywhere; }
.eyebrow { color: #44785b; font-size: 11px; font-weight: 800; letter-spacing: .12em; }
.muted { color: #53655c; font-size: 13px; }
.access-summary, .instructions { background: #f5f8f6; border: 1px solid #e0e8e2; border-radius: 8px; padding: 16px; margin: 20px 0; }
.access-summary p:last-child, .instructions p:last-child { margin-bottom: 0; }
fieldset { border: 0; padding: 0; margin: 24px 0; }
fieldset label { display: flex; align-items: center; gap: 10px; padding: 12px; border: 1px solid #dce5de; border-radius: 8px; margin-top: 8px; font: 14px ui-monospace, monospace; }
input[type=checkbox] { width: 18px; height: 18px; accent-color: #276448; }
label[for] { display: block; font-size: 14px; font-weight: 650; margin-bottom: 8px; }
input[type=text] { width: 100%; border: 1px solid #9cad9f; border-radius: 8px; padding: 14px; font: 18px ui-monospace, monospace; letter-spacing: .06em; }
button { width: 100%; padding: 15px 20px; margin-top: 10px; border: 0; border-radius: 6px; background: #245d43; color: white; font: 650 15px system-ui, sans-serif; cursor: pointer; }
button:hover { background: #194a33; }
button span { margin-left: 8px; }
:focus-visible { outline: 3px solid #6aa98a; outline-offset: 3px; }
code { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: .9em; }
pre { background: #18352a; color: #e1f5e7; border-radius: 8px; padding: 15px; white-space: pre-wrap; overflow-wrap: anywhere; line-height: 1.7; font-size: 13px; }
[role=alert] { background: #fff0ed; color: #973c2a; padding: 14px; border-radius: 8px; }
#agm-status { padding: 16px; border-radius: 10px; background: #f2f5ee; border: 1px solid #dce5d6; }
#agm-status[data-status=approved] { background: #e8f5ec; border-color: #9ac8aa; }
a { color: #245d43; }
[hidden] { display: none !important; }
@media (max-width: 520px) { body { padding: 16px 10px; } main { padding: 24px 18px; border-radius: 14px; } header { margin-bottom: 28px; } .instructions, .access-summary { padding: 16px; } }
`
