package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisnick/agent-gm/internal/gm"
)

// F-1, plant R-M8: the dedicated --user-data-dir is mandatory, not stylistic.
// Dropping it makes Chrome refuse the debugging port, and on a build where it
// did attach it would open a live CDP credential channel onto the owner's own
// Chrome profile -- every cookie for every site -- rather than onto the
// dedicated one (spec section 11.4 step 1).
//
// This test also pins "no other flag is passed": exactly three arguments, in
// this order, and nothing resembling --headless or --enable-automation.
func TestChromeArgvIsExactlyTheThreeArguments(t *testing.T) {
	c := &Capture{Chrome: "/usr/bin/google-chrome"}
	got := c.args("/tmp/agm-chrome-profile-1", 9222)
	want := []string{
		"--user-data-dir=/tmp/agm-chrome-profile-1",
		"--remote-debugging-port=9222",
		gm.GaiaCaptureURL,
	}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, banned := range []string{
		"--headless", "--enable-automation", "--disable-blink-features",
		"--no-sandbox", "--disable-gpu", "--remote-allow-origins",
	} {
		for _, a := range got {
			if strings.HasPrefix(a, banned) {
				t.Errorf("argv carries %q; spec 11.4 says no other flag is passed", a)
			}
		}
	}
	// The debugging port is a live credential channel, so it is bound to
	// loopback on a random port. freeLoopbackPort proves the binding.
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("freeLoopbackPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("freeLoopbackPort returned %d", port)
	}
}

// The short-lived profile is created at mode 0700 and is gone the moment the
// capture ends, whatever ended it (D33). A logged-in Google session is not
// left on the machine, and nothing is written under the state directory.
//
// Plant: delete the `defer os.RemoveAll(profileDir)` in Capture.Run and this
// test fails at "the profile ... still exists". Planted 2026-09-06.
func TestShortLivedProfileIsRemovedAfterAFailedCapture(t *testing.T) {
	root := t.TempDir()
	c := &Capture{
		Chrome:      filepath.Join(t.TempDir(), "no-such-chrome"),
		ProfileRoot: root,
		Timeout:     time.Second,
	}
	if _, err := c.Run(context.Background()); err == nil {
		t.Fatal("Run with no Chrome binary should fail")
	}
	assertProfileGone(t, c, root)
}

// A capture the owner cancels -- Ctrl-C, which cancels the context -- also
// leaves nothing behind.
func TestShortLivedProfileIsRemovedAfterACancelledCapture(t *testing.T) {
	root := t.TempDir()
	chrome := stubChrome(t, "exec sleep 300")
	ctx, cancel := context.WithCancel(context.Background())
	c := &Capture{Chrome: chrome, ProfileRoot: root, Timeout: time.Minute}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Run(ctx)
	}()
	// Give Run long enough to have made the directory, then cancel -- which
	// is what Ctrl-C does.
	waitFor(t, func() bool {
		entries, err := os.ReadDir(root)
		return err == nil && len(entries) == 1
	})
	cancel()
	<-done
	assertProfileGone(t, c, root)
}

// stubChrome writes an executable shell script standing in for the browser.
// It is never a real Chrome, and it is always the process this test started.
func stubChrome(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub browser is a shell script")
	}
	path := filepath.Join(t.TempDir(), "stub-chrome")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The successful path leaves nothing behind either. The capture itself is
// stubbed -- the CDP conversation has its own tests below -- so what is
// asserted here is exactly the profile's life.
func TestShortLivedProfileIsRemovedAfterASuccessfulCapture(t *testing.T) {
	root := t.TempDir()
	c := &Capture{ProfileRoot: root}
	var sawDir string
	got, err := c.runWith(context.Background(), func(_ context.Context, dir string) (map[string]string, error) {
		sawDir = dir
		fi, statErr := os.Stat(dir)
		if statErr != nil {
			t.Errorf("the profile does not exist during the capture: %v", statErr)
		} else if fi.Mode().Perm() != 0o700 {
			t.Errorf("the Chrome profile is mode %o, want 700", fi.Mode().Perm())
		}
		return map[string]string{"SID": "FIXTURE-SID"}, nil
	})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the capture returned %d cookies", len(got))
	}
	if sawDir != c.ProfileDir() {
		t.Errorf("ProfileDir() = %q, capture saw %q", c.ProfileDir(), sawDir)
	}
	assertProfileGone(t, c, root)
}

