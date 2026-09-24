package vcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpLimiter_DeletesLeaveOneSlotForCreates(t *testing.T) {
	l := newOpLimiter(4, 3, 5, nil)
	for i := 0; i < 3; i++ {
		require.True(t, l.tryAcquire(opDelete))
	}
	assert.False(t, l.tryAcquire(opDelete), "deletes are capped at total-1")

	require.True(t, l.tryAcquire(opCreate), "the reserved slot goes to a create")
	assert.False(t, l.tryAcquire(opCreate), "total exhausted")

	l.release(opDelete)
	assert.True(t, l.tryAcquire(opCreate))
}

func TestOpLimiter_CreatesMayUseEverySlot(t *testing.T) {
	l := newOpLimiter(4, 4, 5, nil)
	for i := 0; i < 4; i++ {
		require.True(t, l.tryAcquire(opCreate))
	}
	assert.False(t, l.tryAcquire(opDelete))
}

func TestOpLimiter_PerKindCaps(t *testing.T) {
	l := newOpLimiter(8, 2, 1, nil)
	require.True(t, l.tryAcquire(opCreate))
	require.True(t, l.tryAcquire(opCreate))
	assert.False(t, l.tryAcquire(opCreate))
	require.True(t, l.tryAcquire(opDelete))
	assert.False(t, l.tryAcquire(opDelete))
}

func TestOpLimiter_SingleSlotStillDeletes(t *testing.T) {
	l := newOpLimiter(1, 3, 5, nil)
	require.True(t, l.tryAcquire(opDelete))
	assert.False(t, l.tryAcquire(opCreate))
	l.release(opDelete)
	assert.True(t, l.tryAcquire(opCreate))
}

func TestOpLimiter_PausedRefusesEverything(t *testing.T) {
	gate := testGate(time.Hour)
	gate.throttled("test")
	l := newOpLimiter(4, 3, 5, gate)
	assert.False(t, l.tryAcquire(opCreate))
	assert.False(t, l.tryAcquire(opDelete))
}

func TestOpLimiter_DrainWaitsForInFlight(t *testing.T) {
	l := newOpLimiter(2, 2, 2, nil)
	require.True(t, l.tryAcquire(opCreate))

	done := make(chan error, 1)
	go func() { done <- l.drain(context.Background()) }()
	select {
	case <-done:
		t.Fatal("drain returned while an operation was in flight")
	case <-time.After(20 * time.Millisecond):
	}

	l.release(opCreate)
	require.NoError(t, <-done)
	assert.False(t, l.tryAcquire(opCreate), "drain keeps every slot")
}

func TestOpLimiter_DrainHonoursContext(t *testing.T) {
	l := newOpLimiter(2, 2, 2, nil)
	require.True(t, l.tryAcquire(opCreate))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, l.drain(ctx), context.DeadlineExceeded)
}
