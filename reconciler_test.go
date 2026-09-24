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
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

type fakeOps struct {
	mu      sync.Mutex
	vApps   []*govcd.VApp
	polled  []string
	deleted []string
	create  func() (*createResult, error)
	delete  func(href string) error
	owned   map[string]bool

	listErr      error
	pollVMBlocks bool
}

func (f *fakeOps) getInstancesInInstanceGroup(ctx context.Context) ([]*govcd.VApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]*govcd.VApp(nil), f.vApps...), nil
}

func (f *fakeOps) setVApps(vApps []*govcd.VApp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vApps = vApps
}

func (f *fakeOps) pollVM(ctx context.Context, vmHREF string) (*vmPollResult, error) {
	f.mu.Lock()
	f.polled = append(f.polled, vmHREF)
	blocks := f.pollVMBlocks
	f.mu.Unlock()
	if blocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
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

func (f *fakeOps) ownsVApp(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owned[name]
}

func (f *fakeOps) createRecorded(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.owned, name)
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

func TestPoll_QueuesLeftoversThroughLimiter(t *testing.T) {
	b := newBlocker()
	ops := &fakeOps{delete: func(string) error { b.enter(); return nil }}
	var vApps []*govcd.VApp
	for i := 0; i < 50; i++ {
		vApps = append(vApps, testVApp(i, i%10 == 0))
	}
	ops.setVApps(vApps)
	r := newFakeReconciler(t, ops, ReconcilerConfig{MaxConcurrentOperations: 4})

	r.pollVCD()
	assert.Zero(t, ops.polledCount(), "leftovers must not cost a VM read")
	assert.Len(t, r.store.GetPendingDeletes(), 50)
	assert.Zero(t, b.inFlight.Load(), "polling must not start deletes")

	r.dispatchDeletes()
	require.Eventually(t, func() bool { return b.inFlight.Load() == 3 }, time.Second, 5*time.Millisecond)
	r.pollVCD()
	r.dispatchDeletes()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(3), b.peak.Load())
	close(b.release)
}

func TestPoll_DoesNotQueueVAppOfCreateInFlight(t *testing.T) {
	ops := &fakeOps{owned: map[string]bool{"runner-1": true, "runner-2": true}}
	ops.setVApps([]*govcd.VApp{testVApp(1, true), testVApp(2, false)})
	r := newFakeReconciler(t, ops, ReconcilerConfig{})

	r.pollVCD()
	assert.Empty(t, r.store.GetAll())
}

func TestCreate_RecordsHREFBeforeReleasingOwnership(t *testing.T) {
	ops := &fakeOps{owned: map[string]bool{"runner-1": true}}
	ops.create = func() (*createResult, error) {
		return &createResult{VAppHREF: "https://vcd/vapp-1", VAppName: "runner-1", VMName: "vm-1"}, nil
	}
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	r.store.AddCreateIntent("intent")

	r.doCreate("intent")
	assert.False(t, ops.ownsVApp("runner-1"))

	ops.setVApps([]*govcd.VApp{testVApp(1, false)})
	r.pollVCD()
	inst, ok := r.store.GetByVAppHREF("https://vcd/vapp-1")
	require.True(t, ok)
	assert.Equal(t, "intent", inst.IntentID)
	assert.False(t, inst.Leftover)
	assert.Len(t, r.store.GetAll(), 1)
}

func TestPoll_LeftoverRemovedElsewhereIsMarkedDeleted(t *testing.T) {
	ops := &fakeOps{}
	ops.setVApps([]*govcd.VApp{testVApp(1, false), testVApp(2, false)})
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	r.pollVCD()

	ops.setVApps([]*govcd.VApp{testVApp(2, false)})
	for i := 0; i < missedPollsBeforeDisappeared; i++ {
		r.pollVCD()
	}
	inst, _ := r.store.GetByVAppHREF("https://vcd/vapp-1")
	assert.Equal(t, PhaseDeleted, inst.Phase)
}

func TestUpdate_HidesLeftovers(t *testing.T) {
	ops := &fakeOps{}
	ops.setVApps([]*govcd.VApp{testVApp(1, false)})
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	g := &InstanceGroup{log: testLogger(), InstanceGroupName: t.Name(), ig: r}
	r.pollVCD()

	var reported []string
	require.NoError(t, g.Update(context.Background(), func(id string, _ provider.State) {
		reported = append(reported, id)
	}))
	assert.Empty(t, reported)
}

func TestInstanceGroup_OwnsVApp(t *testing.T) {
	g := &InstanceGroup{}
	assert.False(t, g.ownsVApp("runner-1"))

	g.inflightCreates.Store("runner-1", struct{}{})
	assert.True(t, g.ownsVApp("runner-1"))
	g.createRecorded("runner-1")
	assert.False(t, g.ownsVApp("runner-1"))

	g.failedCreateCleanups.Store("runner-2", time.Now())
	assert.True(t, g.ownsVApp("runner-2"))
}

func TestShutdown_ReturnsWhenContextEndsDuringDelete(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	ops := &fakeOps{delete: func(string) error { <-block; return nil }}
	log := testLogger()
	g := &InstanceGroup{log: log, InstanceGroupName: t.Name(), VAppNamePrefix: "runner-"}
	r := newVCDInstanceGroup(log, newDesiredStateStore(log, t.Name()), g, ReconcilerConfig{})
	r.ops = ops
	r.store.AddLeftover("https://vcd/vapp-1", "runner-1")
	r.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Shutdown(ctx) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after its context ended")
	}
}

// trackRunning stores a Running instance for testVApp(i, ...).
func trackRunning(r *vcdInstanceGroup, i int) {
	intent := fmt.Sprintf("intent-%d", i)
	r.store.AddCreateIntent(intent)
	r.store.UpdateInstance(intent, func(inst *Instance) {
		inst.Phase = PhaseRunning
		inst.ID = fmt.Sprintf("https://vcd/vapp-%d", i)
		inst.Name = fmt.Sprintf("runner-%d", i)
	})
}

func TestPoll_SlowVMReadDoesNotBlockDispatch(t *testing.T) {
	ops := &fakeOps{pollVMBlocks: true}
	r := newFakeReconciler(t, ops, ReconcilerConfig{PollReadTimeout: 20 * time.Millisecond})
	for i := 1; i <= 3; i++ {
		trackRunning(r, i)
	}
	ops.setVApps([]*govcd.VApp{testVApp(1, false), testVApp(2, false), testVApp(3, false)})

	start := time.Now()
	require.NoError(t, r.pollVCD())
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 3, ops.polledCount())
}

func TestPoll_ListFailureIsReportedAndKeepsState(t *testing.T) {
	ops := &fakeOps{listErr: errors.New("search failed")}
	r := newFakeReconciler(t, ops, ReconcilerConfig{})
	trackRunning(r, 1)

	for i := 0; i < missedPollsBeforeDisappeared+1; i++ {
		require.Error(t, r.pollVCD())
	}
	inst, _ := r.store.GetByVAppHREF("https://vcd/vapp-1")
	assert.Equal(t, PhaseRunning, inst.Phase)
	assert.Zero(t, inst.MissedPolls)
}
