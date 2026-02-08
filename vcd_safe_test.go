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
	result, err := safeVCDCall(context.Background(), testLogger(), "test", "test-op", func() (string, error) {
		return "hello", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "hello", result)
}

func TestSafeVCDCall_Error(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(shortCtx(), testLogger(), "test", "test-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("permanent error")
	})

	require.Error(t, err)
	// Should have retried until context expired
	assert.Greater(t, int(calls.Load()), 1)
}

func TestSafeVCDCall_RecoversPanic(t *testing.T) {
	var calls atomic.Int32

	_, err := safeVCDCall(shortCtx(), testLogger(), "test", "panic-op", func() (string, error) {
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

	result, err := safeVCDCall(shortCtx(), testLogger(), "test", "retry-op", func() (int, error) {
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

	result, err := safeVCDCall(shortCtx(), testLogger(), "test", "panic-then-ok", func() (string, error) {
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
	err := safeVCDCallVoid(context.Background(), testLogger(), "test", "void-op", func() error {
		return nil
	})

	require.NoError(t, err)
}

func TestSafeVCDCallVoid_Error(t *testing.T) {
	err := safeVCDCallVoid(shortCtx(), testLogger(), "test", "void-op", func() error {
		return errors.New("failed")
	})

	require.Error(t, err)
}

func TestSafeVCDCallVoid_RecoversPanic(t *testing.T) {
	var calls atomic.Int32

	err := safeVCDCallVoid(shortCtx(), testLogger(), "test", "void-panic", func() error {
		calls.Add(1)
		panic("boom")
	})

	require.Error(t, err)
	assert.Greater(t, int(calls.Load()), 1)
}

func TestSafeVCDCallVoid_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32

	err := safeVCDCallVoid(shortCtx(), testLogger(), "test", "void-retry", func() error {
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

	result, err := safeVCDCall(context.Background(), testLogger(), "test", "struct-op", func() (*myResult, error) {
		return &myResult{Name: "test", Age: 42}, nil
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "test", result.Name)
	assert.Equal(t, 42, result.Age)
}

func TestSafeVCDCall_NilResult(t *testing.T) {
	type myResult struct{}

	result, err := safeVCDCall(shortCtx(), testLogger(), "test", "nil-op", func() (*myResult, error) {
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

	_, err := safeVCDCall(ctx, testLogger(), "test", "cancel-op", func() (string, error) {
		calls.Add(1)
		return "", errors.New("keep retrying")
	})

	require.Error(t, err)
	assert.Greater(t, int(calls.Load()), 0)
}
