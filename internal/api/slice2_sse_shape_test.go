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
	// V-1. Section 4.4 says every JSON surface renders an instant as RFC 3339
	// UTC with millisecond precision, and every DTO pads to three digits. The
	// stream did not: it marshalled a bare time.Time, and Go's RFC3339Nano
	// STRIPS trailing zeros, so ".507Z" and ".52Z" appeared in one stream
	// seconds apart. A client parsing a fixed three-digit fraction works
	// until a timestamp lands on a multiple of ten milliseconds -- one frame
	// in ten, which is the worst possible failure rate: often enough to
	// happen in production, rare enough to survive a test run.
	//
	// Plant: marshal StateChange.At as a time.Time again and this fails on
	// roughly one run in ten -- so the assertion below is also made against a
	// value CHOSEN to strip, in the accounts package's TestStateChangeRendersExactlyThreeFractionalDigits.
	at, _ := frame["at"].(string)
	assertMillisecondWidth(t, "the snapshot frame's at", at)

	// The fields that DO have a value still carry it.
	if got, _ := frame["account_id"].(string); got != accountID {
		t.Errorf("the snapshot frame's account_id is %q, want %s", got, accountID)
	}
	if got, _ := frame["to"].(string); got != "connected" {
		t.Errorf("the snapshot frame's to is %q, want connected", got)
	}
}

// assertMillisecondWidth holds section 4.4's rule: RFC 3339, UTC, exactly
// three fractional digits.
func assertMillisecondWidth(t *testing.T, what, s string) {
	t.Helper()
	if s == "" {
		t.Errorf("%s is empty", what)
		return
	}
	dot := strings.LastIndex(s, ".")
	if dot < 0 || !strings.HasSuffix(s, "Z") {
		t.Errorf("%s is %q, want RFC 3339 UTC with a fractional part (section 4.4)", what, s)
		return
	}
	if digits := len(s) - dot - 2; digits != 3 {
		t.Errorf("%s is %q: %d fractional digits, want exactly 3. Go strips trailing "+
			"zeros, so a client parsing a fixed .SSS breaks on one timestamp in ten",
			what, s, digits)
	}
}
