package vcd

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/vmware/go-vcloud-director/v3/govcd"
)

const defaultSessionRefreshInterval = 20 * time.Hour

// The SDK wraps HTTP errors as strings, sometimes discarding the original type.
var unauthorizedStatus = regexp.MustCompile(`(?i)(\b401\s+Unauthorized\b|\bAPI Error:\s*401\b|\[401:|\bstatus(?: code)?[=: ]+401\b)`)

func isUnauthorizedError(err error) bool {
	return err != nil && unauthorizedStatus.MatchString(err.Error())
}

func setVCDToken(client *govcd.VCDClient, org, token string) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic in SetToken: %v", rec)
		}
	}()
	if err := client.SetToken(org, govcd.ApiTokenHeader, token); err != nil {
		return fmt.Errorf("unable to authenticate to Org %q: %w", org, err)
	}
	return nil
}

// All SDK calls hold clientMu.RLock; SetToken mutates fields that the SDK reads
// without synchronization. Keep the client (and its embedded Client) in place.
func (g *InstanceGroup) authenticateLocked() error {
	if err := setVCDToken(g.vcdClient, g.Org, g.Token); err != nil {
		g.lastAuthenticated = time.Time{}
		return err
	}
	g.lastAuthenticated = time.Now()
	g.authGeneration++
	g.log.Info("renewed VCD session")
	return nil
}

func (g *InstanceGroup) sessionExpired() bool {
	interval := g.sessionRefreshInterval
	if interval <= 0 {
		interval = defaultSessionRefreshInterval
	}
	return time.Since(g.lastAuthenticated) >= interval
}

// lockSession returns with a read lock held, renewing first if needed. The
// reconciler calls this even when idle, so renewal does not depend on scaling.
func (g *InstanceGroup) lockSession(ctx context.Context) (uint64, error) {
	g.clientMu.RLock()
	if err := ctx.Err(); err != nil {
		g.clientMu.RUnlock()
		return 0, err
	}
	if g.vcdClient == nil || !g.sessionExpired() {
		return g.authGeneration, nil
	}
	g.clientMu.RUnlock()
	g.clientMu.Lock()
	var err error
	if g.sessionExpired() {
		err = g.authenticateLocked()
	}
	g.clientMu.Unlock()
	if err != nil {
		return 0, err
	}
	g.clientMu.RLock()
	return g.authGeneration, nil
}

func (g *InstanceGroup) refreshSession(ctx context.Context, generation uint64) error {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.vcdClient == nil {
		return fmt.Errorf("cannot renew VCD session before authentication")
	}
	// Concurrent 401s from one session must trigger only one token exchange.
	if g.authGeneration != generation {
		return nil
	}
	return g.authenticateLocked()
}
