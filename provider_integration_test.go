package vcd

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

const (
	instanceGroupTestTimeout = 60 * time.Minute
	instanceGroupUpdateDelay = 10 * time.Second

	instanceGroupTestSize = 5
)

func TestBasicCloudDirector(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Error("mandatory environment variable VCD_URL not set")
	}

	if os.Getenv("VCD_ORG") == "" {
		t.Error("mandatory environment variable VCD_ORG not set")
	}

	if os.Getenv("VCD_VDC") == "" {
		t.Error("mandatory environment variable VCD_VDC not set")
	}

	if os.Getenv("VCD_NETWORK") == "" {
		t.Error("mandatory environment variable VCD_NETWORK not set")
	}

	if os.Getenv("VCD_NETWORK_ALLOCATION_MODE") == "" {
		t.Error("mandatory environment variable VCD_NETWORK_ALLOCATION_MODE not set")
	}

	if os.Getenv("VCD_TOKEN") == "" {
		t.Error("mandatory environment variable VCD_TOKEN not set")
	}

	if os.Getenv("VCD_CATALOG") == "" {
		t.Error("mandatory environment variable VCD_CATALOG not set")
	}

	if os.Getenv("VCD_TEMPLATE") == "" {
		t.Error("mandatory environment variable VCD_TEMPLATE not set")
	}

	if os.Getenv("VCD_VAPP_NAME_PREFIX") == "" {
		t.Error("mandatory environment variable VCD_VAPP not set")
	}

	t.Run("vcd_create_instance", func(t *testing.T) {
		instanceGroupName, err := generateVMName("test-create-instance")
		require.NoError(t, err)

		ig := &InstanceGroup{
			Name:              instanceGroupName,
			StrURL:            os.Getenv("VCD_URL"),
			Org:               os.Getenv("VCD_ORG"),
			Token:             os.Getenv("VCD_TOKEN"),
			VirtualDatacenter: os.Getenv("VCD_VDC"),
			Network:           os.Getenv("VCD_NETWORK"),
			IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
			InstanceGroupName: instanceGroupName,
			VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
			Catalog:           os.Getenv("VCD_CATALOG"),
			Template:          os.Getenv("VCD_TEMPLATE"),
			StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
			CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
			CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
			MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
			DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
			log: hclog.New(&hclog.LoggerOptions{
				Name:   "test",
				Level:  hclog.Debug,
				Output: os.Stdout,
			}),

			settings: provider.Settings{
				ConnectorConfig: provider.ConnectorConfig{
					UseStaticCredentials: true,
					Password:             "ExcellentPassword123!",
				},
			},
		}

		err = ig.populate()
		require.NoError(t, err)

		err = ig.validate()
		require.NoError(t, err)

		// Create a new VM
		result, err := ig.createInstance()
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotEmpty(t, result.VAppHREF)

		// Delete the VM
		err = ig.deleteInstance(result.VAppHREF)
		require.NoError(t, err)
	})

	t.Run("vcd_create_instance_group", func(t *testing.T) {
		instanceGroupName, err := generateVMName("test-instance-group")
		require.NoError(t, err)
		ig := &InstanceGroup{
			Name:              instanceGroupName,
			StrURL:            os.Getenv("VCD_URL"),
			Org:               os.Getenv("VCD_ORG"),
			Token:             os.Getenv("VCD_TOKEN"),
			VirtualDatacenter: os.Getenv("VCD_VDC"),
			Network:           os.Getenv("VCD_NETWORK"),
			IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
			InstanceGroupName: instanceGroupName,
			VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
			Catalog:           os.Getenv("VCD_CATALOG"),
			Template:          os.Getenv("VCD_TEMPLATE"),
			StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
			CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
			CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
			MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
			DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
			log: hclog.New(&hclog.LoggerOptions{
				Name:   "test",
				Level:  hclog.Debug,
				Output: os.Stdout,
			}),

			settings: provider.Settings{
				ConnectorConfig: provider.ConnectorConfig{
					UseStaticCredentials: true,
					Password:             "ExcellentPassword123!",
				},
			},
		}

		err = ig.populate()
		require.NoError(t, err)

		err = ig.validate()
		require.NoError(t, err)

		// Initialize store and reconciler (needed for Increase/Update)
		ig.store = newDesiredStateStore(ig.log, ig.InstanceGroupName)
		reconciler := newVCDInstanceGroup(ig.log, ig.store, ig, ReconcilerConfig{})
		reconciler.Start()
		ig.ig = reconciler
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), instanceGroupTestTimeout)
			defer cancel()
			reconciler.Shutdown(ctx)
		}()

		num, err := ig.Increase(context.Background(), 1)

		require.NoError(t, err)
		require.Equal(t, 1, num)

		// Wait for instance to be ready by checking status via Update()
		var instanceState provider.State
		var instanceID string

		ctx, _ := context.WithTimeout(context.Background(), instanceGroupTestTimeout)

		for instanceState != provider.StateRunning {
			select {
			case <-ctx.Done():
				t.Error("context deadline exceeded while waiting for instance to be ready")
				return
			default:
				time.Sleep(10 * time.Second)
				err = ig.Update(ctx, func(instance string, state provider.State) {
					instanceState = state
					instanceID = instance
				})
				require.NoError(t, err)
			}
		}

		require.NoError(t, ctx.Err())
		require.Equal(t, provider.StateRunning, instanceState)
		require.NotEmpty(t, instanceID)

		// increase to 2
		delta, err := ig.Increase(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, 1, delta)

		instanceIDs := waitForInstanceGroupSize(t, ctx, ig, 2)
		require.NotNil(t, instanceIDs)
		t.Logf("first increase instances done: %v", instanceIDs)

		// do more increases
		delta, err = ig.Increase(ctx, 3)
		require.NoError(t, err)
		require.Equal(t, 3, delta)
		instanceIDs = waitForInstanceGroupSize(t, ctx, ig, 5)
		require.NotNil(t, instanceIDs)
		require.Equal(t, 5, len(instanceIDs))
		t.Logf("second increase instances done: %v", instanceIDs)

		// decrease to 0
		deletedInstances, err := ig.Decrease(ctx, instanceIDs)
		require.NoError(t, err)
		require.Equal(t, 5, len(deletedInstances))

		instanceIDs = waitForInstanceGroupSize(t, ctx, ig, 0)
		require.NotNil(t, instanceIDs)
		require.Equal(t, 0, len(instanceIDs))
		t.Logf("decrease instances done: %v", instanceIDs)
	})
}

