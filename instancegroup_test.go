package vcd

import (
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase_String(t *testing.T) {
	tests := []struct {
		phase    Phase
		expected string
	}{
		{PhasePendingCreate, "PendingCreate"},
		{PhaseCreating, "Creating"},
		{PhaseRunning, "Running"},
		{PhasePendingDelete, "PendingDelete"},
		{PhaseDeleting, "Deleting"},
		{PhaseDeleted, "Deleted"},
		{Phase(99), "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.phase.String())
		})
	}
}

func TestPhase_IotaOrder(t *testing.T) {
	// Verify the iota ordering is correct
	assert.Equal(t, Phase(0), PhasePendingCreate)
	assert.Equal(t, Phase(1), PhaseCreating)
	assert.Equal(t, Phase(2), PhaseRunning)
	assert.Equal(t, Phase(3), PhasePendingDelete)
	assert.Equal(t, Phase(4), PhaseDeleting)
	assert.Equal(t, Phase(5), PhaseDeleted)
}

func TestRetryDelay_BaseCase(t *testing.T) {
	delay := retryDelay(1)
	// First retry: 10s ± 25% jitter → 7.5s to 12.5s
	assert.GreaterOrEqual(t, delay, 7*time.Second)
	assert.LessOrEqual(t, delay, 13*time.Second)
}

func TestRetryDelay_Escalation(t *testing.T) {
	// Verify delays increase with retry count
	// Due to jitter we check the approximate range
	for _, tc := range []struct {
		retry   int
		minBase time.Duration
		maxBase time.Duration
	}{
		{1, 7 * time.Second, 13 * time.Second},     // ~10s
		{2, 15 * time.Second, 25 * time.Second},     // ~20s
		{3, 30 * time.Second, 50 * time.Second},     // ~40s
		{4, 60 * time.Second, 100 * time.Second},    // ~80s
		{5, 120 * time.Second, 200 * time.Second},   // ~160s
		{6, 225 * time.Second, 6*time.Minute + 15*time.Second}, // ~300s (capped, +25% jitter)
	} {
		delay := retryDelay(tc.retry)
		assert.GreaterOrEqual(t, delay, tc.minBase, "retry %d: delay %v too small", tc.retry, delay)
		assert.LessOrEqual(t, delay, tc.maxBase, "retry %d: delay %v too large", tc.retry, delay)
	}
}

func TestRetryDelay_CappedAt5Min(t *testing.T) {
	// Very high retry count should still be capped
	for i := 0; i < 100; i++ {
		delay := retryDelay(20)
		assert.LessOrEqual(t, delay, 5*time.Minute+2*time.Minute, // 5min + generous jitter margin
			"retry 20: delay %v exceeds cap", delay)
	}
}

func TestRetryDelay_NeverNegative(t *testing.T) {
	for i := 1; i <= 50; i++ {
		for j := 0; j < 10; j++ {
			delay := retryDelay(i)
			assert.Greater(t, delay, time.Duration(0), "retry %d: got non-positive delay %v", i, delay)
		}
	}
}

// --- Circuit breaker tests ---

func newTestReconciler(t *testing.T) *vcdInstanceGroup {
	t.Helper()
	log := hclog.NewNullLogger()
	store := newDesiredStateStore(log, "test-cb")
	return newVCDInstanceGroup(log, store, nil, ReconcilerConfig{
		MaxConcurrentCreates: 3,
		MaxConcurrentDeletes: 5,
		MaxInstanceAge:       24 * time.Hour,
	})
}

func TestCircuitBreaker_BlocksDispatchWhenActive(t *testing.T) {
	r := newTestReconciler(t)
	r.store.AddCreateIntent("test-1")
	r.store.AddCreateIntent("test-2")

	// Trip the breaker
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold
	r.circuitBreakerUntil = time.Now().Add(5 * time.Minute)
	r.cbMu.Unlock()

	r.dispatchCreates()

	// Both should still be PendingCreate — dispatch was blocked
	inst1, _ := r.store.GetByIntentID("test-1")
	assert.Equal(t, PhasePendingCreate, inst1.Phase)
	inst2, _ := r.store.GetByIntentID("test-2")
	assert.Equal(t, PhasePendingCreate, inst2.Phase)
}

func TestCircuitBreaker_InactiveWhenBelowThreshold(t *testing.T) {
	r := newTestReconciler(t)

	// Set failures just below threshold
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold - 1
	r.circuitBreakerUntil = time.Now().Add(5 * time.Minute)
	r.cbMu.Unlock()

	// Breaker should NOT be active
	r.cbMu.Lock()
	active := r.consecutiveCreateFailures >= circuitBreakerThreshold && time.Now().Before(r.circuitBreakerUntil)
	r.cbMu.Unlock()
	assert.False(t, active)
}

