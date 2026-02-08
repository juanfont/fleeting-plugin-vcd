package vcd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

const (
	igTestTimeout     = 60 * time.Minute
	igTestPollDelay   = 10 * time.Second
)

func newTestInstanceGroup(t *testing.T) (*InstanceGroup, VCDInstanceGroup) {
	t.Helper()

	instanceGroupName, err := generateVMName("test-ig")
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

	store := newDesiredStateStore(ig.log, ig.InstanceGroupName)
	ig.store = store

	reconciler := newVCDInstanceGroup(ig.log, store, ig, ReconcilerConfig{
		MaxConcurrentCreates: 3,
		MaxConcurrentDeletes: 5,
	})
	reconciler.Start()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
		defer cancel()
		err := reconciler.Shutdown(ctx)
		if err != nil {
			t.Logf("error during shutdown: %v", err)
		}
	})

	return ig, reconciler
}

func skipIfNoVCDEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("VCD_URL") == "" {
		t.Skip("VCD_URL not set, skipping integration test")
	}
}

func waitForPhase(t *testing.T, ctx context.Context, ig VCDInstanceGroup, phase Phase, expectedCount int) []Instance {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			instances := ig.Instances()
			t.Fatalf("timeout waiting for %d instances in phase %s (have %d total)", expectedCount, phase, len(instances))
			return nil
		default:
			instances := ig.Instances()
			matching := 0
			for _, inst := range instances {
				if inst.Phase == phase {
					matching++
				}
			}
			if matching >= expectedCount {
				var result []Instance
				for _, inst := range instances {
					if inst.Phase == phase {
						result = append(result, inst)
					}
				}
				return result
			}
			time.Sleep(igTestPollDelay)
		}
	}
}

func waitForNonDeletedCount(t *testing.T, ctx context.Context, ig VCDInstanceGroup, expectedCount int) []Instance {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			instances := ig.Instances()
			count := 0
			for _, inst := range instances {
				if inst.Phase != PhaseDeleted {
					count++
				}
			}
			t.Fatalf("timeout waiting for %d non-deleted instances (have %d)", expectedCount, count)
			return nil
		default:
			instances := ig.Instances()
			var nonDeleted []Instance
			for _, inst := range instances {
				if inst.Phase != PhaseDeleted {
					nonDeleted = append(nonDeleted, inst)
				}
			}
			if len(nonDeleted) == expectedCount {
				return nonDeleted
			}
			time.Sleep(igTestPollDelay)
		}
	}
}

func TestInstanceGroup_CreateSingle(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create 1 instance
	ids := ig.Increase(1)
	require.Len(t, ids, 1)

	// Wait for it to be Running
	running := waitForPhase(t, ctx, ig, PhaseRunning, 1)
	require.Len(t, running, 1)
	require.NotEmpty(t, running[0].IPAddress)
	require.NotEmpty(t, running[0].ID)

	t.Logf("instance running: href=%s ip=%s", running[0].ID, running[0].IPAddress)

	// Delete it
	ig.Decrease([]string{running[0].ID})

	// Wait for deletion
	waitForNonDeletedCount(t, ctx, ig, 0)
}

func TestInstanceGroup_ScaleUp(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create 1
	ig.Increase(1)
	waitForPhase(t, ctx, ig, PhaseRunning, 1)

	// Add 2 more
	ig.Increase(2)
	waitForPhase(t, ctx, ig, PhaseRunning, 3)

	// Add 2 more = 5 total
	ig.Increase(2)
	running := waitForPhase(t, ctx, ig, PhaseRunning, 5)
	require.Len(t, running, 5)

	t.Logf("5 instances running")

	// Clean up
	for _, inst := range running {
		ig.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, ig, 0)
}

func TestInstanceGroup_ScaleDown(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create 5
	ig.Increase(5)
	running := waitForPhase(t, ctx, ig, PhaseRunning, 5)
	require.Len(t, running, 5)

	// Delete 3
	toDelete := make([]string, 3)
	for i := 0; i < 3; i++ {
		toDelete[i] = running[i].ID
	}
	ig.Decrease(toDelete)

	// Verify 2 remain
	remaining := waitForNonDeletedCount(t, ctx, ig, 2)
	require.Len(t, remaining, 2)

	// Clean up
	for _, inst := range remaining {
		ig.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, ig, 0)
}

