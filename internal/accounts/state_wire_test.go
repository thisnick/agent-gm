package accounts_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
)

// V-1, against the values that actually strip. A stream test catches this
// only when a timestamp happens to land on a multiple of ten milliseconds;
// these are chosen to land there every time.
//
// Plant: marshal StateChange.At as a bare time.Time and this fails on the
// first two rows. Planted 2026-09-07.
func TestStateChangeRendersExactlyThreeFractionalDigits(t *testing.T) {
	base := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"a whole second", base, "2026-09-07T01:02:03.000Z"},
		{"a multiple of ten milliseconds", base.Add(520 * time.Millisecond),
			"2026-09-07T01:02:03.520Z"},
		{"an ordinary value", base.Add(507 * time.Millisecond),
			"2026-09-07T01:02:03.507Z"},
		{"a non-UTC zone renders as UTC", base.Add(90 * time.Millisecond).
			In(time.FixedZone("somewhere", 5*3600)), "2026-09-07T01:02:03.090Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(accounts.StateChange{
				AccountID: "acct_fixture", To: accounts.StateConnected, At: tc.at,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				At string `json:"at"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.At != tc.want {
				t.Errorf("at = %q, want %q: section 4.4's width is fixed, and Go's "+
					"default marshalling strips trailing zeros", got.At, tc.want)
			}
		})
	}
}
