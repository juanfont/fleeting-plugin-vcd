package vcd

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForSSHReadiness_HonoursContext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close()) // nothing listens: every dial is refused

	g := &InstanceGroup{log: testLogger()}
	g.settings.Password = "unused"
	g.settings.ProtocolPort = port

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = g.waitForSSHReadiness(ctx, "127.0.0.1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestSleepContext(t *testing.T) {
	require.NoError(t, sleepContext(context.Background(), time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)
}
