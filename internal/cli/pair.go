package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/thisnick/agent-gm/internal/config"
)

// DefaultPairingTimeout is how long `agm pair` waits for the owner to tap the
// emoji before giving up (spec section 11.4).
const DefaultPairingTimeout = 5 * time.Minute

// pair is one command from start to paired: it covers POST, GET and DELETE
// /v1/pairing/* (spec section 11.3). The GET is the poll; Ctrl-C issues the
// DELETE, which is why the abandon route has no command of its own.
//
// Cookies are never a flag value. They are captured over CDP from a
// short-lived Chrome profile, or read from --paste / --paste-file: a secret
// must not appear in argv (spec section 12.1).
func (r *runner) pair(inv *invocation) error {
	cookies, err := r.pairingCookies(inv)
	if err != nil {
		return err
	}

	if inv.boolean("--refresh-cookies") {
		accountID := inv.str("--account")
		if accountID == "" {
			accountID = r.env.Getenv("AGENT_GM_ACCOUNT")
		}
		if accountID == "" {
			return flagErr("--account", "is required by `agm pair --refresh-cookies`: "+
				"a refresh re-authenticates one existing pairing")
		}
		resp, err := r.client.Do(r.ctx, Request{
			Method: http.MethodPost,
			Path:   "/v1/accounts/" + accountID + "/refresh-cookies",
			Body:   map[string]any{"cookies": cookies},
		})
		if err != nil {
			return err
		}
		return r.out.Emit(resp)
	}

	body := map[string]any{"cookies": cookies}
	if v := inv.str("--account"); v != "" {
		body["account_id"] = v
	}
	if inv.has("--device-index") {
		n, err := inv.integer("--device-index")
		if err != nil {
			return err
		}
		body["device_index"] = n
	}

	start, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost, Path: "/v1/pairing/start", Body: body,
	})
	if err != nil {
		return err
	}
	var started struct {
		PairingID string `json:"pairing_id"`
		Emoji     string `json:"emoji"`
	}
	if err := json.Unmarshal(start.Data, &started); err != nil || started.PairingID == "" {
		return &ContractError{Msg: "POST /v1/pairing/start answered without a pairing_id"}
	}
	if started.Emoji != "" {
		r.out.Infof("Tap %s on the phone to confirm the pairing.", started.Emoji)
	}

	return r.pollPairing(inv, started.PairingID)
}

// pollPairing is the GET, and the DELETE that Ctrl-C issues.
func (r *runner) pollPairing(inv *invocation, pairingID string) error {
	timeout := r.g.timeout
	if timeout == 0 {
		timeout = DefaultPairingTimeout
	}
	deadline := r.env.Now().Add(timeout)

	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt)
	defer stop()

	for {
		select {
		case <-ctx.Done():
			// Abandoning leaves nothing behind (section 16 Slice 2 test 38).
			r.out.Infof("abandoning the pairing")
			abandonCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := r.client.Do(abandonCtx, Request{
				Method: http.MethodDelete, Path: "/v1/pairing/" + pairingID,
			}); err != nil {
				r.out.Verbosef("abandoning the pairing failed: %v", err)
			}
			return usageErr("pairing abandoned")
		default:
		}

		resp, err := r.client.Do(r.ctx, Request{
			Method: http.MethodGet, Path: "/v1/pairing/" + pairingID,
		})
		if err != nil {
			return err
		}
		var state struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(resp.Data, &state); err != nil {
			return &ContractError{Msg: "GET /v1/pairing/{id} answered without a state"}
		}
		switch state.State {
		case "paired", "failed", "expired":
			return r.out.Emit(resp)
		}
		if !r.env.Now().Before(deadline) {
			return &WaitTimeoutError{WaitFor: "the pairing to be confirmed on the phone"}
		}
		r.out.Verbosef("pairing %s is %s", pairingID, state.State)
		time.Sleep(r.env.PollInterval)
	}
}

// pairingCookies gets the Google cookies: from --paste-file, from stdin with
// --paste, or from the short-lived Chrome profile of section 11.4.
func (r *runner) pairingCookies(inv *invocation) (map[string]string, error) {
	if path := inv.str("--paste-file"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, flagErr("--paste-file", "%s could not be read: %v", path, err)
		}
		return parsePasteOrUsage(string(raw))
	}
	if inv.boolean("--paste") {
		r.out.Infof("Paste the cURL command or cookie JSON, then Ctrl-D:")
		raw, err := io.ReadAll(r.env.Stdin)
		if err != nil {
			return nil, localErr(err, "the paste could not be read")
		}
		return parsePasteOrUsage(string(raw))
	}

	chrome, err := FindChrome(r.env.Getenv("AGENT_GM_CHROME"))
	if err != nil {
		return nil, &LocalError{Msg: NoChromeMessage(r.cred.Server)}
	}
	r.out.Infof("Opening a short-lived Chrome profile; sign in to Google and leave the tab open.")
	capture := &Capture{
		Chrome:      chrome,
		ProfileRoot: config.StateDir(),
		Logf:        func(format string, args ...any) { r.out.Verbosef(format, args...) },
		Environ:     r.env.Getenv,
		Timeout:     r.g.timeout,
	}
	cookies, err := capture.Run(r.ctx)
	if err != nil {
		return nil, err
	}
	return cookies, nil
}

func parsePasteOrUsage(raw string) (map[string]string, error) {
	cookies, err := ParsePaste(raw)
	if err != nil {
		return nil, &UsageError{Msg: err.Error(), Err: err}
	}
	return cookies, nil
}
