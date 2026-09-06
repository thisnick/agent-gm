package core

import (
	"context"
	"errors"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
)

// SendAttempt performs one library send. It is called once per attempt by
// SendWithRetry, always with the same TmpID.
type SendAttempt func(ctx context.Context) (gm.SendResult, error)

// SendWithRetry is the only retry Agent GM performs, and it is not Agent GM's
// own policy: it is the three upstream-derived retries for transient Google
// statuses, executed inside the same request (spec sections 6.1, 3.7).
//
// There is no queue, no worker and no retry schedule Agent GM owns (D4). The
// backoffs are gm.SendRetryBackoff -- [3s, 8s, 20s] at the pin -- measured on
// the injected clock, so a test proves the schedule without waiting 31
// seconds. Only FAILURE_2 and FAILURE_3 are transient; FAILURE_4 is
// not_default_sms_app and is returned at once, because retrying it sends
// nothing and delays an answer the owner has to act on.
//
// **The caller mints one TmpID and reuses it across every attempt** (spec
// section 6.3). A fresh TmpID per attempt would let two attempts of one
// request correlate to two operations, which is exactly the duplicate-message
// hazard the whole idempotency design exists to prevent.
func SendWithRetry(ctx context.Context, clk clock.Clock, attempt SendAttempt) (gm.SendResult, error) {
	for i := 0; ; i++ {
		res, err := attempt(ctx)
		if err != nil {
			// A transport or library error is classified by internal/gm and
			// is never retried here: ErrPhoneNotResponding in particular is
			// pending, not failed, and resending it is how a real person
			// gets the same text twice (D5).
			return res, err
		}
		if res.Status == gm.SendStatusSuccess {
			return res, nil
		}
		if !res.Status.IsTransient() || i >= len(gm.SendRetryBackoff) {
			return res, gm.SendFailure(res.Status)
		}
		if err := clk.Sleep(ctx, gm.SendRetryBackoff[i]); err != nil {
			return res, err
		}
	}
}

// ErrResolveNoConversation means Google answered SUCCESS with no conversation
// in the body, which is a contract violation rather than a refusal.
var ErrResolveNoConversation = errors.New("google answered SUCCESS with no conversation")

// ClassifyResolve turns a GetOrCreateConversation answer into Agent GM's own
// vocabulary (spec sections 3.7, 7.2, §16 Slice 2 test 11).
//
// The CREATE_RCS retry itself lives in the gm adapter, which retries exactly
// once with CreateRCSGroup=true exactly as upstream does, so a CREATE_RCS
// arriving here is the *second* one and there is nothing further to try.
//
// The order of the three failure diagnoses is the contract:
//
//  1. **A version mismatch wins.** If the compiled and live ConfigVersions
//     differ in year, month or day, a failed conversation-creating call is
//     `config_version_stale` naming both versions, whatever the status was.
//     The version diff is the whole detection rule (§3.7, D3); asserting on a
//     particular status number would re-import the unsourced claim §18.1
//     withdrew.
//  2. A second CREATE_RCS with matching versions is `google_error`: it is a
//     named status that Agent GM has already done the one documented thing
//     about.
//  3. Anything else with matching versions is `google_undocumented_status`,
//     carrying the bare integer and claiming no meaning for it.
func ClassifyResolve(res gm.ResolveResult, compiled, live gm.ConfigVersion) (*gm.Conversation, error) {
	if res.Status == gm.ResolveStatusSuccess {
		if res.Conversation == nil {
			return nil, ErrResolveNoConversation
		}
		return res.Conversation, nil
	}
	if !compiled.SameDate(live) {
		return nil, gm.ConfigVersionStale(compiled, live, res.Status)
	}
	if res.Status == gm.ResolveStatusCreateRCS {
		return nil, gm.ResolveCreateRCSTwice()
	}
	return nil, gm.UndocumentedResolveStatus(res.Status)
}
