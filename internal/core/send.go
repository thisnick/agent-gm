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
// vocabulary (spec sections 3.7, 7.2).
//
// The CREATE_RCS retry itself lives in the gm adapter, which retries exactly
// once, so a CREATE_RCS arriving here is the *second* one and there is
// nothing further to try.
//
// **Google's real status always survives.** This used to relabel ANY failure
// as `config_version_stale` whenever the compiled and live ConfigVersions
// differed, and the Slice 2 live gate showed what that costs: a named group
// start failed for an entirely unrelated reason -- the name was being sent on
// the first call -- while the versions happened to differ, as they normally
// do between pin bumps. The operator was told to bump the pin, which would
// not have helped, and the actual status was discarded. That is precisely the
// misdiagnosis D32 exists to prevent, committed by the very code that names
// it.
//
// So the status decides the code, always:
//
//   - a second CREATE_RCS is `google_error`: a named status Agent GM has
//     already done the one documented thing about;
//   - anything else is `google_undocumented_status` carrying the bare
//     integer, claiming no meaning for it.
//
// A version mismatch is added as CONTEXT on whichever of those it is --
// both versions in `details`, and a sentence saying a pin bump may be the fix
// -- because it is genuinely useful when a create fails and genuinely not a
// diagnosis on its own. `config_version_stale` remains what D32 says it is:
// a fact reported by GET /v1/health, never an error code.
func ClassifyResolve(res gm.ResolveResult, compiled, live gm.ConfigVersion) (*gm.Conversation, error) {
	if res.Status == gm.ResolveStatusSuccess {
		if res.Conversation == nil {
			return nil, ErrResolveNoConversation
		}
		return res.Conversation, nil
	}

	var e *gm.Error
	if res.Status == gm.ResolveStatusCreateRCS {
		e = gm.ResolveCreateRCSTwice()
	} else {
		e = gm.UndocumentedResolveStatus(res.Status)
	}
	if !compiled.SameDate(live) {
		e = gm.WithConfigVersionContext(e, compiled, live)
	}
	return nil, e
}
