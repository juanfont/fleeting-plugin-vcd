package vcd

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
)

// Verbatim from esait-pl1-linux-large on 2026-09-24.
const vanishedVAppMsg = "error retrieving vApp: API Error: 403: [ 036ac812-64c0-4bc1-b929-14a8e697f33e ] The VMware Cloud Director entity com.vmware.vcloud.entity.vapp:b84b56a9-97ac-4c48-9655-619ba8efae20 does not exist."

func TestIsEntityNotFoundError(t *testing.T) {
	assert.True(t, isEntityNotFoundError(errors.New(vanishedVAppMsg)))
	assert.True(t, isEntityNotFoundError(govcd.ErrorEntityNotFound))
	assert.True(t, isEntityNotFoundError(fmt.Errorf("lookup: %w", govcd.ErrorEntityNotFound)))
	assert.True(t, isEntityNotFoundError(errors.New("[ENF] entity not found")))
	assert.False(t, isEntityNotFoundError(errors.New("API Error: 500: Internal Server Error")))
	assert.False(t, isEntityNotFoundError(nil))
}

func TestNotFoundIsPermanent_StopsRetrying(t *testing.T) {
	var calls atomic.Int32
	_, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "GetVAppByHref(poll)",
		notFoundIsPermanent(func() (int, error) {
			calls.Add(1)
			return 0, errors.New(vanishedVAppMsg)
		}))
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

func TestNotFoundIsPermanent_OtherErrorsStillRetry(t *testing.T) {
	var calls atomic.Int32
	v, err := safeVCDCall(shortCtx(), &InstanceGroup{log: testLogger(), InstanceGroupName: "test"}, "GetVAppByHref(poll)",
		notFoundIsPermanent(func() (int, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("API Error: 502: Bad Gateway")
			}
			return 7, nil
		}))
	require.NoError(t, err)
	assert.Equal(t, 7, v)
	assert.Equal(t, int32(2), calls.Load())
}