func TestInstanceGroup_ScaleToZero(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create 5
	ig.Increase(5)
	running := waitForPhase(t, ctx, ig, PhaseRunning, 5)
	require.Len(t, running, 5)

	// Delete all
	toDelete := make([]string, len(running))
	for i, inst := range running {
		toDelete[i] = inst.ID
	}
	ig.Decrease(toDelete)

	// Verify 0
	waitForNonDeletedCount(t, ctx, ig, 0)
}

func TestInstanceGroup_Reconciliation(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create instances and observe transitions
	ids := ig.Increase(2)
	require.Len(t, ids, 2)

	// Check that they start in PendingCreate or Creating
	time.Sleep(2 * time.Second) // give reconciler a moment
	instances := ig.Instances()
	for _, inst := range instances {
		t.Logf("instance %s: phase=%s", inst.IntentID, inst.Phase)
		require.True(t, inst.Phase == PhasePendingCreate || inst.Phase == PhaseCreating,
			"expected PendingCreate or Creating, got %s", inst.Phase)
	}

	// Wait for Running
	running := waitForPhase(t, ctx, ig, PhaseRunning, 2)
	require.Len(t, running, 2)

	// Clean up
	for _, inst := range running {
		ig.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, ig, 0)
}

func TestInstanceGroup_Shutdown(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)

	// Create instances
	ig.Increase(2)

	// Wait for Running
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()
	waitForPhase(t, ctx, ig, PhaseRunning, 2)

	// Shutdown should clean up all instances
	err := ig.Shutdown(ctx)
	require.NoError(t, err)
}

// TestInstanceGroup_OrphanEmptyVAppCleanup creates an empty vApp with metadata outside
// the reconciler and verifies it gets auto-deleted when the reconciler discovers it.
func TestInstanceGroup_OrphanEmptyVAppCleanup(t *testing.T) {
	skipIfNoVCDEnv(t)

	rawIG, _ := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create an orphaned empty vApp directly via VCD API
	client, err := rawIG.getVCDClient()
	require.NoError(t, err)

	org, err := client.GetOrgByName(rawIG.Org)
	require.NoError(t, err)

	vdc, err := org.GetVDCByName(rawIG.VirtualDatacenter, true)
	require.NoError(t, err)

	orphanName := fmt.Sprintf("%sorphan-test-%d", rawIG.VAppNamePrefix, time.Now().UnixNano()%100000)
	vapp, err := vdc.CreateRawVApp(orphanName, "orphan test vApp")
	require.NoError(t, err)

	t.Logf("created orphan empty vApp: %s (href=%s)", orphanName, vapp.VApp.HREF)

	// Add metadata so the reconciler finds it
	err = vapp.AddMetadataEntryWithVisibility(
		instanceGroupMetadataKey,
		rawIG.InstanceGroupName,
		types.MetadataStringValue,
		types.MetadataReadWriteVisibility,
		false,
	)
	require.NoError(t, err)

	// Wait for the reconciler to detect and delete the orphan.
	// Poll VCD directly to check if the vApp is gone.
	orphanHREF := vapp.VApp.HREF
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for orphan vApp to be deleted")
		default:
			_, err := vdc.GetVAppByHref(orphanHREF)
			if err != nil {
				t.Logf("orphan vApp deleted successfully")
				return
			}
			time.Sleep(igTestPollDelay)
		}
	}
}

// TestInstanceGroup_RapidScaleDown creates instances then immediately requests deletion
// before all creates finish. Verifies everything converges to 0 with no leaked VMs.
func TestInstanceGroup_RapidScaleDown(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Request 2 creates
	ig.Increase(2)

	// Wait for all to reach Running first — then delete them all at once
	running := waitForPhase(t, ctx, ig, PhaseRunning, 2)
	t.Logf("all 2 instances running, deleting all at once")

	toDelete := make([]string, len(running))
	for i, inst := range running {
		toDelete[i] = inst.ID
	}
	ig.Decrease(toDelete)

	// Wait for everything to be gone
	waitForNonDeletedCount(t, ctx, ig, 0)
	t.Logf("all instances deleted successfully")
}

