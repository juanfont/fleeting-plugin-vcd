package vcd

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLoggedHeldReconciler(t *testing.T, ops *fakeOps) (*vcdInstanceGroup, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	log := hclog.New(&hclog.LoggerOptions{Output: buf, Level: hclog.Info})
	r := newVCDInstanceGroup(log, newDesiredStateStore(log, t.Name()), nil, ReconcilerConfig{})
	r.ops = ops
	return r, buf
}

func TestStartupHold_LogsProgressPeriodically(t *testing.T) {
	ops := &fakeOps{delete: func(string) error { select {} }}
	ops.setVApps([]*govcd.VApp{testVApp(1, false), testVApp(2, false), testVApp(3, false), testVApp(4, false)})
	r, buf := newLoggedHeldReconciler(t, ops)

	r.reconcileOnce(false)
	assert.Contains(t, buf.String(), "startup cleanup started")
	assert.Contains(t, buf.String(), "leftovers=4")

	// Within the interval: no progress line yet.
	r.reconcileOnce(false)
	assert.NotContains(t, buf.String(), "startup cleanup in progress")

	r.lastHoldLog = time.Now().Add(-2 * holdLogInterval)
	require.Eventually(t, func() bool { return r.store.LeftoverCount() == 4 }, time.Second, 5*time.Millisecond)
	r.reconcileOnce(false)
	out := buf.String()
	assert.Contains(t, out, "startup cleanup in progress")
	assert.Contains(t, out, "leftovers_remaining=4")
	assert.Contains(t, out, "deleting=3")
	assert.Contains(t, out, "creates_released_in=")
}

func TestStartupHold_LogsWhileWaitingForFirstCompletePoll(t *testing.T) {
	ops := &fakeOps{listErr: errors.New("vCD unreachable")}
	r, buf := newLoggedHeldReconciler(t, ops)

	r.reconcileOnce(false)
	r.lastHoldLog = time.Now().Add(-2 * holdLogInterval)
	r.reconcileOnce(false)
	assert.Contains(t, buf.String(), "startup cleanup waiting for a complete poll")
}

func TestStartupHold_Metrics(t *testing.T) {
	ops := &fakeOps{delete: func(string) error { select {} }}
	ops.setVApps([]*govcd.VApp{testVApp(1, false), testVApp(2, false)})
	r, _ := newLoggedHeldReconciler(t, ops)

	r.reconcileOnce(false)
	assert.Equal(t, 1.0, gaugeValue(t, "fleeting_vcd_startup_hold", t.Name()))
	assert.Equal(t, 2.0, gaugeValue(t, "fleeting_vcd_startup_leftovers_remaining", t.Name()))

	r.startedAt = time.Now().Add(-2 * r.config.StartupCleanupTimeout)
	r.reconcileOnce(false)
	assert.Equal(t, 0.0, gaugeValue(t, "fleeting_vcd_startup_hold", t.Name()))
}
