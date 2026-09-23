package vcd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

type sessionFixture struct {
	g            *InstanceGroup
	server       *httptest.Server
	exchanges    atomic.Int32
	unauthorized atomic.Int32
	failAuth     atomic.Bool
	validToken   atomic.Value
	handle       func(http.ResponseWriter, *http.Request) bool
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{}
	f.validToken.Store("")
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/vnd.vmware.vcloud.vApp+xml")
		switch r.URL.Path {
		case "/oauth/tenant/org/token":
			if f.failAuth.Load() {
				http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
				return
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "api-token" {
				http.Error(w, "invalid token exchange", http.StatusBadRequest)
				return
			}
			token := fmt.Sprintf("bearer-token-longer-than-32-characters-%d", f.exchanges.Add(1))
			f.validToken.Store(token)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 86400})
			return
		case "/api/versions":
			fmt.Fprintf(w, `<SupportedVersions><VersionInfo><Version>38.1</Version><LoginUrl>%s/api/sessions</LoginUrl></VersionInfo></SupportedVersions>`, base)
			return
		}
		if r.Header.Get("Authorization") != "bearer "+f.validToken.Load().(string) {
			f.unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<Error majorErrorCode="401" message="Unauthorized"/>`)
			return
		}
		if f.handle != nil && f.handle(w, r) {
			return
		}
		switch r.URL.Path {
		case "/api/org":
			fmt.Fprintf(w, `<OrgList><Org name="org" href="%s/api/org/1"/></OrgList>`, base)
		case "/api/org/1":
			fmt.Fprintf(w, `<Org name="org" href="%s/api/org/1"><Link type="application/vnd.vmware.vcloud.vdc+xml" name="vdc" href="%s/api/vdc/1"/></Org>`, base, base)
		case "/api/query":
			fmt.Fprintf(w, `<QueryResultRecords total="1" page="1" pageSize="128"><OrgVdcRecord name="vdc" href="%s/api/vdc/1"/></QueryResultRecords>`, base)
		case "/api/vdc/1":
			fmt.Fprintf(w, `<Vdc name="vdc" href="%s/api/vdc/1"><ResourceEntities><ResourceEntity name="runner-test" type="application/vnd.vmware.vcloud.vApp+xml" href="%s/api/vApp/1"/></ResourceEntities></Vdc>`, base, base)
		case "/api/vApp/1":
			fmt.Fprintf(w, `<VApp name="runner-test" href="%s/api/vApp/1" status="8"/>`, base)
		case "/api/vm/1":
			fmt.Fprintf(w, `<Vm name="vm" href="%s/api/vm/1" status="8"/>`, base)
		case "/api/task/1":
			fmt.Fprintf(w, `<Task href="%s/api/task/1" status="success"/>`, base)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	f.g = &InstanceGroup{StrURL: f.server.URL + "/api", Org: "org", Token: "api-token", VirtualDatacenter: "vdc", log: testLogger(), InstanceGroupName: "session-test"}
	require.NoError(t, f.g.populate())
	_, err := f.g.getVCDClient()
	require.NoError(t, err)
	return f
}

func TestSession_401RenewsExistingObjects(t *testing.T) {
	f := newSessionFixture(t)
	client, err := f.g.getVCDClient()
	require.NoError(t, err)
	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = f.server.URL + "/api/vm/1"
	vapp := govcd.NewVApp(&client.Client)
	vapp.VApp.HREF = f.server.URL + "/api/vApp/1"
	task := govcd.NewTask(&client.Client)
	task.Task.HREF = f.server.URL + "/api/task/1"
	f.validToken.Store("expired")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, safeVCDCallVoid(ctx, f.g, "Refresh VM", vm.Refresh, preserveVM(vm)))
	f.validToken.Store("expired again")
	_, err = safeVCDCall(ctx, f.g, "Refresh vApp", func() (string, error) {
		err := vapp.Refresh()
		return vapp.VApp.Name, err
	}, preserveVApp(vapp))
	require.NoError(t, err)
	require.NoError(t, f.g.waitTaskCompletion(ctx, task))
	current, err := f.g.getVCDClient()
	require.NoError(t, err)
	require.Same(t, client, current)
	require.Equal(t, int32(3), f.exchanges.Load())
	require.Equal(t, int32(2), f.unauthorized.Load())
}

func TestSession_RefreshBeforeExpiry(t *testing.T) {
	f := newSessionFixture(t)
	f.g.SessionRefreshInterval = "1s"
	require.NoError(t, f.g.populate())
	client := f.g.vcdClient
	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = f.server.URL + "/api/vm/1"
	f.g.lastAuthenticated = time.Now().Add(-2 * time.Second)
	require.NoError(t, safeVCDCallVoid(context.Background(), f.g, "Refresh VM", vm.Refresh, preserveVM(vm)))
	require.Same(t, client, f.g.vcdClient)
	require.Equal(t, int32(2), f.exchanges.Load())
	require.Zero(t, f.unauthorized.Load())
}

func TestSession_ConcurrentUnauthorizedRenewsOnce(t *testing.T) {
	f := newSessionFixture(t)
	const workers = 8
	var ready sync.WaitGroup
	ready.Add(workers)
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			first := true
			results <- safeVCDCallVoid(context.Background(), f.g, "concurrent refresh", func() error {
				if first {
					first = false
					ready.Done()
					ready.Wait()
					return fmt.Errorf("error refreshing VM: 401 Unauthorized")
				}
				vm := govcd.NewVM(&f.g.vcdClient.Client)
				vm.VM.HREF = f.server.URL + "/api/vm/1"
				return vm.Refresh()
			})
		}()
	}
	for i := 0; i < workers; i++ {
		require.NoError(t, <-results)
	}
	require.Equal(t, int32(2), f.exchanges.Load())
}

func TestSession_ExpiryDuringTaskPolling(t *testing.T) {
	f := newSessionFixture(t)
	var polls atomic.Int32
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/task/1" {
			return false
		}
		status := "success"
		if polls.Add(1) == 1 {
			status = "running"
			f.validToken.Store("expired")
		}
		fmt.Fprintf(w, `<Task href="%s/api/task/1" status="%s"/>`, f.server.URL, status)
		return true
	}
	task := govcd.NewTask(&f.g.vcdClient.Client)
	task.Task.HREF = f.server.URL + "/api/task/1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, f.g.waitTaskCompletion(ctx, task))
	require.Equal(t, int32(2), polls.Load())
	require.Equal(t, int32(2), f.exchanges.Load())
}

func TestSession_Config(t *testing.T) {
	for _, interval := range []string{"-1h", "0s", "24h", "25h", "invalid"} {
		t.Run(interval, func(t *testing.T) {
			g := &InstanceGroup{SessionRefreshInterval: interval}
			require.Error(t, g.populate())
		})
	}
	g := &InstanceGroup{}
	require.NoError(t, g.populate())
	require.Equal(t, 20*time.Hour, g.sessionRefreshInterval)
}

func TestSession_RenewalFailureCanRecover(t *testing.T) {
	f := newSessionFixture(t)
	client := f.g.vcdClient
	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = f.server.URL + "/api/vm/1"
	f.validToken.Store("expired")
	f.failAuth.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, safeVCDCallVoid(ctx, f.g, "Refresh VM", vm.Refresh, preserveVM(vm)))
	require.Equal(t, f.server.URL+"/api/vm/1", vm.VM.HREF)
	f.failAuth.Store(false)
	require.NoError(t, safeVCDCallVoid(context.Background(), f.g, "Refresh VM", vm.Refresh, preserveVM(vm)))
	require.Same(t, client, f.g.vcdClient)
}

func TestSession_ProactiveRenewalDuringTask(t *testing.T) {
	f := newSessionFixture(t)
	f.g.sessionRefreshInterval = 2 * time.Second
	var polls atomic.Int32
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/task/1" {
			return false
		}
		status := "success"
		if polls.Add(1) == 1 {
			status = "running"
		}
		fmt.Fprintf(w, `<Task href="%s/api/task/1" status="%s"/>`, f.server.URL, status)
		return true
	}
	task := govcd.NewTask(&f.g.vcdClient.Client)
	task.Task.HREF = f.server.URL + "/api/task/1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, f.g.waitTaskCompletion(ctx, task))
	require.Equal(t, int32(2), f.exchanges.Load())
	require.Zero(t, f.unauthorized.Load())
}

func TestSession_UnauthorizedDetection(t *testing.T) {
	for _, message := range []string{"error retrieving task: 401 Unauthorized", "API Error: 401: Unauthorized", "[401:UNAUTHORIZED]"} {
		require.True(t, isUnauthorizedError(fmt.Errorf("%s", message)))
	}
	require.False(t, isUnauthorizedError(nil))
	require.False(t, isUnauthorizedError(fmt.Errorf("403 Forbidden")))
	require.False(t, isUnauthorizedError(fmt.Errorf("timeout after 1401 seconds")))
	require.False(t, isUnauthorizedError(fmt.Errorf("GET /api/vm/401: 403 Forbidden")))
}
