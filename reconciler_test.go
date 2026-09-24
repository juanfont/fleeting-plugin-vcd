package vcd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
)

type fakeOps struct {
	mu      sync.Mutex
	vApps   []*govcd.VApp
	polled  []string
	deleted []string
	create  func() (*createResult, error)
	delete  func(href string) error
}

func (f *fakeOps) getInstancesInInstanceGroup() ([]*govcd.VApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*govcd.VApp(nil), f.vApps...), nil
}

func (f *fakeOps) setVApps(vApps []*govcd.VApp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vApps = vApps
}

func (f *fakeOps) pollVM(vmHREF string) (*vmPollResult, error) {
	f.mu.Lock()
	f.polled = append(f.polled, vmHREF)
	f.mu.Unlock()
	return &vmPollResult{IP: "10.0.0.1", OSType: "debian12_64Guest"}, nil
}

func (f *fakeOps) polledCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.polled)
}

func (f *fakeOps) createInstance() (*createResult, error) {
	if f.create != nil {
		return f.create()
	}
	return nil, errors.New("unexpected create")
}

func (f *fakeOps) deleteInstance(href string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, href)
	f.mu.Unlock()
	if f.delete != nil {
		return f.delete(href)
	}
	return nil
}

func (f *fakeOps) cleanUpInstanceByName(name string) error {
	return f.deleteInstance("name:" + name)
}

func newFakeReconciler(t *testing.T, ops *fakeOps, config ReconcilerConfig) *vcdInstanceGroup {
	t.Helper()
	log := testLogger()
	r := newVCDInstanceGroup(log, newDesiredStateStore(log, t.Name()), nil, config)
	r.ops = ops
	return r
}

// testVApp builds a discovered vApp named runner-<i>, with one VM unless empty.
func testVApp(i int, empty bool) *govcd.VApp {
	vapp := &govcd.VApp{VApp: &types.VApp{
		HREF:   fmt.Sprintf("https://vcd/vapp-%d", i),
		Name:   fmt.Sprintf("runner-%d", i),
		Status: 4,
	}}
	if !empty {
		vapp.VApp.Children = &types.VAppChildren{VM: []*types.Vm{{
			HREF: fmt.Sprintf("https://vcd/vm-%d", i), Name: fmt.Sprintf("vm-%d", i), Status: 4,
		}}}
	}
	return vapp
}

// blocker counts concurrent calls and holds them until released.
type blocker struct {
	inFlight, peak atomic.Int32
	release        chan struct{}
}

func newBlocker() *blocker { return &blocker{release: make(chan struct{})} }

func (b *blocker) enter() {
	n := b.inFlight.Add(1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-b.release
	b.inFlight.Add(-1)
}

func queueDeletes(r *vcdInstanceGroup, n int) {
	for i := 0; i < n; i++ {
		href := fmt.Sprintf("https://vcd/old-%d", i)
		r.store.AddPreexisting(href, fmt.Sprintf("old-%d", i), "", "", "", "", "")
		r.store.MarkForDeletion(href)
	}
}

func TestReconciler_CreatesAndDeletesShareOperationLimit(t *testing.T) {
	b := newBlocker()
	ops := &fakeOps{
		create: func() (*createResult, error) { b.enter(); return nil, errors.New("boom") },
		delete: func(string) error { b.enter(); return nil },
	}
	r := newFakeReconciler(t, ops, ReconcilerConfig{MaxConcurrentOperations: 4, MaxConcurrentCreates: 3, MaxConcurrentDeletes: 5})
	queueDeletes(r, 5)
	for i := 0; i < 5; i++ {
		r.store.AddCreateIntent(fmt.Sprintf("new-%d", i))
	}

	r.dispatchCreates()
	r.dispatchDeletes()
	require.Eventually(t, func() bool { return b.inFlight.Load() == 4 }, time.Second, 5*time.Millisecond)
	r.dispatchCreates()
	r.dispatchDeletes()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(4), b.peak.Load())

	close(b.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, r.limiter.drain(ctx))
}

func TestReconciler_DeleteBacklogLeavesSlotForCreates(t *testing.T) {
	b := newBlocker()
	created := make(chan struct{}, 1)
	ops := &fakeOps{
		create: func() (*createResult, error) { created <- struct{}{}; return nil, errors.New("boom") },
		delete: func(string) error { b.enter(); return nil },
	}
	r := newFakeReconciler(t, ops, ReconcilerConfig{MaxConcurrentOperations: 4})
	queueDeletes(r, 10)
	r.dispatchDeletes()
	require.Eventually(t, func() bool { return b.inFlight.Load() == 3 }, time.Second, 5*time.Millisecond)

	r.store.AddCreateIntent("new")
	r.dispatchCreates()
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("create did not start while deletes held their share")
	}
	close(b.release)
}

func TestReconciler_ThrottlePausePausesDispatch(t *testing.T) {
	ops := &fakeOps{
		create: func() (*createResult, error) { t.Error("create dispatched during pause"); return nil, errors.New("x") },
		delete: func(string) error { t.Error("delete dispatched during pause"); return nil },
	}
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	r.throttle.throttled("test")
	r.store.AddCreateIntent("a")
	queueDeletes(r, 1)

	r.dispatchCreates()
	r.dispatchDeletes()

	inst, _ := r.store.GetByIntentID("a")
	assert.Equal(t, PhasePendingCreate, inst.Phase)
	assert.Len(t, r.store.GetPendingDeletes(), 1)
}

func TestReconciler_CompletedOperationResetsThrottleBackoff(t *testing.T) {
	ops := &fakeOps{create: func() (*createResult, error) {
		return &createResult{VAppHREF: "https://vcd/vapp-1", VAppName: "runner-1"}, nil
	}}
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	r.throttle.bo = testGate(time.Minute).bo
	now := time.Now()
	r.throttle.now = func() time.Time { return now }
	r.throttle.throttled("a")
	now = now.Add(time.Minute)
	r.throttle.throttled("b") // escalated to 2m
	now = now.Add(2 * time.Minute)

	r.store.AddCreateIntent("a")
	r.doCreate("a")

	r.throttle.throttled("c")
	assert.Equal(t, now.Add(time.Minute), r.throttle.pausedUntil)
}

func TestReconciler_RecordsLastReconcileTime(t *testing.T) {
	r := newFakeReconciler(t, &fakeOps{}, ReconcilerConfig{})
	before := float64(time.Now().Unix())
	r.reconcileOnce(false)
	assert.GreaterOrEqual(t, gaugeValue(t, "fleeting_vcd_last_reconcile_timestamp_seconds", t.Name()), before)
}

// gaugeValue reads a gauge for one instance group from the default registry.
func gaugeValue(t *testing.T, name, group string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == instanceGroupLabel && label.GetValue() == group {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("gauge %s{%s=%q} not found", name, instanceGroupLabel, group)
	return 0
}

func TestConfig_MaxConcurrentOperationsDefault(t *testing.T) {
	g := &InstanceGroup{}
	require.NoError(t, g.populate())
	assert.Equal(t, defaultMaxConcurrentOperations, g.MaxConcurrentOperations)
}
