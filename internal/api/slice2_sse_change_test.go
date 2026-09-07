package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// U-2. The SSE route had no test that a state CHANGE ever reaches a client:
// the snapshot was asserted, the feed was asserted in internal/accounts, and
// the wire between them was not. A subscription that is opened and never
// published to looks exactly like a working stream on a quiet server, which
// is the failure mode `agm session --watch` exists to make visible.
//
// This opens the stream, drives a real transition through the sign-out route,
// and reads the frame off the socket.
//
// Plant: drop the feed publish in Supervisor.announce, or the subscription in
// the events handler, and this fails at "no account.state_changed frame
// arrived". Planted 2026-09-07.
func TestSlice2_AStateChangeReachesAnSSEClient(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("sse-change@example.test")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.HTTP.URL+"/v1/accounts/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", resp.StatusCode)
	}

	type frame struct {
		event string
		data  map[string]any
	}
	frames := make(chan frame, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					continue
				}
				select {
				case frames <- frame{event: event, data: payload}:
				default:
				}
			}
		}
		close(frames)
	}()

	// The snapshot first: it is written before the flush, so waiting for it
	// also proves the handler has subscribed before the change is driven --
	// a change published to nobody would make this test pass by racing.
	select {
	case f, open := <-frames:
		if !open {
			t.Fatal("the stream closed before the snapshot")
		}
		if f.event != "account.state" {
			t.Fatalf("the first frame is %q, want account.state", f.event)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no snapshot frame arrived")
	}

	// A real transition, through a real route.
	s.call("POST", "/v1/accounts/"+accountID+"/sign-out",
		map[string]any{"confirm": true}).ok(t, 200)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, open := <-frames:
			if !open {
				t.Fatal("the stream closed before the change arrived")
			}
			if f.event != "account.state_changed" {
				continue
			}
			if got, _ := f.data["account_id"].(string); got != accountID {
				t.Errorf("the change names account %q, want %s", got, accountID)
			}
			if got, _ := f.data["to"].(string); got != "signed_out" {
				t.Errorf("the change's to is %q, want signed_out", got)
			}
			if got, _ := f.data["state_reason"].(string); got != "credentials" {
				t.Errorf("the change's state_reason is %v, want credentials", f.data["state_reason"])
			}
			assertMillisecondWidth(t, "the change frame's at", func() string {
				v, _ := f.data["at"].(string)
				return v
			}())
			if f.data["from"] == nil {
				t.Error("the change's from is null; on a CHANGE there is a previous " +
					"state and it is what tells a client what moved")
			}
			return
		case <-deadline:
			t.Fatal("no account.state_changed frame arrived: a subscription that is " +
				"never published to is indistinguishable from a quiet server, which " +
				"is the whole thing `agm session --watch` exists to show")
		}
	}
}
