package vcd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() hclog.Logger {
	return hclog.NewNullLogger()
}

func shortCtx() context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	return ctx
}

func TestSafeVCDCall_Success(t *testing.T) {
	result, err := safeVCDCall(context.Background(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "test-op", func() (string, error) {
		return "hello", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "hello", result)
}

func TestSafeVCDCall_Error(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "test-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("permanent error")
	})

	require.Error(t, err)
	// Should have retried until context expired
	assert.Greater(t, int(calls.Load()), 1)
}

func TestSafeVCDCall_RecoversPanic(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "panic-op", func() (string, error) {
		calls.Add(1)
		panic("vcd library exploded")
	})

	// Should error (panics are converted to errors and retried until context expires)
	require.Error(t, err)

	// Should have retried multiple times
	assert.Greater(t, int(calls.Load()), 1)
}

func TestSafeVCDCall_RetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32

	result, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "retry-op", func() (int, error) {
		n := calls.Add(1)
		if n < 3 {
			return 0, errors.New("transient error")
		}
		return 42, nil
	})

	require.NoError(t, err)
	assert.Equal(t, 42, result)
	assert.Equal(t, int32(3), calls.Load())
}

func TestSafeVCDCall_RetriesPanicThenSucceeds(t *testing.T) {
	var calls atomic.Int32

	result, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "panic-then-ok", func() (string, error) {
		n := calls.Add(1)
		if n == 1 {
			panic("first call panics")
		}
		return "recovered", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "recovered", result)
	assert.Equal(t, int32(2), calls.Load())
}

func TestSafeVCDCallVoid_Success(t *testing.T) {
	err := safeVCDCallVoid(context.Background(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "void-op", func() error {
		return nil
	})

	require.NoError(t, err)
}

func TestSafeVCDCallVoid_Error(t *testing.T) {
	err := safeVCDCallVoid(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "void-op", func() error {
		return errors.New("failed")
	})

	require.Error(t, err)
}

func TestSafeVCDCallVoid_RecoversPanic(t *testing.T) {
	var calls atomic.Int32

	err := safeVCDCallVoid(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "void-panic", func() error {
		calls.Add(1)
		panic("boom")
	})

	require.Error(t, err)
	assert.Greater(t, int(calls.Load()), 1)
}

func TestSafeVCDCallVoid_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32

	err := safeVCDCallVoid(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "void-retry", func() error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("not yet")
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

func TestSafeVCDCall_StructResult(t *testing.T) {
	type myResult struct {
		Name string
		Age  int
	}

	result, err := safeVCDCall(context.Background(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "struct-op", func() (*myResult, error) {
		return &myResult{Name: "test", Age: 42}, nil
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "test", result.Name)
	assert.Equal(t, 42, result.Age)
}

func TestSafeVCDCall_NilResult(t *testing.T) {
	type myResult struct{}

	result, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "nil-op", func() (*myResult, error) {
		return nil, errors.New("nope")
	})

	require.Error(t, err)
	assert.Nil(t, result)
}

func TestSafeVCDCall_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := safeVCDCall(ctx, &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "cancel-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("keep retrying")
	})

	require.Error(t, err)
	assert.Greater(t, int(calls.Load()), 0)
}

// --- Permanent error detection tests ---

func TestIsPermanentError_StorageQuota(t *testing.T) {
	err := errors.New("API Error: 400: exceed the VDC's storage quota")
	assert.True(t, isPermanentError(err))
}

func TestIsPermanentError_EntityNotExist(t *testing.T) {
	err := errors.New("entity does not exist")
	assert.True(t, isPermanentError(err))
}

func TestIsPermanentError_UnableToPerform(t *testing.T) {
	err := errors.New("[400:VALIDATION] - Unable to perform this action. Contact your cloud administrator.")
	assert.True(t, isPermanentError(err))
}

func TestIsPermanentError_TransientError(t *testing.T) {
	err := errors.New("connection refused")
	assert.False(t, isPermanentError(err))
}

func TestIsPermanentError_Nil(t *testing.T) {
	assert.False(t, isPermanentError(nil))
}

func TestSafeVCDCall_PermanentErrorNoRetry(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(context.Background(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "permanent-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("exceed the VDC's storage quota")
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage quota")
	// Should NOT retry — only 1 call
	assert.Equal(t, int32(1), calls.Load())
}

func TestSafeVCDCall_PermanentErrorUnableToPerform(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(context.Background(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "unable-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("Unable to perform this action. Contact your cloud administrator.")
	})

	require.Error(t, err)
	// Should NOT retry — only 1 call
	assert.Equal(t, int32(1), calls.Load())
}

const operationLimitMsg = "API Error: 503: The maximum number of simultaneous operations for organization acme has been reached"

func TestVCDCall_OperationLimitWaitsAndReplays(t *testing.T) {
	calls := map[string]func(*InstanceGroup, func() (int, error)) (int, error){
		"safe": func(g *InstanceGroup, fn func() (int, error)) (int, error) {
			return safeVCDCall(shortCtx(), g, "op", fn)
		},
		"single": func(g *InstanceGroup, fn func() (int, error)) (int, error) {
			return singleVCDCall(shortCtx(), g, "op", fn)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			g := &InstanceGroup{log: testLogger(), InstanceGroupName: "test", throttle: testGate(20 * time.Millisecond)}
			var n atomic.Int32
			start := time.Now()
			v, err := call(g, func() (int, error) {
				if n.Add(1) <= 2 {
					return 0, errors.New(operationLimitMsg)
				}
				return 7, nil
			})
			require.NoError(t, err)
			assert.Equal(t, 7, v)
			assert.Equal(t, int32(3), n.Load())
			// Pauses of 20ms then 40ms: the second rejection started a new episode.
			assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)
		})
	}
}

func TestVCDCall_OperationLimitWaitHonoursContext(t *testing.T) {
	g := &InstanceGroup{log: testLogger(), InstanceGroupName: "test", throttle: testGate(time.Hour)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	_, err := safeVCDCall(ctx, g, "op", func() (int, error) {
		n.Add(1)
		return 0, errors.New(operationLimitMsg)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), n.Load())
}

func TestVCDCall_OperationLimitWithoutGateIsNotReplayedForMutations(t *testing.T) {
	g := &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}
	var n atomic.Int32
	_, err := singleVCDCall(context.Background(), g, "op", func() (int, error) {
		n.Add(1)
		return 0, errors.New(operationLimitMsg)
	})
	require.Error(t, err)
	assert.Equal(t, int32(1), n.Load())
}
