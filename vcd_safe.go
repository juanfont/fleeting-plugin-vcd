package vcd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/go-hclog"
)

// permanentErrorPatterns are VCD error messages that should not be retried.
// These indicate resource limits or configuration issues that won't resolve on retry.
var permanentErrorPatterns = []string{
	"exceed the VDC's storage quota",
	"entity does not exist",
	"Unable to perform this action",
}

func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range permanentErrorPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

const (
	safeCallMaxElapsedTime  = 5 * time.Minute
	safeCallInitialInterval = 1 * time.Second
)

// safeVCDCall wraps any VCD API call with panic recovery + exponential backoff retry.
// The go-vcloud-director library is known to panic randomly, and the VCD API is slow
// and unreliable. This wrapper ensures every call is recoverable.
// The context controls cancellation and timeout — when the context expires, retries stop.
func safeVCDCall[T any](ctx context.Context, log hclog.Logger, instanceGroup string, desc string, fn func() (T, error)) (result T, err error) {
	startTime := time.Now()

	op := func() (T, error) {
		var val T
		var opErr error

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("panic recovered in VCD call", "operation", desc, "panic", r)
					opErr = fmt.Errorf("panic in %s: %v", desc, r)
				}
			}()
			val, opErr = fn()
		}()

		if isPermanentError(opErr) {
			return val, backoff.Permanent(opErr)
		}
		return val, opErr
	}

	bo := backoff.NewExponentialBackOff(
		backoff.WithMaxElapsedTime(safeCallMaxElapsedTime),
		backoff.WithInitialInterval(safeCallInitialInterval),
	)

	result, err = backoff.RetryWithData(op, backoff.WithContext(bo, ctx))

	duration := time.Since(startTime).Seconds()
	APICallsTotal.WithLabelValues(instanceGroup, desc).Inc()
	APIDuration.WithLabelValues(instanceGroup, desc).Observe(duration)
	if err != nil {
		APIErrorsTotal.WithLabelValues(instanceGroup, desc).Inc()
		log.Error("VCD call failed after retries", "operation", desc, "error", err)
	}

	return result, err
}

// safeVCDCallVoid is safeVCDCall for void operations.
func safeVCDCallVoid(ctx context.Context, log hclog.Logger, instanceGroup string, desc string, fn func() error) error {
	_, err := safeVCDCall(ctx, log, instanceGroup, desc, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}
