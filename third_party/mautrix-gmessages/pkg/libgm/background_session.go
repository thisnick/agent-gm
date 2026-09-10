package libgm

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// RunBackground serves one push or RPC batch without claiming an active
// session or starting the recovery pinger. Callers must serialize calls and
// must not run Connect/Disconnect concurrently. The listener remains open
// while request executes, then drains until idle. Context cancels its HTTP.
func (c *Client) RunBackground(ctx context.Context, request func(context.Context) error) error {
	if err := c.checkLoggedIn(); err != nil {
		return err
	}
	if c.sessionHandler.sessionID == "" {
		c.sessionHandler.sessionID = uuid.NewString()
	}
	c.backgroundBusy.Store(true)
	defer c.backgroundBusy.Store(false)
	ready := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- c.doLongPoll(ctx, true, true, func() { close(ready) }) }()
	select {
	case <-ready:
	case <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("background listener failed to open")
	}
	var requestErr error
	if request != nil {
		requestErr = request(ctx)
	}
	c.backgroundBusy.Store(false)
	clean := <-done
	ackErr := c.sessionHandler.sendAckRequestContext(ctx)
	if requestErr != nil {
		return requestErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !clean {
		return errors.New("background listener closed uncleanly")
	}
	return ackErr
}

func waitBackgroundRetry(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
