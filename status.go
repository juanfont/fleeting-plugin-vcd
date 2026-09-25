package vcd

import (
	"sync"
	"time"
)

// DebugStatus is a snapshot of reconciler state for the debug server. The
// reconciler publishes it at the end of every cycle, so readers never touch
// fields owned by the reconcile goroutine.
type DebugStatus struct {
	LastReconcileAt              time.Time `json:"last_reconcile_at"`
	LastReconcileDurationSeconds float64   `json:"last_reconcile_duration_seconds"`
	LastPollError                string    `json:"last_poll_error,omitempty"`

	StartupHold        bool      `json:"startup_hold"`
	StartupPolled      bool      `json:"startup_polled"`
	LeftoversRemaining int       `json:"leftovers_remaining"`
	LeftoversDeleting  int       `json:"leftovers_deleting"`
	HoldStartedAt      time.Time `json:"hold_started_at,omitempty"`
	HoldTimeoutSeconds float64   `json:"hold_timeout_seconds"`

	ThrottlePausedUntil time.Time `json:"throttle_paused_until,omitempty"`
	BreakerFailures     int       `json:"breaker_consecutive_failures"`
	BreakerOpenUntil    time.Time `json:"breaker_open_until,omitempty"`

	OpsLimit        int `json:"ops_limit"`
	CreatesInFlight int `json:"creates_in_flight"`
	DeletesInFlight int `json:"deletes_in_flight"`
}

type statusBox struct {
	mu sync.Mutex
	st DebugStatus
}

func (b *statusBox) set(st DebugStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st = st
}

func (b *statusBox) get() DebugStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

// DebugStatus returns the state published by the last reconcile cycle.
func (r *vcdInstanceGroup) DebugStatus() DebugStatus {
	return r.status.get()
}

// publishStatus runs on the reconcile goroutine at the end of each cycle.
func (r *vcdInstanceGroup) publishStatus(started time.Time, pollErr error) {
	st := DebugStatus{
		LastReconcileAt:              time.Now(),
		LastReconcileDurationSeconds: time.Since(started).Seconds(),
		StartupHold:                  r.startupHold,
		StartupPolled:                r.startupPolled,
		HoldTimeoutSeconds:           r.config.StartupCleanupTimeout.Seconds(),
		ThrottlePausedUntil:          r.throttle.until(),
		OpsLimit:                     int(r.limiter.size),
		CreatesInFlight:              int(r.limiter.createsInUse.Load()),
		DeletesInFlight:              int(r.limiter.deletesInUse.Load()),
	}
	if pollErr != nil {
		st.LastPollError = pollErr.Error()
	}
	if r.startupHold {
		st.LeftoversRemaining, st.LeftoversDeleting = r.leftoverProgress()
		if r.startupPolled {
			st.HoldStartedAt = r.startedAt
		}
	}
	r.cbMu.Lock()
	st.BreakerFailures = r.consecutiveCreateFailures
	if time.Now().Before(r.circuitBreakerUntil) {
		st.BreakerOpenUntil = r.circuitBreakerUntil
	}
	r.cbMu.Unlock()
	r.status.set(st)
}
