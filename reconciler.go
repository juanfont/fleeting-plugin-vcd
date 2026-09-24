package vcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/go-hclog"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
)

const (
	defaultReconcileInterval       = 10 * time.Second
	defaultMaxConcurrentCreates    = 3
	defaultMaxConcurrentDeletes    = 5
	defaultMaxConcurrentOperations = 4
	defaultMaxInstanceAge          = 24 * time.Hour
	gcCheckEveryN                  = 360 // ~1h at 10s interval

	// Retry backoff for per-intent retries
	retryBaseDelay  = 10 * time.Second
	retryMaxDelay   = 5 * time.Minute
	retryMultiplier = 2.0
	retryJitter     = 0.25

	// Circuit breaker: stop creating after N consecutive failures. Cooldowns grow
	// exponentially across consecutive trips and reset after a successful create.
	circuitBreakerThreshold   = 3
	circuitBreakerCooldown    = 5 * time.Minute
	circuitBreakerMaxCooldown = 30 * time.Minute
)

// ReconcilerConfig holds configuration for the reconciliation loop.
type ReconcilerConfig struct {
	MaxConcurrentCreates    int
	MaxConcurrentDeletes    int
	MaxConcurrentOperations int
	Interval                time.Duration
	MaxInstanceAge          time.Duration
}

func (c *ReconcilerConfig) withDefaults() ReconcilerConfig {
	out := *c
	if out.MaxConcurrentCreates <= 0 {
		out.MaxConcurrentCreates = defaultMaxConcurrentCreates
	}
	if out.MaxConcurrentDeletes <= 0 {
		out.MaxConcurrentDeletes = defaultMaxConcurrentDeletes
	}
	if out.MaxConcurrentOperations <= 0 {
		out.MaxConcurrentOperations = defaultMaxConcurrentOperations
	}
	if out.Interval <= 0 {
		out.Interval = defaultReconcileInterval
	}
	if out.MaxInstanceAge <= 0 {
		out.MaxInstanceAge = defaultMaxInstanceAge
	}
	return out
}

// vcdOps is the vCD work the reconciler performs. *InstanceGroup implements it;
// unit tests substitute a fake.
type vcdOps interface {
	getInstancesInInstanceGroup() ([]*govcd.VApp, error)
	pollVM(vmHREF string) (*vmPollResult, error)
	createInstance() (*createResult, error)
	deleteInstance(href string) error
	cleanUpInstanceByName(name string) error
	ownsVApp(name string) bool
	createRecorded(name string)
}

// vcdInstanceGroup implements VCDInstanceGroup using a reconciliation loop.
type vcdInstanceGroup struct {
	log    hclog.Logger
	store  *desiredStateStore
	config ReconcilerConfig
	ig     *InstanceGroup // reference to the provider for VCD operations

	ops      vcdOps
	limiter  *opLimiter
	throttle *throttleGate

	// backgroundDeletes tracks failed-create cleanups in flight to avoid
	// starting duplicates on every cycle.
	backgroundDeletes sync.Map

	// Circuit breaker: stop dispatching creates after consecutive failures.
	cbMu                      sync.Mutex
	consecutiveCreateFailures int
	circuitBreakerUntil       time.Time
	cbLoggedActive            bool // avoid log spam: only log once when active
	cbBackoff                 *backoff.ExponentialBackOff

	triggerCh    chan struct{}
	stopCh       chan struct{}
	doneCh       chan struct{}
	shutdownOnce sync.Once
	shutdownErr  error
}

func newVCDInstanceGroup(log hclog.Logger, store *desiredStateStore, ig *InstanceGroup, config ReconcilerConfig) *vcdInstanceGroup {
	config = config.withDefaults()
	throttle := newThrottleGate(log, store.instanceGroupName)

	r := &vcdInstanceGroup{
		log:       log,
		store:     store,
		config:    config,
		ig:        ig,
		limiter:   newOpLimiter(config.MaxConcurrentOperations, config.MaxConcurrentCreates, config.MaxConcurrentDeletes, throttle),
		throttle:  throttle,
		cbBackoff: newCircuitBreakerBackOff(),
		triggerCh: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
	if ig != nil {
		// vCD calls report throttling to the same gate that pauses dispatch.
		ig.throttle = throttle
		r.ops = ig
	}
	return r
}

func newCircuitBreakerBackOff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(circuitBreakerCooldown),
		backoff.WithMultiplier(2),
		backoff.WithMaxInterval(circuitBreakerMaxCooldown),
		backoff.WithRandomizationFactor(0.25),
		backoff.WithMaxElapsedTime(0),
	)
}