func TestProvisioning(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Error("mandatory environment variable VCD_URL not set")
	}

	if os.Getenv("VCD_ORG") == "" {
		t.Error("mandatory environment variable VCD_ORG not set")
	}

	if os.Getenv("VCD_VDC") == "" {
		t.Error("mandatory environment variable VCD_VDC not set")
	}

	if os.Getenv("VCD_NETWORK") == "" {
		t.Error("mandatory environment variable VCD_NETWORK not set")
	}

	if os.Getenv("VCD_NETWORK_ALLOCATION_MODE") == "" {
		t.Error("mandatory environment variable VCD_NETWORK_ALLOCATION_MODE not set")
	}

	if os.Getenv("VCD_TOKEN") == "" {
		t.Error("mandatory environment variable VCD_TOKEN not set")
	}

	if os.Getenv("VCD_CATALOG") == "" {
		t.Error("mandatory environment variable VCD_CATALOG not set")
	}

	if os.Getenv("VCD_TEMPLATE") == "" {
		t.Error("mandatory environment variable VCD_TEMPLATE not set")
	}

	if os.Getenv("VCD_VAPP_NAME_PREFIX") == "" {
		t.Error("mandatory environment variable VCD_VAPP not set")
	}

	pluginBinary := integration.BuildPluginBinary(t, "cmd/fleeting-plugin-vcd", "fleeting-plugin-vcd")

	t.Run("fleeting_static_credentials_ssh_key", func(t *testing.T) {
		instanceGroupName, err := generateVMName("test-fleeting-static-credentials-ssh-key")
		require.NoError(t, err)

		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		pemBlock, err := ssh.MarshalPrivateKey(crypto.PrivateKey(privateKey), "")
		require.NoError(t, err)

		privateKeyPem := pem.EncodeToMemory(pemBlock)

		integration.TestProvisioning(t,
			pluginBinary,
			integration.Config{
				PluginConfig: InstanceGroup{
					Name:              "vcd",
					StrURL:            os.Getenv("VCD_URL"),
					Org:               os.Getenv("VCD_ORG"),
					VirtualDatacenter: os.Getenv("VCD_VDC"),
					Network:           os.Getenv("VCD_NETWORK"),
					IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
					Token:             os.Getenv("VCD_TOKEN"),
					InstanceGroupName: instanceGroupName,
					VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
					Catalog:           os.Getenv("VCD_CATALOG"),
					Template:          os.Getenv("VCD_TEMPLATE"),
					StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
					CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
					CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
					MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
					DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
				},
				ConnectorConfig: provider.ConnectorConfig{
					Timeout:              30 * time.Minute,
					UseStaticCredentials: true,
					Username:             "root",
					Password:             "ExcellentPassword123!",
					Key:                  privateKeyPem,
				},
				MaxInstances:    3,
				UseExternalAddr: true,
			},
		)
	})

	t.Run("fleeting_static_credentials_user_password", func(t *testing.T) {
		instanceGroupName, err := generateVMName("test-fleeting-static-credentials-user-password")
		require.NoError(t, err)

		integration.TestProvisioning(t,
			pluginBinary,
			integration.Config{
				PluginConfig: InstanceGroup{
					Name:              "vcd",
					StrURL:            os.Getenv("VCD_URL"),
					Org:               os.Getenv("VCD_ORG"),
					VirtualDatacenter: os.Getenv("VCD_VDC"),
					Network:           os.Getenv("VCD_NETWORK"),
					IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
					Token:             os.Getenv("VCD_TOKEN"),
					InstanceGroupName: instanceGroupName,
					VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
					Catalog:           os.Getenv("VCD_CATALOG"),
					Template:          os.Getenv("VCD_TEMPLATE"),
					StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
					CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
					CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
					MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
					DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
				},
				ConnectorConfig: provider.ConnectorConfig{
					Timeout:              30 * time.Minute,
					UseStaticCredentials: true,
					Username:             "foobar",
					Password:             "ExcellentPassword123!",
				},
				MaxInstances:    3,
				UseExternalAddr: true,
			},
		)
	})
}

