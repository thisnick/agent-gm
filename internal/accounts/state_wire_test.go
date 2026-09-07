package accounts_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/wire"
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

// W-1. The same bug as V-1, one struct over: AccountHealth, BackfillHealth
// and SweepHealth are served STRAIGHT out of this package by GET /v1/health
// and GET /v1/accounts/{id} -- they never pass through internal/api's DTO
// layer -- so Go's default encoder rendered their instants as RFC3339Nano,
// which strips trailing zeros. The same account's last_event_at came back
// ".120Z" from GET /v1/accounts, which IS a DTO route, and ".12Z" from
// GET /v1/health, while section 7.5 promises those routes serve the same
// object.
//
// The values here are chosen to strip -- a whole second, a multiple of ten
// milliseconds -- because a test over a live response catches this only when
// a timestamp happens to land on one, which is one reading in ten.
//
// Plant: type any of the three fields back to *time.Time and this does not
// compile, which is stronger than failing. Planted 2026-09-07.
func TestHealthBlocksRenderExactlyThreeFractionalDigits(t *testing.T) {
	whole := time.Date(2026, 9, 7, 16, 53, 20, 0, time.UTC)
	tenth := whole.Add(500 * time.Millisecond)
	odd := whole.Add(120 * time.Millisecond)

	h := accounts.AccountHealth{
		AccountID:   "acct_fixture",
		State:       accounts.StateConnected,
		LastEventAt: strPtr(wire.Instant(odd)),
		Backfill: accounts.BackfillHealth{
			State: "complete", CompletedAt: strPtr(wire.Instant(whole)),
		},
		Sweep: accounts.SweepHealth{LastSweepAt: strPtr(wire.Instant(tenth))},
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		LastEventAt *string `json:"last_event_at"`
		Backfill    struct {
			CompletedAt *string `json:"completed_at"`
		} `json:"backfill"`
		Sweep struct {
			LastSweepAt *string `json:"last_sweep_at"`
		} `json:"sweep"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		got   *string
		want  string
	}{
		{"last_event_at", got.LastEventAt, "2026-09-07T16:53:20.120Z"},
		{"backfill.completed_at", got.Backfill.CompletedAt, "2026-09-07T16:53:20.000Z"},
		{"sweep.last_sweep_at", got.Sweep.LastSweepAt, "2026-09-07T16:53:20.500Z"},
	} {
		if tc.got == nil {
			t.Errorf("%s is null", tc.field)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %q, want %q: section 4.4's width is fixed, and these "+
				"blocks are served without passing through the DTO layer",
				tc.field, *tc.got, tc.want)
		}
	}
}

func strPtr(s string) *string { return &s }

// And the same rule through the production path, so the assertion above is
// about what healthFor actually writes rather than about a fixture. The
// stored millisecond values are chosen to strip: a whole second for the
// backfill completion, a multiple of ten for the sweep.
func TestHealthForRendersStoredTimestampsAtAFixedWidth(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	acct, err := h.sup.Pair(ctx, fake.New("width@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Far enough ahead of the fake clock that the store's MAX() keeps it:
	// connecting already stamped a sweep, and the point here is the WIDTH of
	// the rendering, not the value.
	whole := time.Date(2036, 9, 7, 16, 53, 20, 0, time.UTC)
	if err := h.store.SetAccountBackfillComplete(ctx, acct.ID, whole); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountLastSweep(ctx, acct.ID, whole.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	block, err := h.sup.HealthFor(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"completed_at":"2036-09-07T16:53:20.000Z"`,
		`"last_sweep_at":"2036-09-07T16:53:20.500Z"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the health block does not contain %s:\n%s", want, raw)
		}
	}
}
