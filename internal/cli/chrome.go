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
	return FindChromeIn(env, chromeCandidates(), exec.LookPath)
}

// FindChromeIn is FindChrome with the filesystem injected, so the
// no-Chrome case is deterministic on a machine that has Chrome. The search
// order is the spec's: the environment, then the platform's usual locations,
// then PATH.
func FindChromeIn(env string, candidates []string, lookPath func(string) (string, error)) (string, error) {
	if env != "" {
		if isExecutable(env) {
			return env, nil
		}
		return "", fmt.Errorf("%w: AGENT_GM_CHROME=%s is not an executable", ErrNoChrome, env)
	}
	for _, c := range candidates {
		if isExecutable(c) {
			return c, nil
		}
	}
	for _, name := range chromeOnPath {
		if p, err := lookPath(name); err == nil && p != "" {
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

// NewProfileDir creates the short-lived Chrome profile for one capture.
//
// The profile is deliberately temporary (D33): it holds a logged-in Google
// session, so it is created fresh under root for the capture and deleted the
// moment Chrome is closed -- on success, on failure, on timeout and on
// Ctrl-C -- leaving nothing under the state directory. root may be empty for
// the platform's temporary directory.
func NewProfileDir(root string) (string, error) {
	if root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", fmt.Errorf("creating %s: %w", root, err)
		}
	}
	dir, err := os.MkdirTemp(root, "agm-chrome-profile-")
	if err != nil {
		return "", fmt.Errorf("creating the Chrome profile directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("securing the Chrome profile directory: %w", err)
	}
	return dir, nil
}

// Capture drives the short-lived Chrome profile over CDP.
type Capture struct {
	// Chrome is the browser binary.
	Chrome string
	// ProfileRoot is the directory the short-lived --user-data-dir is made
	// under. Empty means the platform's temporary directory. The profile
	// itself is created by Run and removed by Run, always.
	ProfileRoot string
	// Timeout bounds how long the owner has to finish signing in.
	Timeout time.Duration
	// Logf receives progress lines for the owner. Cookie values never reach
	// it.
	Logf func(format string, args ...any)
	// Environ is the environment Chrome's display hint is read from. Empty
	// means the process environment. It exists so the hint is testable.
	Environ func(string) string

	// profileDir is the directory this capture used, kept so a test can
	// assert it is gone afterwards.
	mu         sync.Mutex
	profileDir string
}

// ProfileDir reports the short-lived profile directory the last Run used. It
// must not exist after Run returns.
func (c *Capture) ProfileDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profileDir
}

// args builds Chrome's argv.
//
// The dedicated --user-data-dir is mandatory, not stylistic: current Chrome
// refuses --remote-debugging-port against the default profile directory, and a
// build that did attach would open a live CDP credential channel onto the
// owner's own profile rather than onto the dedicated one.
//
// No other flag is passed -- no --enable-automation, no --headless -- so
// navigator.webdriver is unset, there is no automation infobar, and Google's
// sign-in sees an ordinary Chrome (spec section 11.4 step 1).
func (c *Capture) args(profileDir string, port int) []string {
	return []string{
		"--user-data-dir=" + profileDir,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		gm.GaiaCaptureURL,
	}
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
	return c.runWith(ctx, c.capture)
}

// runWith owns the short-lived profile's whole life (D33): it is created
// here, handed to the capture, and removed on every path out -- success, a
// Chrome that died on launch, a timeout, and a Ctrl-C, which cancels ctx and
// unwinds through here. Nothing signed in to Google is left on the machine
// and nothing is written under the state directory.
func (c *Capture) runWith(ctx context.Context, capture func(context.Context, string) (map[string]string, error)) (map[string]string, error) {
	profileDir, err := NewProfileDir(c.ProfileRoot)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.profileDir = profileDir
	c.mu.Unlock()
	defer func() { _ = os.RemoveAll(profileDir) }()

	return capture(ctx, profileDir)
}

func (c *Capture) capture(ctx context.Context, profileDir string) (map[string]string, error) {
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

	cmd := exec.CommandContext(runCtx, c.Chrome, c.args(profileDir, port)...)
	// Chrome's own stderr is kept, bounded, rather than discarded: when
	// Chrome exits immediately -- no display, no X authority, a broken
	// profile -- its stderr is the only thing that says why, and reporting
	// "the debugging port never answered" instead is what cost the Slice 1
	// live gate an hour.
	stderr := &tailBuffer{limit: 8 << 10}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching Chrome (%s): %w%s", c.Chrome, err, c.displayHint())
	}

	// Wait for the process in the background so an early exit is a fact this
	// function can select on rather than a timeout it has to infer.
	// Closed rather than written, so both the poll below and kill() can
	// observe the exit without racing each other for one value.
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()

	// Chrome is killed the moment the capture completes. The process is
	// always the one we started -- never a name pattern.
	var once sync.Once
	kill := func() {
		once.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-waited
		})
	}
	defer kill()

	wsURL, err := waitForDevTools(runCtx, port, waited, func() error {
		return c.exitError(stderr)
	})
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