func assertProfileGone(t *testing.T, c *Capture, root string) {
	t.Helper()
	if c.ProfileDir() == "" {
		t.Fatal("Capture.Run never recorded a profile directory")
	}
	if _, err := os.Stat(c.ProfileDir()); !os.IsNotExist(err) {
		t.Errorf("the profile %s still exists after the capture (stat err = %v)", c.ProfileDir(), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the profile root still holds %d entries; the profile is short-lived", len(entries))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the condition")
}

// A Chrome that dies at once is reported as what it was, with its own stderr,
// rather than as "the debugging port never answered" -- the Slice 1 live-gate
// finding. Planted 2026-09-06: discard cmd.Stderr and this test fails at
// "does not carry chrome's stderr".
func TestChromeEarlyExitIsReportedWithItsStderr(t *testing.T) {
	root := t.TempDir()
	chrome := stubChrome(t, "echo 'Xlib: cannot open display :0' >&2\nexit 1")
	c := &Capture{
		Chrome:      chrome,
		ProfileRoot: root,
		Timeout:     30 * time.Second,
		Environ:     func(string) string { return "" },
	}
	start := time.Now()
	_, err := c.Run(context.Background())
	if err == nil {
		t.Fatal("a Chrome that exits at once must be an error")
	}
	if !errors.Is(err, ErrChromeExited) {
		t.Fatalf("error = %v, want one wrapping ErrChromeExited", err)
	}
	if !strings.Contains(err.Error(), "cannot open display") {
		t.Errorf("the error does not carry chrome's stderr: %v", err)
	}
	if strings.Contains(err.Error(), "never answered") {
		t.Errorf("the error still blames the debugging port: %v", err)
	}
	// It must not have waited out the timeout to notice.
	if time.Since(start) > 20*time.Second {
		t.Errorf("noticing the exit took %v; it should be immediate", time.Since(start))
	}
	assertProfileGone(t, c, root)
}

// The hint for the common Linux-desktop case: a terminal with no access to
// the owner's graphical session. Chrome exits at once there and says
// something an owner does not read as "set XAUTHORITY".
func TestDisplayHint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the display hint is a Linux-desktop diagnosis")
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	t.Run("no display at all names both variables and the two ways out", func(t *testing.T) {
		h := DisplayHint(env(map[string]string{"HOME": "/nonexistent-home"}))
		for _, want := range []string{"DISPLAY", "WAYLAND_DISPLAY", "--server", "--paste"} {
			if !strings.Contains(h, want) {
				t.Errorf("the hint does not mention %q: %s", want, h)
			}
		}
	})
	t.Run("a display with no authority names XAUTHORITY", func(t *testing.T) {
		h := DisplayHint(env(map[string]string{"DISPLAY": ":0", "HOME": "/nonexistent-home"}))
		if !strings.Contains(h, "XAUTHORITY") {
			t.Errorf("the hint does not mention XAUTHORITY: %s", h)
		}
	})
	t.Run("a complete environment gets no hint", func(t *testing.T) {
		if h := DisplayHint(env(map[string]string{"DISPLAY": ":0", "XAUTHORITY": "/run/x"})); h != "" {
			t.Errorf("hint = %q, want none", h)
		}
		if h := DisplayHint(env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"})); h != "" {
			t.Errorf("hint = %q, want none for a Wayland session", h)
		}
	})
}

// F-2, plant R-M4: the read must cover https://messages.google.com as well as
// https://www.google.com. OSID is host-scoped to the former, so a read of
// .google.com alone silently returns an unusable set -- the most common way
// the gaia flow fails (spec section 11.4 step 4).
func TestCookieReadCoversBothHosts(t *testing.T) {
	params := getCookiesParams()
	urls, ok := params["urls"].([]any)
	if !ok {
		t.Fatalf("Network.getCookies params carry no urls array: %#v", params)
	}
	seen := map[string]bool{}
	for _, u := range urls {
		s, _ := u.(string)
		seen[s] = true
	}
	for _, want := range []string{"https://messages.google.com", "https://www.google.com"} {
		if !seen[want] {
			t.Errorf("the cookie read does not cover %s (urls = %v)", want, urls)
		}
	}
}

