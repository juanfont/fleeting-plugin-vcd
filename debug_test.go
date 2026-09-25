package vcd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

func newTestDebugServer(t *testing.T, status DebugStatus) (*httptest.Server, *desiredStateStore) {
	t.Helper()
	store := newDesiredStateStore(testLogger(), t.Name())
	ds := NewDebugServer(testLogger(), store, "group-a", func() DebugStatus { return status })
	srv := httptest.NewServer(ds)
	t.Cleanup(srv.Close)
	return srv, store
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body), resp.Header
}

func TestDebug_PageLoadsFragmentsWithHtmx(t *testing.T) {
	srv, _ := newTestDebugServer(t, DebugStatus{})
	code, body, _ := get(t, srv.URL+"/")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `src="static/htmx.min.js"`)
	assert.Contains(t, body, `hx-get="fragments/summary"`)
	assert.Contains(t, body, `hx-get="fragments/instances"`)
	assert.NotContains(t, body, `http-equiv="refresh"`, "no full-page reloads")
}

func TestDebug_ServesEmbeddedHtmx(t *testing.T) {
	srv, _ := newTestDebugServer(t, DebugStatus{})
	code, body, hdr := get(t, srv.URL+"/static/htmx.min.js")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, hdr.Get("Content-Type"), "javascript")
	assert.True(t, strings.HasPrefix(body, "var htmx="))
}

func TestDebug_InstancesFragmentFilters(t *testing.T) {
	srv, store := newTestDebugServer(t, DebugStatus{})
	store.AddLeftover("https://vcd/vapp-old", "runner-old")
	store.AddCreateIntent("new")
	store.UpdateInstance("new", func(i *Instance) {
		i.Phase = PhaseRunning
		i.ID = "https://vcd/vapp-new"
		i.Name = "runner-new"
		i.IPAddress = "10.0.0.7"
	})
	store.AddCreateIntent("gone")
	store.UpdateInstance("gone", func(i *Instance) { i.Phase = PhaseDeleted; i.Name = "runner-gone" })

	_, body, _ := get(t, srv.URL+"/fragments/instances")
	assert.Contains(t, body, "runner-old")
	assert.Contains(t, body, "leftover")
	assert.Contains(t, body, "10.0.0.7")
	assert.Contains(t, body, "runner-gone")
	assert.NotContains(t, body, "<html", "fragments are partial")

	_, body, _ = get(t, srv.URL+"/fragments/instances?hide_deleted=on")
	assert.NotContains(t, body, "runner-gone")

	_, body, _ = get(t, srv.URL+"/fragments/instances?phase=Running")
	assert.Contains(t, body, "runner-new")
	assert.NotContains(t, body, "runner-old")
}

func TestDebug_SummaryShowsStartupHold(t *testing.T) {
	srv, _ := newTestDebugServer(t, DebugStatus{
		StartupHold: true, StartupPolled: true, LeftoversRemaining: 12, LeftoversDeleting: 3,
		HoldStartedAt: time.Now().Add(-4 * time.Minute), HoldTimeoutSeconds: 1800,
		LastReconcileAt: time.Now(),
	})
	_, body, _ := get(t, srv.URL+"/fragments/summary")
	assert.Contains(t, body, "Startup cleanup")
	assert.Contains(t, body, "12 leftovers remaining")
	assert.Contains(t, body, "3 deleting")
}

func TestDebug_APIStatus(t *testing.T) {
	srv, store := newTestDebugServer(t, DebugStatus{StartupHold: true, LeftoversRemaining: 2})
	store.AddLeftover("https://vcd/vapp-old", "runner-old")
	code, body, hdr := get(t, srv.URL+"/api/status")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, hdr.Get("Content-Type"), "application/json")
	var out struct {
		InstanceGroup string         `json:"instance_group"`
		Status        DebugStatus    `json:"status"`
		Phases        map[string]int `json:"phases"`
		Instances     []struct {
			Name     string `json:"name"`
			Phase    string `json:"phase"`
			Leftover bool   `json:"leftover"`
		} `json:"instances"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, "group-a", out.InstanceGroup)
	assert.True(t, out.Status.StartupHold)
	assert.Equal(t, 1, out.Phases["PendingDelete"])
	require.Len(t, out.Instances, 1)
	assert.Equal(t, "runner-old", out.Instances[0].Name)
	assert.True(t, out.Instances[0].Leftover)
}

func TestDebugStatus_PublishedEachCycle(t *testing.T) {
	ops := &fakeOps{delete: func(string) error { select {} }}
	ops.setVApps([]*govcd.VApp{testVApp(1, false), testVApp(2, false)})
	r := newHeldReconciler(t, ops, ReconcilerConfig{})

	assert.True(t, r.DebugStatus().LastReconcileAt.IsZero())
	r.reconcileOnce(false)
	st := r.DebugStatus()
	assert.False(t, st.LastReconcileAt.IsZero())
	assert.True(t, st.StartupHold)
	assert.True(t, st.StartupPolled)
	assert.Equal(t, 2, st.LeftoversRemaining)
	assert.Equal(t, 4, st.OpsLimit)
	require.Eventually(t, func() bool { r.reconcileOnce(false); return r.DebugStatus().DeletesInFlight == 2 }, time.Second, 5*time.Millisecond)
}

func TestDebug_UnnamedIntentsKeepStableOrder(t *testing.T) {
	srv, store := newTestDebugServer(t, DebugStatus{})
	for _, id := range []string{"c", "a", "d", "b"} {
		store.AddCreateIntent("intent-" + id)
	}
	_, first, _ := get(t, srv.URL+"/fragments/instances")
	for i := 0; i < 20; i++ {
		_, again, _ := get(t, srv.URL+"/fragments/instances")
		require.Equal(t, first, again)
	}
	assert.Less(t, strings.Index(first, "intent-a"), strings.Index(first, "intent-d"))
}