// ErrChromeExited means Chrome stopped before the capture could start. Its
// own stderr is carried in the message, because that is where the reason is.
var ErrChromeExited = errors.New("chrome exited before the capture could start")

// exitError builds the message for a Chrome that died on launch.
func (c *Capture) exitError(stderr *tailBuffer) error {
	said := strings.TrimSpace(stderr.String())
	if said == "" {
		said = "(chrome printed nothing to stderr)"
	}
	return fmt.Errorf("%w; chrome said:\n%s%s", ErrChromeExited, indent(said, "    "), c.displayHint())
}

func (c *Capture) getenv(key string) string {
	if c.Environ != nil {
		return c.Environ(key)
	}
	return os.Getenv(key)
}

// displayHint returns the desktop-session hint, or "" when the environment
// looks fine. It is appended to a launch failure rather than printed always,
// because it is only ever an explanation of one.
func (c *Capture) displayHint() string {
	h := DisplayHint(c.getenv)
	if h == "" {
		return ""
	}
	return "\n\n" + h
}

// DisplayHint diagnoses the common Linux-desktop case: a CLI run from a
// terminal that has no access to the owner's graphical session. Chrome cannot
// open a window there and exits at once, and the message it prints
// ("cannot open display") is not one an owner reads as "set XAUTHORITY".
//
// It returns "" on macOS and Windows, which have no such variables, and ""
// when the environment already names a display and an authority.
func DisplayHint(getenv func(string) string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return ""
	}
	display := getenv("DISPLAY")
	wayland := getenv("WAYLAND_DISPLAY")
	if display == "" && wayland == "" {
		return `Agent GM found no graphical session: neither DISPLAY nor WAYLAND_DISPLAY
is set, so Chrome has no window to open. Run this command from a terminal
inside the desktop session, or pair from a machine that has one:

    agm pair --server <this server's URL>

On a headless host use the paste fallback instead: agm pair --paste`
	}
	if wayland != "" {
		return ""
	}
	if getenv("XAUTHORITY") != "" {
		return ""
	}
	if home := getenv("HOME"); home != "" {
		if _, err := os.Stat(filepath.Join(home, ".Xauthority")); err == nil {
			return ""
		}
	}
	return `DISPLAY is set to ` + display + ` but XAUTHORITY is not, and there is no
~/.Xauthority, so Chrome may be refused by the X server it is pointed at.
If this shell is not the one that started the desktop session -- an ssh or a
remote-desktop shell often is not -- take the authority file from the running
session, for example:

    export XAUTHORITY=$(tr '\0' '\n' < /proc/$(pgrep -x gnome-shell | head -1)/environ \
        | sed -n 's/^XAUTHORITY=//p')

then run agm pair again.`
}

func indent(s, with string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = with + l
	}
	return strings.Join(lines, "\n")
}

// tailBuffer keeps the last limit bytes written to it. Chrome can be chatty,
// and only the end of what it said is useful.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
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

// waitForDevTools polls Chrome's debugging port until it answers. exited is
// closed-or-written when the browser process ends; onExit builds the error
// for that case, so a Chrome that died on launch is reported as what it was
// rather than as a port that never answered.
func waitForDevTools(ctx context.Context, port int, exited <-chan struct{}, onExit func() error) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return "", onExit()
		default:
		}
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
		case <-exited:
			return "", onExit()
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

// getCookiesParams is the exact Network.getCookies request. It is a function
// so a test can assert the urls array without a browser.
func getCookiesParams() map[string]any {
	return map[string]any{"urls": cookieURLs}
}

// selectGaiaCookies keeps exactly the seven, each only from the host it is
// scoped to. A value on the wrong host is not the cookie we mean.
func selectGaiaCookies(cookies []cdpCookie) map[string]string {
	wanted := map[string]bool{}
	for _, n := range gm.GaiaRequiredCookies {
		wanted[n] = true
	}
	for _, n := range gm.GaiaOptionalCookies {
		wanted[n] = true
	}
	got := map[string]string{}
	for _, ck := range cookies {
		if !wanted[ck.Name] || ck.Value == "" {
			continue
		}
		if !domainMatches(ck.Domain, gm.GaiaCookieDomains[ck.Name]) {
			continue
		}
		got[ck.Name] = ck.Value
	}
	return got
}

func (c *cdpConn) readCookies(ctx context.Context, sessionID string) (map[string]string, error) {
	var out struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	err := c.call(ctx, sessionID, "Network.getCookies", getCookiesParams(), &out)
	if err != nil {
		return nil, err
	}
	return selectGaiaCookies(out.Cookies), nil
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