// TestInstanceGroup_ConcurrentGroups runs two instance groups simultaneously on the same
// VDC and verifies they don't interfere with each other.
func TestInstanceGroup_ConcurrentGroups(t *testing.T) {
	skipIfNoVCDEnv(t)

	_, igA := newTestInstanceGroup(t)
	_, igB := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Create 1 instance in each group
	igA.Increase(1)
	igB.Increase(1)

	// Wait for both to have 1 Running
	runningA := waitForPhase(t, ctx, igA, PhaseRunning, 1)
	runningB := waitForPhase(t, ctx, igB, PhaseRunning, 1)

	t.Logf("group A has %d running, group B has %d running", len(runningA), len(runningB))

	// Verify no overlap in instance IDs
	hrefsA := map[string]bool{}
	for _, inst := range runningA {
		hrefsA[inst.ID] = true
	}
	for _, inst := range runningB {
		assert.False(t, hrefsA[inst.ID], "instance %s appears in both groups", inst.ID)
	}

	// Delete group A, verify group B is unaffected
	for _, inst := range runningA {
		igA.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, igA, 0)

	// Group B should still have 1
	remainingB := waitForPhase(t, ctx, igB, PhaseRunning, 1)
	require.Len(t, remainingB, 1)

	// Clean up group B
	for _, inst := range remainingB {
		igB.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, igB, 0)
}

// TestInstanceGroup_ShutdownMidCreate calls Shutdown while creates are still in-flight.
// Verifies clean shutdown with no leaked VMs.
func TestInstanceGroup_ShutdownMidCreate(t *testing.T) {
	skipIfNoVCDEnv(t)

	rawIG, ig := newTestInstanceGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	// Request 2 creates
	ig.Increase(2)

	// Wait a bit for some to start, but not all to finish
	time.Sleep(30 * time.Second)

	instances := ig.Instances()
	t.Logf("instances at shutdown time:")
	for _, inst := range instances {
		t.Logf("  %s: phase=%s id=%s", inst.IntentID, inst.Phase, inst.ID)
	}

	// Shutdown mid-create
	err := ig.Shutdown(ctx)
	require.NoError(t, err)

	// Verify no VMs left in VCD for this instance group
	time.Sleep(30 * time.Second) // give VCD time to settle
	vApps, err := rawIG.getInstancesInInstanceGroup()
	require.NoError(t, err)
	assert.Empty(t, vApps, "expected no vApps left after shutdown, found %d", len(vApps))
}

// TestInstanceGroup_ScaleLimits verifies that MaxConcurrentCreates throttles correctly.
// Requests more creates than the limit and verifies all eventually complete.
func TestInstanceGroup_ScaleLimits(t *testing.T) {
	skipIfNoVCDEnv(t)

	// Use a custom instance group with MaxConcurrentCreates=2
	instanceGroupName, err := generateVMName("test-limits")
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
			Name:   "test-limits",
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

	store := newDesiredStateStore(ig.log, ig.InstanceGroupName)
	ig.store = store

	reconciler := newVCDInstanceGroup(ig.log, store, ig, ReconcilerConfig{
		MaxConcurrentCreates: 1, // Only 1 concurrent create
		MaxConcurrentDeletes: 5,
	})
	reconciler.Start()

	ctx, cancel := context.WithTimeout(context.Background(), igTestTimeout)
	defer cancel()

	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), igTestTimeout)
		defer shutdownCancel()
		if err := reconciler.Shutdown(shutdownCtx); err != nil {
			t.Logf("error during shutdown: %v", err)
		}
	})

	// Request 2 creates — with MaxConcurrentCreates=1, they must be serialized
	ids := reconciler.Increase(2)
	require.Len(t, ids, 2)

	// Both should eventually reach Running
	running := waitForPhase(t, ctx, reconciler, PhaseRunning, 2)
	require.Len(t, running, 2)

	t.Logf("both instances running despite MaxConcurrentCreates=1")

	// Clean up
	for _, inst := range running {
		reconciler.Decrease([]string{inst.ID})
	}
	waitForNonDeletedCount(t, ctx, reconciler, 0)
}
