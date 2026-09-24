package vcd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

var permanentErrorPatterns = []string{
	"exceed the VDC's storage quota",
	"entity does not exist",
	"Unable to perform this action",
	"already exists",
}

func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pattern := range permanentErrorPatterns {
		if strings.Contains(msg, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// notFoundIsPermanent stops safeVCDCall retrying a read of an entity that no
// longer exists. Discovery re-lists every cycle, so a retry only delays it.
func notFoundIsPermanent[T any](fn func() (T, error)) func() (T, error) {
	return func() (T, error) {
		v, err := fn()
		if isEntityNotFoundError(err) {
			return v, backoff.Permanent(err)
		}
		return v, err
	}
}

const (
	safeCallMaxElapsedTime  = 5 * time.Minute
	safeCallInitialInterval = time.Second
)

func vcdCallAttempt[T any](ctx context.Context, g *InstanceGroup, desc string, fn func() (T, error), restore []func()) (result T, generation uint64, err error) {
	generation, err = g.lockSession(ctx)
	if err != nil {
		return result, generation, err
	}
	defer g.clientMu.RUnlock()
	defer func() {
		if rec := recover(); rec != nil {
			g.log.Error("panic recovered in VCD call", "operation", desc, "panic", rec)
			err = fmt.Errorf("panic in %s: %v", desc, rec)
		}
		if err != nil {
			for _, rollback := range restore {
				rollback()
			}
		}
	}()
	result, err = fn()
	return result, generation, err
}

// safeVCDCall retries reads and repeatable operations. Authentication errors
// renew the shared client before retrying the same object.
func safeVCDCall[T any](ctx context.Context, g *InstanceGroup, desc string, fn func() (T, error), restore ...func()) (T, error) {
	return vcdCall(ctx, g, desc, fn, true, restore)
}

// singleVCDCall never replays a mutation, including when an SDK method reports
// a 401 from a read AFTER the mutation succeeded. Renew for subsequent recovery.
func singleVCDCall[T any](ctx context.Context, g *InstanceGroup, desc string, fn func() (T, error), restore ...func()) (T, error) {
	return vcdCall(ctx, g, desc, fn, false, restore)
}

func vcdCall[T any](ctx context.Context, g *InstanceGroup, desc string, fn func() (T, error), retry bool, restore []func()) (result T, err error) {
	startTime := time.Now()
	defer func() {
		APICallsTotal.WithLabelValues(g.InstanceGroupName, desc).Inc()
		APIDuration.WithLabelValues(g.InstanceGroupName, desc).Observe(time.Since(startTime).Seconds())
		if err != nil {
			APIErrorsTotal.WithLabelValues(g.InstanceGroupName, desc).Inc()
			g.log.Error("VCD call failed", "operation", desc, "error", err)
		}
	}()
	op := func() (T, error) {
		val, generation, opErr := vcdCallAttempt(ctx, g, desc, fn, restore)
		// Rejected before any task started, so waiting and replaying is safe
		// even for calls that must never replay a mutation.
		for g.throttle != nil && isOperationLimitError(opErr) {
			g.throttle.throttled(desc)
			if waitErr := g.throttle.wait(ctx); waitErr != nil {
				return val, backoff.Permanent(fmt.Errorf("%w; waiting for vCD operation limit: %w", opErr, waitErr))
			}
			val, generation, opErr = vcdCallAttempt(ctx, g, desc, fn, restore)
		}
		if isUnauthorizedError(opErr) {
			if authErr := g.refreshSession(ctx, generation); authErr != nil {
				return val, fmt.Errorf("%w; session renewal failed: %v", opErr, authErr)
			}
			if retry {
				val, _, opErr = vcdCallAttempt(ctx, g, desc, fn, restore)
			}
		}
		if isPermanentError(opErr) {
			return val, backoff.Permanent(opErr)
		}
		return val, opErr
	}
	if !retry {
		return op()
	}
	bo := backoff.NewExponentialBackOff(
		backoff.WithMaxElapsedTime(safeCallMaxElapsedTime),
		backoff.WithInitialInterval(safeCallInitialInterval),
	)
	return backoff.RetryWithData(op, backoff.WithContext(bo, ctx))
}

func safeVCDCallVoid(ctx context.Context, g *InstanceGroup, desc string, fn func() error, restore ...func()) error {
	_, err := safeVCDCall(ctx, g, desc, func() (struct{}, error) {
		return struct{}{}, fn()
	}, restore...)
	return err
}

// SDK Refresh methods replace the payload before issuing the request. Restore
// it on failure so retries retain the HREF, including Refresh calls inside
// higher-level SDK methods such as GetStatus and ChangeMemory.
func preserveVM(vm *govcd.VM) func() {
	previous := vm.VM
	return func() { vm.VM = previous }
}

func preserveVApp(vapp *govcd.VApp) func() {
	previous := vapp.VApp
	return func() { vapp.VApp = previous }
}
