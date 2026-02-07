package vcd

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/go-hclog"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
	"golang.org/x/sync/semaphore"
)

const (
	defaultReconcileInterval    = 10 * time.Second
	defaultMaxConcurrentCreates = 3
	defaultMaxConcurrentDeletes = 5
	defaultMaxInstanceAge       = 24 * time.Hour
	gcCheckEveryN               = 360 // ~1h at 10s interval

	// Retry backoff for per-intent retries
	retryBaseDelay  = 10 * time.Second
	retryMaxDelay   = 5 * time.Minute
	retryMultiplier = 2.0
	retryJitter     = 0.25
)

// ReconcilerConfig holds configuration for the reconciliation loop.
type ReconcilerConfig struct {
	MaxConcurrentCreates int
	MaxConcurrentDeletes int
	Interval             time.Duration
	MaxInstanceAge       time.Duration
}

func (c *ReconcilerConfig) withDefaults() ReconcilerConfig {
	out := *c
	if out.MaxConcurrentCreates <= 0 {
		out.MaxConcurrentCreates = defaultMaxConcurrentCreates
	}
	if out.MaxConcurrentDeletes <= 0 {
		out.MaxConcurrentDeletes = defaultMaxConcurrentDeletes
	}
	if out.Interval <= 0 {
		out.Interval = defaultReconcileInterval
	}
	if out.MaxInstanceAge <= 0 {
		out.MaxInstanceAge = defaultMaxInstanceAge
	}
	return out
}

// vcdInstanceGroup implements VCDInstanceGroup using a reconciliation loop.
type vcdInstanceGroup struct {
	log    hclog.Logger
	store  *desiredStateStore
	config ReconcilerConfig
	ig     *InstanceGroup // reference to the provider for VCD operations

	createSem *semaphore.Weighted
	deleteSem *semaphore.Weighted

	triggerCh    chan struct{}
	stopCh       chan struct{}
	doneCh       chan struct{}
	shutdownOnce sync.Once
	shutdownErr  error
}

func newVCDInstanceGroup(log hclog.Logger, store *desiredStateStore, ig *InstanceGroup, config ReconcilerConfig) *vcdInstanceGroup {
	config = config.withDefaults()

	return &vcdInstanceGroup{
		log:       log,
		store:     store,
		config:    config,
		ig:        ig,
		createSem: semaphore.NewWeighted(int64(config.MaxConcurrentCreates)),
		deleteSem: semaphore.NewWeighted(int64(config.MaxConcurrentDeletes)),
		triggerCh: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start begins the reconciliation loop.
func (r *vcdInstanceGroup) Start() {
	go r.run()
}

func (r *vcdInstanceGroup) run() {
	defer close(r.doneCh)

	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()

	gcCounter := 0

	for {
		select {
		case <-r.stopCh:
			return
		case <-r.triggerCh:
			r.reconcileOnce(gcCounter%gcCheckEveryN == 0)
			gcCounter++
		case <-ticker.C:
			r.reconcileOnce(gcCounter%gcCheckEveryN == 0)
			gcCounter++
		}
	}
}

// trigger requests an immediate reconciliation cycle.
func (r *vcdInstanceGroup) trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
		// already triggered
	}
}

func (r *vcdInstanceGroup) reconcileOnce(doGC bool) {
	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime).Seconds()
		ReconcileDuration.WithLabelValues(r.store.instanceGroupName).Observe(duration)
		ReconcileTotal.WithLabelValues(r.store.instanceGroupName).Inc()
	}()

	// Recover from panics in the reconcile loop itself
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("panic recovered in reconcileOnce", "panic", rec)
		}
	}()

	// Step 1: Poll VCD and update cache
	r.pollVCD()

	// Step 2: Dispatch pending creates
	r.dispatchCreates()

	// Step 3: Dispatch pending deletes
	r.dispatchDeletes()

	// Step 4: GC check
	if doGC {
		r.gcCheck()
	}

	// Step 5: Prune deleted instances
	r.store.Prune()

	// Step 6: Update metrics
	r.store.UpdateMetrics()
	PoolSize.WithLabelValues(r.store.instanceGroupName).Set(float64(r.store.Size()))
}