func TestScaleLimits(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Skip("VCD_URL not set, skipping")
	}

	const targetSize = 2

	instanceGroupName, err := generateVMName("test-scale-limits")
	require.NoError(t, err)

	ig := &InstanceGroup{
		Name:              instanceGroupName,
		StrURL:            os.Getenv("VCD_URL"),
		Org:               os.Getenv("VCD_ORG"),
		Token:             os.Getenv("VCD_TOKEN"),
		VirtualDatacenter: os.Getenv("VCD_VDC"),
		Network:           os.Getenv("VCD_NETWORK"),
		IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
		InstanceGroupName: instanceGroupName,
		VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
		Catalog:           os.Getenv("VCD_CATALOG"),
		Template:          os.Getenv("VCD_TEMPLATE"),
		StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
		CPUCount:          1,
		CoresPerSocket:    1,
		MemoryMB:          512,
		DiskSizeGB:        0, // use template default
		log: hclog.New(&hclog.LoggerOptions{
			Name:   "test",
			Level:  hclog.Debug,
			Output: os.Stdout,
		}),

		settings: provider.Settings{
			ConnectorConfig: provider.ConnectorConfig{
				UseStaticCredentials: true,
				Password:             "ExcellentPassword123!",
			},
		},
	}

	err = ig.populate()
	require.NoError(t, err)

	err = ig.validate()
	require.NoError(t, err)

	ig.store = newDesiredStateStore(ig.log, ig.InstanceGroupName)
	reconciler := newVCDInstanceGroup(ig.log, ig.store, ig, ReconcilerConfig{
		MaxConcurrentCreates: 5,
		MaxConcurrentDeletes: 10,
	})
	reconciler.Start()
	ig.ig = reconciler
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), instanceGroupTestTimeout)
		defer cancel()
		reconciler.Shutdown(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), instanceGroupTestTimeout)
	defer cancel()

	// Request all at once
	startTime := time.Now()
	t.Logf("requesting %d instances", targetSize)
	num, err := ig.Increase(ctx, targetSize)
	require.NoError(t, err)
	require.Equal(t, targetSize, num)

	// Wait for all to be running
	instanceIDs := waitForInstanceGroupSize(t, ctx, ig, targetSize)
	require.NotNil(t, instanceIDs)
	require.Equal(t, targetSize, len(instanceIDs))
	createDuration := time.Since(startTime)
	t.Logf("all %d instances running in %s", targetSize, createDuration)

	// Delete all
	startTime = time.Now()
	deletedInstances, err := ig.Decrease(ctx, instanceIDs)
	require.NoError(t, err)
	require.Equal(t, targetSize, len(deletedInstances))

	instanceIDs = waitForInstanceGroupSize(t, ctx, ig, 0)
	require.NotNil(t, instanceIDs)
	require.Equal(t, 0, len(instanceIDs))
	deleteDuration := time.Since(startTime)
	t.Logf("all %d instances deleted in %s", targetSize, deleteDuration)

	t.Logf("SUMMARY: create=%s delete=%s total=%s", createDuration, deleteDuration, createDuration+deleteDuration)
}

func mustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}

func waitForInstanceGroupSize(t *testing.T, ctx context.Context, ig *InstanceGroup, expectedSize int) []string {
	instanceMap := xsync.NewMapOf[string, provider.State]()
	for instanceMap.Size() != expectedSize {
		select {
		case <-ctx.Done():
			t.Errorf("context deadline exceeded while waiting for %d instances", expectedSize)
			return nil
		default:
			instanceMap.Clear()

			err := ig.Update(ctx, func(instance string, state provider.State) {
				instanceMap.Store(instance, state)
			})
			require.NoError(t, err)

			if instanceMap.Size() == expectedSize {
				t.Logf("instance group size is %d", instanceMap.Size())
				break
			}

			time.Sleep(instanceGroupUpdateDelay)
		}
	}

	instanceIDs := []string{}
	instanceMap.Range(func(key string, value provider.State) bool {
		instanceIDs = append(instanceIDs, key)
		return true
	})

	return instanceIDs
}
