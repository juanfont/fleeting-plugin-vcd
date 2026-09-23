package vcd

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

// Send an invalid bearer on one task read to force a real vCD 401 without
// revoking the API token or affecting other runners' sessions.
type expiringTaskTransport struct {
	base     http.RoundTripper
	armed    atomic.Bool
	failures atomic.Int32
}

func (tr *expiringTaskTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/task/") && tr.armed.CompareAndSwap(true, false) {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "bearer deliberately-expired-integration-test-token")
		req.Header.Set(govcd.BearerTokenHeader, "deliberately-expired-integration-test-token")
		resp, err := tr.base.RoundTrip(req)
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			tr.failures.Add(1)
		}
		return resp, err
	}
	return tr.base.RoundTrip(req)
}

func TestInstanceGroup_SessionRenewal(t *testing.T) {
	skipIfNoVCDEnv(t)
	g, ig := newTestInstanceGroup(t)
	client, err := g.getVCDClient()
	require.NoError(t, err)
	g.clientMu.Lock()
	transport := &expiringTaskTransport{base: client.Client.Http.Transport}
	client.Client.Http.Transport = transport
	g.sessionRefreshInterval = 30 * time.Second
	g.clientMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()
	transport.armed.Store(true)
	require.Len(t, ig.Increase(1), 1)
	running := waitForPhase(t, ctx, ig, PhaseRunning, 1)
	require.Equal(t, int32(1), transport.failures.Load(), "create must recover from a real task-poll 401")

	transport.armed.Store(true)
	ig.Decrease([]string{running[0].ID})
	waitForNonDeletedCount(t, ctx, ig, 0)
	require.Equal(t, int32(2), transport.failures.Load(), "delete must recover from a real task-poll 401")
	current, err := g.getVCDClient()
	require.NoError(t, err)
	require.Same(t, client, current)
	g.clientMu.RLock()
	generation := g.authGeneration
	g.clientMu.RUnlock()
	require.GreaterOrEqual(t, generation, uint64(3))
}