func (r *vcdInstanceGroup) pollVCD() {
	vApps, err := r.ig.getInstancesInInstanceGroup()
	if err != nil {
		r.log.Error("error polling VCD", "error", err)
		return
	}

	knownHREFs := make(map[string]bool)
	client, err := r.ig.getVCDClient()
	if err != nil {
		r.log.Error("error getting VCD client for poll", "error", err)
		return
	}

	for _, vapp := range vApps {
		href := vapp.VApp.HREF
		knownHREFs[href] = true // Always track to prevent false disappearance

		hasVM := vapp.VApp.Children != nil && len(vapp.VApp.Children.VM) > 0

		if hasVM {
			vmName := vapp.VApp.Children.VM[0].Name
			vappStatus := types.VAppStatuses[vapp.VApp.Status]
			vmStatus := types.VAppStatuses[vapp.VApp.Children.VM[0].Status]

			// Try to get IP address and OS type from the VM
			vmHREF := vapp.VApp.Children.VM[0].HREF
			pollResult, err := safeVCDCall(context.Background(), r.log, r.ig.InstanceGroupName, "refresh VM for poll", func() (*vmPollResult, error) {
				vm := govcd.NewVM(&client.Client)
				vm.VM.HREF = vmHREF
				if err := vm.Refresh(); err != nil {
					if isEntityNotFoundError(err) {
						return nil, backoff.Permanent(err)
					}
					return nil, err
				}
				ip, _ := getPrimaryIPAddress(vm)
				var osType string
				if vm.VM.VmSpecSection != nil {
					osType = vm.VM.VmSpecSection.OsType
				}
				return &vmPollResult{IP: ip, OSType: osType}, nil
			})

			var ipAddress, osType string
			if err == nil && pollResult != nil {
				ipAddress = pollResult.IP
				osType = pollResult.OSType
			}

			// Update existing or add as preexisting.
			// Skip AddPreexisting when there are in-flight creates, because the VApp
			// is almost certainly from one of our create workers — not truly preexisting.
			// It will be properly linked when doCreate calls UpdateInstance with the HREF.
			if !r.store.UpdateFromVCD(href, vapp.VApp.Name, vmName, ipAddress, vappStatus, vmStatus, osType) {
				if !r.store.HasCreating() {
					r.store.AddPreexisting(href, vapp.VApp.Name, vmName, ipAddress, vappStatus, vmStatus, osType)
				}
			}
		} else {
			// Empty vApp (in-flight creation or orphan) — update if tracked, but don't add as preexisting
			vappStatus := types.VAppStatuses[vapp.VApp.Status]
			if !r.store.UpdateFromVCD(href, vapp.VApp.Name, "", "", vappStatus, "", "") && !r.store.HasCreating() {
				// Not tracked in store and no in-flight creates — this is an orphaned empty vApp
				// (e.g. process crashed between CreateRawVApp and AddNewVMWithStorageProfile).
				// Clean it up so it doesn't consume quota forever.
				r.log.Warn("deleting orphaned empty vApp", "href", href, "name", vapp.VApp.Name)
				go r.ig.deleteInstance(href)
			}
		}
	}

	// Mark disappeared instances — but only if the poll actually returned results.
	// If VCD is unreachable, getInstancesInInstanceGroup returns an empty list to
	// keep fleeting alive. We must not treat that as "all instances disappeared"
	// or fleeting will request a stampede of new creates when VCD comes back.
	if len(knownHREFs) > 0 || r.store.Size() == 0 {
		r.store.MarkDisappeared(knownHREFs)
	} else {
		r.log.Warn("skipping MarkDisappeared: poll returned 0 vApps but store has instances (VCD may be unreachable)")
	}
}

type vmPollResult struct {
	IP     string
	OSType string
}