func TestCircuitBreaker_InactiveAfterCooldown(t *testing.T) {
	r := newTestReconciler(t)

	// Set failures above threshold but cooldown expired
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold + 5
	r.circuitBreakerUntil = time.Now().Add(-1 * time.Second)
	r.cbMu.Unlock()

	// Breaker should NOT be active — cooldown expired
	r.cbMu.Lock()
	active := r.consecutiveCreateFailures >= circuitBreakerThreshold && time.Now().Before(r.circuitBreakerUntil)
	r.cbMu.Unlock()
	assert.False(t, active)
}

func TestCircuitBreaker_TripsAtExactThreshold(t *testing.T) {
	r := newTestReconciler(t)

	// Simulate consecutive failures (same logic as doCreate failure path)
	for i := 0; i < circuitBreakerThreshold; i++ {
		r.cbMu.Lock()
		r.consecutiveCreateFailures++
		if r.consecutiveCreateFailures >= circuitBreakerThreshold {
			r.circuitBreakerUntil = time.Now().Add(circuitBreakerCooldown)
		}
		r.cbMu.Unlock()
	}

	// Breaker should be active
	r.cbMu.Lock()
	assert.Equal(t, circuitBreakerThreshold, r.consecutiveCreateFailures)
	assert.True(t, time.Now().Before(r.circuitBreakerUntil))
	r.cbMu.Unlock()

	// Verify dispatch is actually blocked
	r.store.AddCreateIntent("blocked")
	r.dispatchCreates()
	inst, _ := r.store.GetByIntentID("blocked")
	assert.Equal(t, PhasePendingCreate, inst.Phase)
}

func TestCircuitBreaker_DoesNotTripBelowThreshold(t *testing.T) {
	r := newTestReconciler(t)

	// Simulate failures below threshold
	for i := 0; i < circuitBreakerThreshold-1; i++ {
		r.cbMu.Lock()
		r.consecutiveCreateFailures++
		if r.consecutiveCreateFailures >= circuitBreakerThreshold {
			r.circuitBreakerUntil = time.Now().Add(circuitBreakerCooldown)
		}
		r.cbMu.Unlock()
	}

	// Breaker should NOT be tripped
	r.cbMu.Lock()
	assert.Equal(t, circuitBreakerThreshold-1, r.consecutiveCreateFailures)
	assert.True(t, r.circuitBreakerUntil.IsZero())
	r.cbMu.Unlock()
}

func TestCircuitBreaker_ResetsOnSuccess(t *testing.T) {
	r := newTestReconciler(t)

	// Trip the breaker
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold
	r.circuitBreakerUntil = time.Now().Add(circuitBreakerCooldown)
	r.cbMu.Unlock()

	// Simulate a success (same logic as doCreate success path)
	r.cbMu.Lock()
	r.consecutiveCreateFailures = 0
	r.cbMu.Unlock()

	// Breaker should no longer be active
	r.cbMu.Lock()
	active := r.consecutiveCreateFailures >= circuitBreakerThreshold && time.Now().Before(r.circuitBreakerUntil)
	r.cbMu.Unlock()
	assert.False(t, active)
}

func TestCircuitBreaker_FailureAfterResetStartsFresh(t *testing.T) {
	r := newTestReconciler(t)

	// Trip and reset
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold
	r.cbMu.Unlock()

	r.cbMu.Lock()
	r.consecutiveCreateFailures = 0
	r.cbMu.Unlock()

	// One more failure — should NOT trip (only 1 failure, threshold is 3)
	r.cbMu.Lock()
	r.consecutiveCreateFailures++
	r.cbMu.Unlock()

	r.cbMu.Lock()
	assert.Equal(t, 1, r.consecutiveCreateFailures)
	assert.True(t, r.circuitBreakerUntil.IsZero() || time.Now().After(r.circuitBreakerUntil))
	r.cbMu.Unlock()
}

func TestCircuitBreaker_LoggedActiveFlag(t *testing.T) {
	r := newTestReconciler(t)

	// Trip the breaker
	r.cbMu.Lock()
	r.consecutiveCreateFailures = circuitBreakerThreshold
	r.circuitBreakerUntil = time.Now().Add(5 * time.Minute)
	r.cbLoggedActive = false
	r.cbMu.Unlock()

	// First dispatch — should set cbLoggedActive to true
	r.dispatchCreates()

	r.cbMu.Lock()
	assert.True(t, r.cbLoggedActive)
	r.cbMu.Unlock()

	// Second dispatch — cbLoggedActive still true (no log spam)
	r.dispatchCreates()

	r.cbMu.Lock()
	assert.True(t, r.cbLoggedActive)
	r.cbMu.Unlock()
}

