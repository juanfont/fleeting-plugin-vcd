package vcd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