// recordCreateFailure counts a failed create and trips the breaker at the
// threshold. Failures while the breaker is active come from creates dispatched
// before the trip and do not lengthen the cooldown.
func (r *vcdInstanceGroup) recordCreateFailure() {
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.consecutiveCreateFailures++
	if r.consecutiveCreateFailures < circuitBreakerThreshold || time.Now().Before(r.circuitBreakerUntil) {
		return
	}
	cooldown := r.cbBackoff.NextBackOff()
	r.circuitBreakerUntil = time.Now().Add(cooldown)
	r.cbLoggedActive = false // allow one "active" log after trip
	r.log.Warn("circuit breaker tripped, pausing creates",
		"consecutive_failures", r.consecutiveCreateFailures,
		"cooldown", cooldown)
}

func (r *vcdInstanceGroup) recordCreateSuccess() {
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	if r.consecutiveCreateFailures > 0 {
		r.log.Info("circuit breaker reset, create succeeded after failures",
			"previous_failures", r.consecutiveCreateFailures)
	}
	r.consecutiveCreateFailures = 0
	r.cbBackoff.Reset()
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
		LastReconcileTimestamp.WithLabelValues(r.store.instanceGroupName).SetToCurrentTime()
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
	r.dispatchCreateCleanups()

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
	vApps, err := r.ops.getInstancesInInstanceGroup()
	if err != nil {
		r.log.Error("error polling VCD", "error", err)
		return
	}

	knownHREFs := make(map[string]bool)

	for _, vapp := range vApps {
		href := vapp.VApp.HREF
		name := vapp.VApp.Name
		knownHREFs[href] = true // Always track to prevent false disappearance
		vappStatus := types.VAppStatuses[vapp.VApp.Status]

		inst, tracked := r.store.GetByVAppHREF(href)
		if !tracked {
			// A create in flight links its own vApp once it stores the HREF.
			if r.ops.ownsVApp(name) {
				continue
			}
			if r.store.AddLeftover(href, name) {
				LeftoverVAppsQueuedTotal.WithLabelValues(r.store.instanceGroupName).Inc()
				r.log.Warn("queueing leftover vApp for deletion", "href", href, "vapp", name)
			}
			continue
		}

		hasVM := vapp.VApp.Children != nil && len(vapp.VApp.Children.VM) > 0
		if inst.Leftover || !hasVM {
			// Leftovers only wait for deletion; skip the per-VM read.
			r.store.UpdateFromVCD(href, name, "", "", vappStatus, "", "")
			continue
		}

		vm := vapp.VApp.Children.VM[0]
		var ipAddress, osType string
		if pollResult, err := r.ops.pollVM(vm.HREF); err == nil && pollResult != nil {
			ipAddress = pollResult.IP
			osType = pollResult.OSType
		}
		r.store.UpdateFromVCD(href, name, vm.Name, ipAddress, vappStatus, types.VAppStatuses[vm.Status], osType)
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
	r.cbMu.Lock()
	if r.consecutiveCreateFailures >= circuitBreakerThreshold && time.Now().Before(r.circuitBreakerUntil) {
		if !r.cbLoggedActive {
			r.log.Warn("circuit breaker active, skipping creates",
				"consecutive_failures", r.consecutiveCreateFailures,
				"resume_at", r.circuitBreakerUntil.Format(time.RFC3339))
			r.cbLoggedActive = true
		}
		r.cbMu.Unlock()
		return
	}
	r.cbMu.Unlock()

	pending := r.store.GetPendingCreates()
	for _, inst := range pending {
		intentID := inst.IntentID
		if !r.limiter.tryAcquire(opCreate) {
			break // operation limit reached or throttled
		}

		// Mark as creating
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhaseCreating
			now := time.Now()
			inst.CreateStartedAt = &now
		})

		go func() {
			defer r.limiter.release(opCreate)
			r.doCreate(intentID)
		}()
	}
}

func (r *vcdInstanceGroup) doCreate(intentID string) {
	startTime := time.Now()

	result, err := r.ops.createInstance()
	duration := time.Since(startTime).Seconds()
	InstanceCreationDuration.WithLabelValues(r.store.instanceGroupName).Observe(duration)

	if err != nil {
		InstancesFailedTotal.WithLabelValues(r.store.instanceGroupName, "create").Inc()
		r.log.Error("instance creation failed", "intentID", intentID, "error", err)

		r.recordCreateFailure()

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

	r.recordCreateSuccess()
	r.throttle.succeeded()

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

	// The store now knows the HREF, so discovery links the vApp to this intent.
	r.ops.createRecorded(result.VAppName)

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

		if !r.limiter.tryAcquire(opDelete) {
			break // operation limit reached or throttled
		}

		// Mark as deleting
		r.store.UpdateInstance(intentID, func(inst *Instance) {
			inst.Phase = PhaseDeleting
			now := time.Now()
			inst.DeleteStartedAt = &now
		})

		go func() {
			defer r.limiter.release(opDelete)
			r.doDelete(intentID, href)
		}()
	}
}

