package vcd

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
)

func TestCreateRawVApp_AdoptsAfterAmbiguousResponse(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newSessionFixture(t)
			var posts atomic.Int32
			f.handle = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/vdc/1/action/composeVApp" {
					return false
				}
				posts.Add(1)
				w.WriteHeader(status)
				message := "The VMware Cloud Director entity runner-test already exists"
				if status == http.StatusInternalServerError {
					message = "request failed after commit"
				}
				fmt.Fprintf(w, `<Error majorErrorCode="%d" message="%s"/>`, status, message)
				return true
			}
			vdc := govcd.NewVdc(&f.g.vcdClient.Client)
			vdc.Vdc.HREF = f.server.URL + "/api/vdc/1"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			vapp, err := f.g.createRawVApp(ctx, f.g.vcdClient, vdc, "runner-test", "runner")
			require.NoError(t, err)
			require.Equal(t, "runner-test", vapp.VApp.Name)
			require.Equal(t, int32(1), posts.Load())
		})
	}
}

func TestCreateRawVApp_Task401DoesNotRepeatPOST(t *testing.T) {
	f := newSessionFixture(t)
	var posts atomic.Int32
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/vdc/1/action/composeVApp" {
			return false
		}
		posts.Add(1)
		var params types.ComposeVAppParams
		if err := xml.NewDecoder(r.Body).Decode(&params); err != nil || params.Name != "runner-test" || params.Deploy || params.PowerOn {
			http.Error(w, "invalid compose request", http.StatusBadRequest)
			return true
		}
		fmt.Fprintf(w, `<VApp name="runner-test" href="%s/api/vApp/1"><Tasks><Task href="%s/api/task/1" status="running"/></Tasks></VApp>`, f.server.URL, f.server.URL)
		f.validToken.Store("expired")
		return true
	}
	vdc := govcd.NewVdc(&f.g.vcdClient.Client)
	vdc.Vdc.HREF = f.server.URL + "/api/vdc/1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vapp, err := f.g.createRawVApp(ctx, f.g.vcdClient, vdc, "runner-test", "runner")
	require.NoError(t, err)
	require.NotNil(t, vapp)
	require.Equal(t, int32(1), posts.Load())
	require.Equal(t, int32(2), f.exchanges.Load())
}

func TestSingleVCDCall_DoesNotReplayMutation(t *testing.T) {
	for _, message := range []string{"already exists", "connection reset by peer", "401 Unauthorized"} {
		t.Run(message, func(t *testing.T) {
			f := newSessionFixture(t)
			calls := 0
			_, err := singleVCDCall(context.Background(), f.g, "AddNewVM", func() (int, error) {
				calls++
				return 0, fmt.Errorf("%s", message)
			})
			require.Error(t, err)
			require.Equal(t, 1, calls)
		})
	}
}

func TestSafeVCDCall_AlreadyExistsIsPermanent(t *testing.T) {
	g := &InstanceGroup{log: testLogger(), InstanceGroupName: "create-test"}
	calls := 0
	_, err := safeVCDCall(context.Background(), g, "create", func() (int, error) {
		calls++
		return 0, fmt.Errorf("API Error: 400: entity runner-test already exists")
	})
	require.Error(t, err)
	require.Equal(t, 1, calls)
}

func TestDeleteInstance_TaskExpiryCompletesLifecycle(t *testing.T) {
	f := newSessionFixture(t)
	var deletes atomic.Int32
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodDelete {
			return false
		}
		deletes.Add(1)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `<Task href="%s/api/task/1" status="running"/>`, f.server.URL)
		f.validToken.Store("expired")
		return true
	}
	store := newDesiredStateStore(testLogger(), "test-delete")
	store.AddCreateIntent("delete-intent")
	store.UpdateInstance("delete-intent", func(inst *Instance) {
		inst.ID = f.server.URL + "/api/vApp/1"
		inst.Phase = PhaseDeleting
	})
	r := newVCDInstanceGroup(testLogger(), store, f.g, ReconcilerConfig{})
	r.doDelete("delete-intent", f.server.URL+"/api/vApp/1")
	inst, found := store.GetByIntentID("delete-intent")
	require.True(t, found)
	require.Equal(t, PhaseDeleted, inst.Phase)
	require.Zero(t, inst.RetryCount)
	require.Equal(t, int32(1), deletes.Load())
	require.Equal(t, int32(2), f.exchanges.Load())
}

func TestFailedCreateCleanup_RetainedUntilDeleted(t *testing.T) {
	f := newSessionFixture(t)
	var failDelete atomic.Bool
	failDelete.Store(true)
	var deletes atomic.Int32
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodDelete {
			return false
		}
		deletes.Add(1)
		if failDelete.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error majorErrorCode="403" message="Unable to perform this action"/>`)
		} else {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `<Task href="%s/api/task/1" status="running"/>`, f.server.URL)
		}
		return true
	}
	f.g.failedCreateCleanups.Store("runner-test", time.Now())
	r := newVCDInstanceGroup(testLogger(), newDesiredStateStore(testLogger(), "test-cleanup"), f.g, ReconcilerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitForCleanup := func() {
		t.Helper()
		require.NoError(t, r.limiter.total.Acquire(ctx, r.limiter.size))
		r.limiter.total.Release(r.limiter.size)
	}
	r.dispatchCreateCleanups()
	waitForCleanup()
	_, pending := f.g.failedCreateCleanups.Load("runner-test")
	require.True(t, pending)
	failDelete.Store(false)
	r.dispatchCreateCleanups()
	waitForCleanup()
	_, pending = f.g.failedCreateCleanups.Load("runner-test")
	require.False(t, pending)
	require.Equal(t, int32(2), deletes.Load())
}