func (r *vcdInstanceGroup) dispatchCreates() {
	pending := r.store.GetPendingCreates()
	for _, inst := range pending {
		intentID := inst.IntentID
		if !r.createSem.TryAcquire(1) {
			break // max concurrent creates reached
		}

		// Mark as creating
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhaseCreating
			now := time.Now()
			inst.CreateStartedAt = &now
		})

		go func() {
			defer r.createSem.Release(1)
			r.doCreate(intentID)
		}()
	}
}

func (r *vcdInstanceGroup) doCreate(intentID string) {
	startTime := time.Now()

	result, err := r.ig.createInstance()
	duration := time.Since(startTime).Seconds()
	InstanceCreationDuration.WithLabelValues(r.store.instanceGroupName).Observe(duration)

	if err != nil {
		InstancesFailedTotal.WithLabelValues(r.store.instanceGroupName, "create").Inc()
		r.log.Error("instance creation failed", "intentID", intentID, "error", err)

		// Back to PendingCreate with retry backoff
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhasePendingCreate
			inst.CreateStartedAt = nil
			inst.RetryCount++
			inst.LastError = err.Error()
			delay := retryDelay(inst.RetryCount)
			nextRetry := time.Now().Add(delay)
			inst.NextRetryAfter = &nextRetry
		})
		return
	}

	InstancesCreatedTotal.WithLabelValues(r.store.instanceGroupName).Inc()

	// Success — update instance with VCD data
	r.store.UpdateInstance(intentID, func(inst *Instance) {
		inst.Phase = PhaseRunning
		now := time.Now()
		inst.CreateCompletedAt = &now
		inst.ID = result.VAppHREF
		inst.Name = result.VAppName
		inst.VMName = result.VMName
		inst.IPAddress = result.IPAddress
		inst.OSType = result.OSType
		inst.LastError = ""
		inst.NextRetryAfter = nil
	})

	r.log.Info("instance created successfully",
		"intentID", intentID,
		"href", result.VAppHREF,
		"vappName", result.VAppName,
		"ip", result.IPAddress,
	)
}

func (r *vcdInstanceGroup) dispatchDeletes() {
	pending := r.store.GetPendingDeletes()
	for _, inst := range pending {
		intentID := inst.IntentID
		href := inst.ID
		if href == "" {
			// Instance was never created in VCD, just mark as deleted
			r.store.UpdateInstance(intentID, func(inst *Instance) {
				inst.Phase = PhaseDeleted
				now := time.Now()
				inst.DeleteCompletedAt = &now
			})
			continue
		}

		if !r.deleteSem.TryAcquire(1) {
			break // max concurrent deletes reached
		}

		// Mark as deleting
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhaseDeleting
			now := time.Now()
			inst.DeleteStartedAt = &now
		})

		go func() {
			defer r.deleteSem.Release(1)
			r.doDelete(intentID, href)
		}()
	}
}

func (r *vcdInstanceGroup) doDelete(intentID, href string) {
	startTime := time.Now()

	err := r.ig.deleteInstance(href)
	duration := time.Since(startTime).Seconds()
	InstanceDeletionDuration.WithLabelValues(r.store.instanceGroupName).Observe(duration)

	if err != nil {
		InstancesFailedTotal.WithLabelValues(r.store.instanceGroupName, "delete").Inc()
		r.log.Error("instance deletion failed", "intentID", intentID, "href", href, "error", err)

		// Back to PendingDelete with retry backoff
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhasePendingDelete
			inst.DeleteStartedAt = nil
			inst.RetryCount++
			inst.LastError = err.Error()
			delay := retryDelay(inst.RetryCount)
			nextRetry := time.Now().Add(delay)
			inst.NextRetryAfter = &nextRetry
		})
		return
	}

	InstancesDeletedTotal.WithLabelValues(r.store.instanceGroupName).Inc()

	// Success
	r.store.UpdateInstance(intentID, func(inst *Instance) {
		inst.Phase = PhaseDeleted
		now := time.Now()
		inst.DeleteCompletedAt = &now
		inst.LastError = ""
		inst.NextRetryAfter = nil
	})

	r.log.Info("instance deleted successfully", "intentID", intentID, "href", href)
}