// The filter keeps exactly the seven, and only from the host each is scoped
// to: an OSID served on .google.com is not the cookie we mean.
func TestSelectGaiaCookiesEnforcesTheHostScoping(t *testing.T) {
	got := selectGaiaCookies([]cdpCookie{
		{Name: "OSID", Value: "right", Domain: "messages.google.com"},
		{Name: "SID", Value: "right", Domain: ".google.com"},
		{Name: "HSID", Value: "right", Domain: ".google.com"},
		{Name: "SSID", Value: "right", Domain: ".google.com"},
		{Name: "APISID", Value: "right", Domain: ".google.com"},
		{Name: "SAPISID", Value: "right", Domain: ".google.com"},
		{Name: "__Secure-1PSIDTS", Value: "right", Domain: ".google.com"},
		{Name: "NID", Value: "noise", Domain: ".google.com"},
		{Name: "SIDCC", Value: "noise", Domain: ".google.com"},
	})
	if len(got) != 7 {
		t.Errorf("kept %d cookies, want exactly the seven: %v", len(got), keys(got))
	}
	if missing := gm.MissingRequiredCookies(got); len(missing) != 0 {
		t.Errorf("required cookies missing: %v", missing)
	}

	// An OSID served on the wrong host is dropped, and the set is then
	// unusable -- which is what the capture must notice.
	wrongHost := selectGaiaCookies([]cdpCookie{
		{Name: "OSID", Value: "wrong-host", Domain: ".google.com"},
		{Name: "SID", Value: "right", Domain: ".google.com"},
		{Name: "HSID", Value: "right", Domain: ".google.com"},
		{Name: "SSID", Value: "right", Domain: ".google.com"},
		{Name: "APISID", Value: "right", Domain: ".google.com"},
		{Name: "SAPISID", Value: "right", Domain: ".google.com"},
	})
	if _, ok := wrongHost["OSID"]; ok {
		t.Error("an OSID on .google.com was accepted")
	}
	if missing := gm.MissingRequiredCookies(wrongHost); len(missing) != 1 || missing[0] != "OSID" {
		t.Errorf("missing = %v, want [OSID]", missing)
	}
	// An empty value is not a cookie.
	if len(selectGaiaCookies([]cdpCookie{{Name: "SID", Value: "", Domain: ".google.com"}})) != 0 {
		t.Error("an empty cookie value was kept")
	}
}

// --- a fake CDP endpoint -----------------------------------------------------

// fakeCDP is just enough of Chrome's debugging protocol to drive
// cdpConn.waitForCookies: Target.getTargets, Target.attachToTarget and
// Network.getCookies. It records the urls array every read asked for, so the
// two-host requirement is asserted against the wire and not against a table.
type fakeCDP struct {
	mu       sync.Mutex
	cookies  []cdpCookie
	askedFor [][]string
	server   *httptest.Server
}

func newFakeCDP(t *testing.T, cookies []cdpCookie) *fakeCDP {
	t.Helper()
	f := &fakeCDP{cookies: cookies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close(websocket.StatusNormalClosure, "") }()
		ws.SetReadLimit(1 << 20)
		ctx := r.Context()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var req cdpRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			var result any
			switch req.Method {
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []any{
					map[string]any{"targetId": "T1", "type": "page"},
				}}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S1"}
			case "Network.getCookies":
				var urls []string
				if raw, ok := req.Params["urls"].([]any); ok {
					for _, u := range raw {
						if s, ok := u.(string); ok {
							urls = append(urls, s)
						}
					}
				}
				f.mu.Lock()
				f.askedFor = append(f.askedFor, urls)
				cs := append([]cdpCookie(nil), f.cookies...)
				f.mu.Unlock()
				result = map[string]any{"cookies": cs}
			default:
				result = map[string]any{}
			}
			body, err := json.Marshal(map[string]any{"id": req.ID, "result": result})
			if err != nil {
				return
			}
			if err := ws.Write(ctx, websocket.MessageText, body); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeCDP) wsURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http")
}

func (f *fakeCDP) reads() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.askedFor...)
}

func goodCookies() []cdpCookie {
	out := []cdpCookie{{Name: "OSID", Value: "FIXTURE-OSID", Domain: "messages.google.com"}}
	for _, n := range []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSIDTS"} {
		out = append(out, cdpCookie{Name: n, Value: "FIXTURE-" + n, Domain: ".google.com"})
	}
	return out
}

