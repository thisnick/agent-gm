// Package cli holds the pieces `agm` needs that are not the REST client: the
// Chrome cookie capture, the paste fallback, and the messages the owner
// reads at a terminal (spec section 11.4).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/gm"
)

// ErrNoChrome means no Chrome or Chromium binary was found. `agm pair` does
// not fail with a missing-binary error: it explains what it needs and offers
// the alternatives, and exits 9 (local configuration), not 2 -- nothing about
// the command was malformed.
var ErrNoChrome = errors.New("no Chrome or Chromium binary found")

// ErrOSIDNeverAppeared means the sign-in never reached messages.google.com.
// OSID is host-scoped there and is not set by the accounts.google.com
// sign-in alone; reading before it exists silently returns an unusable set.
var ErrOSIDNeverAppeared = errors.New("the OSID cookie never appeared")

// chromeCandidates are the platform's usual locations, searched after
// AGENT_GM_CHROME and before PATH.
func chromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		}
	case "windows":
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	default:
		return []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/snap/bin/chromium", "/usr/bin/brave-browser",
		}
	}
}

var chromeOnPath = []string{
	"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
}

// FindChrome resolves the browser: AGENT_GM_CHROME, then the platform's usual
// locations, then PATH (spec section 11.4).
func FindChrome(env string) (string, error) {
	if env != "" {
		if isExecutable(env) {
			return env, nil
		}
		return "", fmt.Errorf("%w: AGENT_GM_CHROME=%s is not an executable", ErrNoChrome, env)
	}
	for _, c := range chromeCandidates() {
		if isExecutable(c) {
			return c, nil
		}
	}
	for _, name := range chromeOnPath {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", ErrNoChrome
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0 || runtime.GOOS == "windows"
}

// NoChromeMessage is what `agm pair` prints when it cannot find Chrome. The
// two ways forward are in the order spec section 11.4 states, best first.
func NoChromeMessage(publicURL string) string {
	if publicURL == "" {
		publicURL = "https://gm.agent-wx.app"
	}
	return `Agent GM could not find Chrome or Chromium on this machine.

The default pairing flow signs in to your Google account in a dedicated
Chrome profile and reads the seven session cookies Google Messages for web
uses. Five of them are httpOnly, so only a real browser can produce them.

Two ways forward, best first:

  1. Run this same command from a machine that has Chrome, pointing at this
     server -- only the cookies travel, over TLS:

         agm pair --server ` + publicURL + `

  2. Paste them yourself. In a browser already signed in to
     messages.google.com, open devtools -> Network, right-click any request
     -> Copy as cURL, then:

         agm pair --paste                 # reads the paste from stdin
         agm pair --paste-file ./curl.txt

Set AGENT_GM_CHROME to a binary path if Chrome is somewhere unusual.
`
}

// ProfileDir is the kept Chrome profile for one account. It is keyed by
// account: a single shared profile would hold whichever account signed in
// last, so a refresh for account A run after adding account B would capture
// B's cookies and only then fail on pairing_wrong_account -- a full sign-in
// wasted on a knowable mistake (spec section 11.4).
//
// accountID may be empty when adding a NEW account, which gets a fresh
// directory so it always starts signed out and the owner is never offered the
// wrong account by accident.
func ProfileDir(stateDir, accountID string) string {
	base := filepath.Join(stateDir, "chrome-profile")
	if accountID == "" {
		return filepath.Join(base, "new-"+uuid.NewString())
	}
	return filepath.Join(base, accountID)
}

// ForgetBrowser deletes the saved Chrome sign-in. With an account ID it
// removes one; without, it removes them all.
func ForgetBrowser(stateDir, accountID string) error {
	base := filepath.Join(stateDir, "chrome-profile")
	if accountID == "" {
		return os.RemoveAll(base)
	}
	return os.RemoveAll(filepath.Join(base, accountID))
}

// ForgetBrowserEffect is the exact sentence the prompt, the route's `effect`
// field and the tool description all use -- the same words everywhere.
const ForgetBrowserEffect = "deletes the saved Chrome sign-in for this account; " +
	"the next pair or cookie refresh will ask you to sign in to Google again"

// Capture drives the dedicated Chrome profile over CDP.
type Capture struct {
	// Chrome is the browser binary.
	Chrome string
	// ProfileDir is the dedicated --user-data-dir. It is mandatory, not
	// stylistic: current Chrome refuses --remote-debugging-port against the
	// default profile directory.
	ProfileDir string
	// Timeout bounds how long the owner has to finish signing in.
	Timeout time.Duration
	// Logf receives progress lines for the owner. Cookie values never reach
	// it.
	Logf func(format string, args ...any)
}

func (c *Capture) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run launches Chrome, waits for the sign-in, reads exactly the seven cookies
// across both hosts, terminates Chrome and returns them.
//
// The cookies are never written to a file of Agent GM's own: they go from CDP
// into the caller and nowhere else.
func (c *Capture) Run(ctx context.Context) (map[string]string, error) {
	if err := os.MkdirAll(c.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the Chrome profile directory: %w", err)
	}
	if err := os.Chmod(c.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("securing the Chrome profile directory: %w", err)
	}

	port, err := freeLoopbackPort()
	if err != nil {
		return nil, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No other flag is passed -- no --enable-automation, no --headless -- so
	// navigator.webdriver is unset, there is no automation infobar, and
	// Google's sign-in sees an ordinary Chrome.
	cmd := exec.CommandContext(runCtx, c.Chrome,
		"--user-data-dir="+c.ProfileDir,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		gm.GaiaCaptureURL,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching Chrome: %w", err)
	}
	// Chrome is killed the moment the capture completes. The process is
	// always the one we started -- never a name pattern.
	var once sync.Once
	kill := func() {
		once.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}
	defer kill()

	wsURL, err := waitForDevTools(runCtx, port)
	if err != nil {
		return nil, err
	}

	cdp, err := dialCDP(runCtx, wsURL)
	if err != nil {
		return nil, err
	}
	defer cdp.close()

	cookies, err := cdp.waitForCookies(runCtx, c.logf)
	if err != nil {
		return nil, err
	}

	c.logf("Captured %d cookies.  Closing Chrome.", len(cookies))
	kill()
	return cookies, nil
}

func freeLoopbackPort() (int, error) {
	// Bound to loopback on a random port. The debugging port is a live
	// credential channel: anything that can connect to it reads every cookie
	// in that profile.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserving a loopback port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func waitForDevTools(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err == nil {
			var body struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			dec := json.NewDecoder(resp.Body)
			decErr := dec.Decode(&body)
			_ = resp.Body.Close()
			if decErr == nil && body.WebSocketDebuggerURL != "" {
				return body.WebSocketDebuggerURL, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("chrome's debugging port never answered: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// --- a very small CDP client -------------------------------------------------

type cdpConn struct {
	ws *websocket.Conn
	mu sync.Mutex
	id int64
}

func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to Chrome's debugging protocol: %w", err)
	}
	// Cookie payloads are large; the default read limit is too small.
	ws.SetReadLimit(32 << 20)
	return &cdpConn{ws: ws}, nil
}

func (c *cdpConn) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

type cdpRequest struct {
	ID        int64          `json:"id"`
	Method    string         `json:"method"`
	Params    map[string]any `json:"params,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one command and waits for its reply, discarding the events that
// arrive in between.
func (c *cdpConn) call(ctx context.Context, sessionID, method string, params map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	id := c.id
	req := cdpRequest{ID: id, Method: method, Params: params, SessionID: sessionID}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, body); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		var resp cdpResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue // an event, or another command's reply
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: chrome said %d %s", method, resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// attachToPage finds a page target and attaches to it, because
// Network.getCookies is a page-level command.
func (c *cdpConn) attachToPage(ctx context.Context) (string, error) {
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := c.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return "", err
	}
	for _, t := range targets.TargetInfos {
		if t.Type != "page" {
			continue
		}
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		err := c.call(ctx, "", "Target.attachToTarget",
			map[string]any{"targetId": t.TargetID, "flatten": true}, &attached)
		if err != nil {
			continue
		}
		if attached.SessionID != "" {
			return attached.SessionID, nil
		}
	}
	return "", errors.New("chrome has no page to read cookies from yet")
}

type cdpCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	HTTPOnly bool   `json:"httpOnly"`
}

// cookieURLs are both hosts the read must cover. OSID is host-scoped to
// messages.google.com, so a read of .google.com alone is the most common way
// this fails.
var cookieURLs = []any{"https://messages.google.com", "https://www.google.com"}

func (c *cdpConn) readCookies(ctx context.Context, sessionID string) (map[string]string, error) {
	var out struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	err := c.call(ctx, sessionID, "Network.getCookies",
		map[string]any{"urls": cookieURLs}, &out)
	if err != nil {
		return nil, err
	}
	// Exactly the seven, and nothing else.
	wanted := map[string]bool{}
	for _, n := range gm.GaiaRequiredCookies {
		wanted[n] = true
	}
	for _, n := range gm.GaiaOptionalCookies {
		wanted[n] = true
	}
	got := map[string]string{}
	for _, ck := range out.Cookies {
		if !wanted[ck.Name] || ck.Value == "" {
			continue
		}
		// OSID must come from messages.google.com; everything else from
		// .google.com. A value on the wrong host is not the cookie we mean.
		want := gm.GaiaCookieDomains[ck.Name]
		if !domainMatches(ck.Domain, want) {
			continue
		}
		got[ck.Name] = ck.Value
	}
	return got, nil
}

func domainMatches(got, want string) bool {
	got = strings.TrimPrefix(got, ".")
	want = strings.TrimPrefix(want, ".")
	return strings.EqualFold(got, want) || strings.HasSuffix(strings.ToLower(got), "."+strings.ToLower(want))
}

// waitForCookies waits for the owner to finish signing in, and then waits for
// OSID to exist before reading. __Secure-1PSIDTS is read last, because it
// rotates.
func (c *cdpConn) waitForCookies(ctx context.Context, logf func(string, ...any)) (map[string]string, error) {
	announced := false
	for {
		sessionID, err := c.attachToPage(ctx)
		if err == nil {
			cookies, err := c.readCookies(ctx, sessionID)
			if err == nil {
				if missing := gm.MissingRequiredCookies(cookies); len(missing) == 0 {
					// One more read, last, for the rotating cookie.
					if fresh, err := c.readCookies(ctx, sessionID); err == nil {
						if v := fresh["__Secure-1PSIDTS"]; v != "" {
							cookies["__Secure-1PSIDTS"] = v
						}
					}
					return cookies, nil
				} else if !announced && len(cookies) > 0 {
					announced = true
					if logf != nil {
						logf("Signed in. Waiting for Google Messages for web to load, "+
							"because %s is set only by messages.google.com.", strings.Join(missing, ", "))
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w within the pairing timeout: sign in and let "+
				"messages.google.com finish loading", ErrOSIDNeverAppeared)
		case <-time.After(time.Second):
		}
	}
}