func (r *vcdInstanceGroup) gcCheck() {
	GCRunsTotal.WithLabelValues(r.store.instanceGroupName).Inc()
	startTime := time.Now()
	defer func() {
		GCDuration.WithLabelValues(r.store.instanceGroupName).Observe(time.Since(startTime).Seconds())
	}()

	candidates := r.store.GetRunningForGC(r.config.MaxInstanceAge)
	for _, inst := range candidates {
		r.log.Info("GC: marking instance for deletion",
			"intentID", inst.IntentID, "href", inst.ID, "name", inst.Name)
		r.store.MarkGC(inst.IntentID)
		GCInstancesCollectedTotal.WithLabelValues(r.store.instanceGroupName).Inc()
	}
}

// VCDInstanceGroup interface implementation

func (r *vcdInstanceGroup) Increase(n int) []string {
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		intentID, err := GenerateRandomStringVMNameSafe(16)
		if err != nil {
			r.log.Error("failed to generate intent ID", "error", err)
			continue
		}
		r.store.AddCreateIntent(intentID)
		ids[i] = intentID
		r.log.Info("added create intent", "intentID", intentID)
	}
	r.trigger()
	return ids
}

func (r *vcdInstanceGroup) Decrease(instanceIDs []string) {
	for _, id := range instanceIDs {
		r.store.MarkForDeletion(id)
		r.log.Info("marked instance for deletion", "id", id)
	}
	r.trigger()
}

func (r *vcdInstanceGroup) Instances() []Instance {
	return r.store.GetAll()
}

func (r *vcdInstanceGroup) Instance(id string) (Instance, bool) {
	// Try VApp HREF first (most common lookup from fleeting)
	if inst, ok := r.store.GetByVAppHREF(id); ok {
		return inst, true
	}
	// Fall back to intent ID
	return r.store.GetByIntentID(id)
}

func (r *vcdInstanceGroup) Shutdown(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		r.log.Info("shutting down reconciler")

		// Stop the reconciliation loop
		close(r.stopCh)

		// Wait for the loop to exit
		select {
		case <-r.doneCh:
		case <-ctx.Done():
			r.shutdownErr = ctx.Err()
			return
		}

		// Wait for all in-flight creates and deletes to finish
		r.log.Info("waiting for in-flight operations to complete")
		if err := r.createSem.Acquire(ctx, int64(r.config.MaxConcurrentCreates)); err != nil {
			r.shutdownErr = fmt.Errorf("waiting for creates: %w", err)
			return
		}
		if err := r.deleteSem.Acquire(ctx, int64(r.config.MaxConcurrentDeletes)); err != nil {
			r.shutdownErr = fmt.Errorf("waiting for deletes: %w", err)
			return
		}

		// Delete all remaining instances
		r.log.Info("cleaning up all instances")
		instances := r.store.GetAll()
		for _, inst := range instances {
			if inst.ID == "" || inst.Phase == PhaseDeleted {
				continue
			}
			if !strings.HasPrefix(inst.Name, r.ig.VAppNamePrefix) {
				r.log.Warn("skipping cleanup of instance without matching prefix",
					"name", inst.Name, "prefix", r.ig.VAppNamePrefix)
				continue
			}
			r.log.Info("shutdown: deleting instance", "href", inst.ID, "name", inst.Name)
			if err := r.ig.deleteInstance(inst.ID); err != nil {
				r.log.Error("error deleting instance during shutdown",
					"href", inst.ID, "name", inst.Name, "error", err)
			}
		}
	})

	return r.shutdownErr
}

// retryDelay calculates the backoff delay for a given retry count.
// 10s base, 2x factor, 5min cap, 25% jitter.
func retryDelay(retryCount int) time.Duration {
	delay := retryBaseDelay * time.Duration(math.Pow(retryMultiplier, float64(retryCount-1)))
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}
	// Add jitter: ±25%
	jitter := float64(delay) * retryJitter * (2*rand.Float64() - 1)
	delay += time.Duration(jitter)
	if delay < 0 {
		delay = retryBaseDelay
	}
	return delay
}