// Failed creates may not have received metadata yet, so discovery cannot be
// relied on for cleanup. Retain their names until deletion is confirmed.
func (r *vcdInstanceGroup) dispatchCreateCleanups() {
	if r.ig == nil {
		return
	}
	r.ig.failedCreateCleanups.Range(func(key, value any) bool {
		name := key.(string)
		requestedAt := value.(time.Time)
		cleanupKey := "create:" + name
		if _, loaded := r.backgroundDeletes.LoadOrStore(cleanupKey, true); loaded {
			return true
		}
		if !r.limiter.tryAcquire(opDelete) {
			r.backgroundDeletes.Delete(cleanupKey)
			return false
		}
		go func() {
			defer r.limiter.release(opDelete)
			defer r.backgroundDeletes.Delete(cleanupKey)
			defer func() {
				if rec := recover(); rec != nil {
					r.log.Error("panic cleaning up failed create", "name", name, "panic", rec)
				}
			}()
			err := r.ops.cleanUpInstanceByName(name)
			if err == nil {
				r.throttle.succeeded()
			}
			// Allow time for a POST whose response was lost to become visible.
			if err == nil || (errors.Is(err, govcd.ErrorEntityNotFound) && time.Since(requestedAt) >= safeCallMaxElapsedTime) {
				r.ig.failedCreateCleanups.Delete(name)
			} else {
				r.log.Warn("failed create cleanup will be retried", "name", name, "error", err)
			}
		}()
		return true
	})
}

func (r *vcdInstanceGroup) doDelete(intentID, href string) {
	startTime := time.Now()

	err := r.ops.deleteInstance(href)
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
	r.throttle.succeeded()

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
		if err := r.limiter.drain(ctx); err != nil {
			r.shutdownErr = fmt.Errorf("waiting for in-flight operations: %w", err)
			return
		}

		// Delete all remaining instances. vCD calls may wait out the org
		// operation limit indefinitely, so each one is bounded by ctx.
		r.ig.failedCreateCleanups.Range(func(key, value any) bool {
			err := withinContext(ctx, func() error { return r.ops.cleanUpInstanceByName(key.(string)) })
			if err != nil && !errors.Is(err, govcd.ErrorEntityNotFound) {
				r.shutdownErr = errors.Join(r.shutdownErr, fmt.Errorf("cleanup failed create %s: %w", key, err))
			}
			return ctx.Err() == nil
		})
		r.log.Info("cleaning up all instances")
		instances := r.store.GetAll()
		for _, inst := range instances {
			if ctx.Err() != nil {
				break
			}
			if inst.ID == "" || inst.Phase == PhaseDeleted {
				continue
			}
			if !strings.HasPrefix(inst.Name, r.ig.VAppNamePrefix) {
				r.log.Warn("skipping cleanup of instance without matching prefix",
					"name", inst.Name, "prefix", r.ig.VAppNamePrefix)
				continue
			}
			r.log.Info("shutdown: deleting instance", "href", inst.ID, "name", inst.Name)
			if err := withinContext(ctx, func() error { return r.ops.deleteInstance(inst.ID) }); err != nil {
				r.log.Error("error deleting instance during shutdown",
					"href", inst.ID, "name", inst.Name, "error", err)
			}
		}
		if err := ctx.Err(); err != nil {
			r.shutdownErr = errors.Join(r.shutdownErr, fmt.Errorf("cleanup interrupted: %w", err))
		}
	})

	return r.shutdownErr
}

// withinContext runs fn but stops waiting for it when ctx ends. The vCD SDK
// takes no context, so an abandoned call finishes in the background.
func withinContext(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelay returns the backoff before retry number retryCount of an instance:
// 10s base, 2x factor, 5min cap, 25% jitter.
func retryDelay(retryCount int) time.Duration {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(retryBaseDelay),
		backoff.WithMultiplier(retryMultiplier),
		backoff.WithMaxInterval(retryMaxDelay),
		backoff.WithRandomizationFactor(retryJitter),
		backoff.WithMaxElapsedTime(0),
	)
	delay := b.NextBackOff()
	for i := 1; i < retryCount; i++ {
		delay = b.NextBackOff()
	}
	return delay
}
