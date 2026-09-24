package vcd

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/go-hclog"
)

const (
	throttleInitialPause = 15 * time.Second
	throttleMaxPause     = 5 * time.Minute
	// Wide jitter keeps plugins sharing one org from resuming in lockstep.
	throttleJitter = 0.5
)

// isOperationLimitError reports whether vCD rejected a request because the
// organization reached its simultaneous-operation limit. vCD rejects these
// before starting a task, so replaying the request is safe even for mutations.
func isOperationLimitError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "maximum number of simultaneous operations")
}

// throttleGate pauses the vCD work of an instance group after the org operation
// limit is hit. The limit is shared with every other client of the org, so the
// pause grows while throttling continues and resets once an operation
// completes. A nil gate never pauses.
type throttleGate struct {
	mu          sync.Mutex
	bo          *backoff.ExponentialBackOff
	pausedUntil time.Time
	now         func() time.Time
	log         hclog.Logger
	group       string
}

func newThrottleGate(log hclog.Logger, group string) *throttleGate {
	return &throttleGate{
		bo: backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(throttleInitialPause),
			backoff.WithMultiplier(2),
			backoff.WithMaxInterval(throttleMaxPause),
			backoff.WithRandomizationFactor(throttleJitter),
			backoff.WithMaxElapsedTime(0),
		),
		now:   time.Now,
		log:   log,
		group: group,
	}
}

// throttled records a rejected request. Rejections that arrive while a pause is
// active belong to the same episode and do not lengthen it.
func (g *throttleGate) throttled(desc string) {
	if g == nil {
		return
	}
	ThrottleEventsTotal.WithLabelValues(g.group).Inc()
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Before(g.pausedUntil) {
		return
	}
	pause := g.bo.NextBackOff()
	g.pausedUntil = now.Add(pause)
	ThrottlePauseSeconds.WithLabelValues(g.group).Set(pause.Seconds())
	g.log.Warn("vCD organization operation limit reached, pausing vCD operations",
		"operation", desc, "pause", pause)
}

// succeeded resets the pause length after an operation completes.
func (g *throttleGate) succeeded() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bo.Reset()
}

func (g *throttleGate) paused() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.now().Before(g.pausedUntil) {
		return true
	}
	ThrottlePauseSeconds.WithLabelValues(g.group).Set(0)
	return false
}

// wait blocks until no pause is active, including pauses extended while waiting.
func (g *throttleGate) wait(ctx context.Context) error {
	if g == nil {
		return ctx.Err()
	}
	for {
		g.mu.Lock()
		remaining := g.pausedUntil.Sub(g.now())
		g.mu.Unlock()
		if remaining <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