// The whole CDP read, end to end against a fake endpoint: it attaches to a
// page, asks for BOTH hosts, and returns the seven.
func TestWaitForCookiesReadsBothHostsAndReturnsTheSeven(t *testing.T) {
	f := newFakeCDP(t, goodCookies())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialCDP(ctx, f.wsURL())
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer conn.close()

	got, err := conn.waitForCookies(ctx, nil)
	if err != nil {
		t.Fatalf("waitForCookies: %v", err)
	}
	if len(got) != 7 {
		t.Errorf("captured %d cookies, want 7: %v", len(got), keys(got))
	}
	if got["OSID"] != "FIXTURE-OSID" {
		t.Errorf("OSID = %q", got["OSID"])
	}

	reads := f.reads()
	if len(reads) == 0 {
		t.Fatal("no Network.getCookies request reached the endpoint")
	}
	for i, urls := range reads {
		seen := map[string]bool{}
		for _, u := range urls {
			seen[u] = true
		}
		for _, want := range []string{"https://messages.google.com", "https://www.google.com"} {
			if !seen[want] {
				t.Errorf("read %d asked for %v, which does not cover %s", i, urls, want)
			}
		}
	}
	// __Secure-1PSIDTS is read last, because it rotates: that means at least
	// two reads.
	if len(reads) < 2 {
		t.Errorf("the capture made %d reads; __Secure-1PSIDTS must be read last, in a second read", len(reads))
	}
}

// A capture that never sees OSID is refused rather than returning an unusable
// set. This is the .google.com-only failure the spec names.
func TestWaitForCookiesRejectsACaptureMissingOSID(t *testing.T) {
	var without []cdpCookie
	for _, c := range goodCookies() {
		if c.Name == "OSID" {
			continue
		}
		without = append(without, c)
	}
	f := newFakeCDP(t, without)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := dialCDP(ctx, f.wsURL())
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer conn.close()

	var announced string
	_, err = conn.waitForCookies(ctx, func(format string, args ...any) {
		announced = fmt.Sprintf(format, args...)
	})
	if !errors.Is(err, ErrOSIDNeverAppeared) {
		t.Fatalf("got %v, want ErrOSIDNeverAppeared", err)
	}
	if !strings.Contains(err.Error(), "messages.google.com") {
		t.Errorf("the error does not say where OSID comes from: %v", err)
	}
	// The owner is told what is still missing, by name, and never a value.
	if !strings.Contains(announced, "OSID") {
		t.Errorf("the progress line does not name OSID: %q", announced)
	}
	if strings.Contains(announced, "FIXTURE-") {
		t.Errorf("the progress line echoed a cookie value: %q", announced)
	}
}

// F-6: the no-Chrome path is deterministic, on a machine with Chrome or
// without one.
func TestFindChromeIsHermeticallyTestable(t *testing.T) {
	never := func(string) (string, error) { return "", errors.New("not on PATH") }

	// Nothing anywhere.
	if _, err := FindChromeIn("", nil, never); !errors.Is(err, ErrNoChrome) {
		t.Fatalf("got %v, want ErrNoChrome", err)
	}
	// A candidate that does not exist is not a browser.
	if _, err := FindChromeIn("", []string{filepath.Join(t.TempDir(), "ghost")}, never); !errors.Is(err, ErrNoChrome) {
		t.Fatalf("got %v, want ErrNoChrome", err)
	}

	// A platform candidate is preferred over PATH.
	dir := t.TempDir()
	candidate := filepath.Join(dir, "chrome-candidate")
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	onPath := filepath.Join(dir, "chrome-on-path")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found := func(string) (string, error) { return onPath, nil }
	got, err := FindChromeIn("", []string{candidate}, found)
	if err != nil {
		t.Fatal(err)
	}
	if got != candidate {
		t.Errorf("FindChromeIn chose %s, want the platform candidate %s", got, candidate)
	}
	// With no candidate it falls through to PATH.
	got, err = FindChromeIn("", nil, found)
	if err != nil {
		t.Fatal(err)
	}
	if got != onPath {
		t.Errorf("FindChromeIn chose %s, want the PATH hit %s", got, onPath)
	}
	// AGENT_GM_CHROME wins over both.
	env := filepath.Join(dir, "chrome-env")
	if err := os.WriteFile(env, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = FindChromeIn(env, []string{candidate}, found)
	if err != nil {
		t.Fatal(err)
	}
	if got != env {
		t.Errorf("FindChromeIn chose %s, want AGENT_GM_CHROME %s", got, env)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