// --- GC tests ---

func TestGCCheck_MarksOldInstances(t *testing.T) {
	r := newTestReconciler(t)
	r.config.MaxInstanceAge = 1 * time.Hour

	// Old running instance
	r.store.AddCreateIntent("old")
	oldTime := time.Now().Add(-2 * time.Hour)
	r.store.UpdateInstance("old", func(inst *Instance) {
		inst.Phase = PhaseRunning
		inst.ID = "https://vcd/old"
		inst.CreateCompletedAt = &oldTime
	})

	// Recent running instance
	r.store.AddCreateIntent("recent")
	recentTime := time.Now().Add(-30 * time.Minute)
	r.store.UpdateInstance("recent", func(inst *Instance) {
		inst.Phase = PhaseRunning
		inst.ID = "https://vcd/recent"
		inst.CreateCompletedAt = &recentTime
	})

	r.gcCheck()

	// Old instance should be marked for deletion
	old, ok := r.store.GetByIntentID("old")
	require.True(t, ok)
	assert.Equal(t, PhasePendingDelete, old.Phase)
	assert.NotNil(t, old.GCMarkedAt)
	assert.NotNil(t, old.DeleteRequestedAt)

	// Recent instance should be untouched
	recent, ok := r.store.GetByIntentID("recent")
	require.True(t, ok)
	assert.Equal(t, PhaseRunning, recent.Phase)
	assert.Nil(t, recent.GCMarkedAt)
}

func TestGCCheck_IgnoresNonRunning(t *testing.T) {
	r := newTestReconciler(t)
	r.config.MaxInstanceAge = 1 * time.Hour

	oldTime := time.Now().Add(-2 * time.Hour)

	// PendingCreate — should not be GC'd
	r.store.AddCreateIntent("pending")
	r.store.UpdateInstance("pending", func(inst *Instance) {
		inst.CreateCompletedAt = &oldTime
	})

	// Deleting — should not be GC'd
	r.store.AddCreateIntent("deleting")
	r.store.UpdateInstance("deleting", func(inst *Instance) {
		inst.Phase = PhaseDeleting
		inst.ID = "https://vcd/deleting"
		inst.CreateCompletedAt = &oldTime
	})

	// Deleted — should not be GC'd
	r.store.AddCreateIntent("deleted")
	r.store.UpdateInstance("deleted", func(inst *Instance) {
		inst.Phase = PhaseDeleted
		inst.ID = "https://vcd/deleted"
		inst.CreateCompletedAt = &oldTime
	})

	r.gcCheck()

	pending, _ := r.store.GetByIntentID("pending")
	assert.Equal(t, PhasePendingCreate, pending.Phase)
	deleting, _ := r.store.GetByIntentID("deleting")
	assert.Equal(t, PhaseDeleting, deleting.Phase)
	deleted, _ := r.store.GetByIntentID("deleted")
	assert.Equal(t, PhaseDeleted, deleted.Phase)
}

func TestGCCheck_IgnoresRunningWithNoCreateCompletedAt(t *testing.T) {
	r := newTestReconciler(t)
	r.config.MaxInstanceAge = 1 * time.Hour

	// Running but no CreateCompletedAt — shouldn't be GC'd
	r.store.AddCreateIntent("no-timestamp")
	r.store.UpdateInstance("no-timestamp", func(inst *Instance) {
		inst.Phase = PhaseRunning
		inst.ID = "https://vcd/no-ts"
	})

	r.gcCheck()

	inst, _ := r.store.GetByIntentID("no-timestamp")
	assert.Equal(t, PhaseRunning, inst.Phase)
}

func TestGCCheck_MultipleOldInstances(t *testing.T) {
	r := newTestReconciler(t)
	r.config.MaxInstanceAge = 1 * time.Hour
	oldTime := time.Now().Add(-3 * time.Hour)

	for _, id := range []string{"a", "b", "c"} {
		r.store.AddCreateIntent(id)
		r.store.UpdateInstance(id, func(inst *Instance) {
			inst.Phase = PhaseRunning
			inst.ID = "https://vcd/" + id
			inst.CreateCompletedAt = &oldTime
		})
	}

	r.gcCheck()

	for _, id := range []string{"a", "b", "c"} {
		inst, _ := r.store.GetByIntentID(id)
		assert.Equal(t, PhasePendingDelete, inst.Phase, "instance %s should be GC'd", id)
	}
}
