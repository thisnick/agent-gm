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

// S-5. The SSE feed is a JSON surface, and section 4.7 says `state_reason` is
// a short machine-readable string **or null**. It was serving `""`: on the
// opening `account.state` snapshot, where `from` genuinely has no value
// because the client held no prior state, and on any change with no reason.
//
// An empty string and "there is none" are different, and a client branching
// on truthiness gets the same answer for both only by accident -- which is
// the same defect R-10c fixed on /v1/accounts, reappearing on the one route
// that did not go through the DTO layer.
//
// Plant: drop StateChange.MarshalJSON and this fails naming the field that
// came back as "". Planted 2026-09-07.
func TestSlice2_TheSSESnapshotWritesNullNotEmptyString(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("sse-shape@example.test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.HTTP.URL+"/v1/accounts/"+accountID+"/events", nil)
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

	// The snapshot is written before the first flush, so it is there to read
	// without waiting for anything to happen.
	var frame map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("the snapshot frame is not JSON: %v", err)
		}
		break
	}
	if frame == nil {
		t.Fatal("the stream sent no snapshot frame at all")
	}

	for _, field := range []string{"from", "state_reason"} {
		v, present := frame[field]
		if !present {
			t.Errorf("the snapshot frame omits %q entirely; section 4.7 says it is "+
				"present and null, not absent", field)
			continue
		}
		if v != nil {
			t.Errorf("the snapshot frame's %q is %#v, want null: on a snapshot the "+
				"client held no prior state, and \"\" is a value a client can read "+
				"as one", field, v)
		}
	}
	// The fields that DO have a value still carry it.
	if got, _ := frame["account_id"].(string); got != accountID {
		t.Errorf("the snapshot frame's account_id is %q, want %s", got, accountID)
	}
	if got, _ := frame["to"].(string); got != "connected" {
		t.Errorf("the snapshot frame's to is %q, want connected", got)
	}
}
