package vcd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testGate returns a gate with deterministic pauses: initial, 2x, 4x, 8x.
func testGate(initial time.Duration) *throttleGate {
	g := newThrottleGate(testLogger(), "throttle-test")
	g.bo = backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(initial),
		backoff.WithMultiplier(2),
		backoff.WithMaxInterval(8*initial),
		backoff.WithRandomizationFactor(0),
		backoff.WithMaxElapsedTime(0),
	)
	return g
}

func TestIsOperationLimitError(t *testing.T) {
	assert.True(t, isOperationLimitError(errors.New(`API Error: 503: [ 5d1c ] The maximum number of simultaneous operations for organization "acme" has been reached.`)))
	assert.True(t, isOperationLimitError(errors.New("THE MAXIMUM NUMBER OF SIMULTANEOUS OPERATIONS")))
	assert.False(t, isOperationLimitError(nil))
	assert.False(t, isOperationLimitError(errors.New("API Error: 503: Service Unavailable")))
	assert.False(t, isOperationLimitError(errors.New("GET https://vcd/api/vApp/vapp-9a503f1c: 404 not found")))
}

func TestThrottleGate_PauseGrowsPerEpisode(t *testing.T) {
	g := testGate(time.Minute)
	now := time.Now()
	g.now = func() time.Time { return now }

	g.throttled("a")
	assert.True(t, g.paused())
	assert.Equal(t, now.Add(time.Minute), g.pausedUntil)

	// Concurrent rejections during the same pause do not escalate it.
	g.throttled("b")
	g.throttled("c")
	assert.Equal(t, now.Add(time.Minute), g.pausedUntil)

	now = now.Add(time.Minute)
	assert.False(t, g.paused())
	g.throttled("d")
	assert.Equal(t, now.Add(2*time.Minute), g.pausedUntil)
}

func TestThrottleGate_SuccessResetsPause(t *testing.T) {
	g := testGate(time.Minute)
	now := time.Now()
	g.now = func() time.Time { return now }

	g.throttled("a")
	now = now.Add(time.Minute)
	g.throttled("b") // 2m
	now = now.Add(2 * time.Minute)

	g.succeeded()
	g.throttled("c")
	assert.Equal(t, now.Add(time.Minute), g.pausedUntil)
}

func TestThrottleGate_WaitBlocksUntilPauseEnds(t *testing.T) {
	g := testGate(50 * time.Millisecond)
	g.throttled("a")
	start := time.Now()
	require.NoError(t, g.wait(context.Background()))
	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
	assert.False(t, g.paused())
}

func TestThrottleGate_WaitHonoursContext(t *testing.T) {
	g := testGate(time.Hour)
	g.throttled("a")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, g.wait(ctx), context.DeadlineExceeded)
}

func TestThrottleGate_NilIsNoop(t *testing.T) {
	var g *throttleGate
	g.throttled("a")
	g.succeeded()
	assert.False(t, g.paused())
	require.NoError(t, g.wait(context.Background()))
}

func TestThrottleGate_PauseGaugeClearsWhenPauseEnds(t *testing.T) {
	g := testGate(time.Minute)
	g.group = t.Name()
	now := time.Now()
	g.now = func() time.Time { return now }

	g.throttled("a")
	assert.Equal(t, 60.0, gaugeValue(t, "fleeting_vcd_throttle_pause_seconds", t.Name()))

	now = now.Add(time.Minute)
	assert.False(t, g.paused())
	assert.Equal(t, 0.0, gaugeValue(t, "fleeting_vcd_throttle_pause_seconds", t.Name()))
}
