package vcd

import (
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

const (
	pruneAfter = 5 * time.Minute

	// missedPollsBeforeDisappeared is the number of consecutive polls an instance
	// must be missing from VCD before it's marked as Deleted. VCD search results
	// are eventually consistent and can temporarily omit instances.
	missedPollsBeforeDisappeared = 3
)

// desiredStateStore is a thread-safe cache of desired + observed instance state.
// It maintains two indexes for fast lookup: by intent ID and by VApp HREF.
// All reads from outside the reconciler hit this cache only — never VCD.
type desiredStateStore struct {
	mu                sync.RWMutex
	instances         map[string]*Instance // keyed by IntentID
	byVAppHREF        map[string]*Instance // secondary index by VApp HREF
	log               hclog.Logger
	instanceGroupName string
}

func newDesiredStateStore(log hclog.Logger, instanceGroupName string) *desiredStateStore {
	return &desiredStateStore{
		instances:         make(map[string]*Instance),
		byVAppHREF:        make(map[string]*Instance),
		log:               log,
		instanceGroupName: instanceGroupName,
	}
}

// AddCreateIntent adds a new instance with PhasePendingCreate.
// Returns the intent ID.
func (s *desiredStateStore) AddCreateIntent(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	inst := &Instance{
		IntentID:  intentID,
		Phase:     PhasePendingCreate,
		CreatedAt: &now,
	}
	s.instances[intentID] = inst
}

// MarkForDeletion transitions a Running instance to PhasePendingDelete.
// The instanceID can be either an IntentID or a VApp HREF.
func (s *desiredStateStore) MarkForDeletion(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.findLocked(instanceID)
	if inst == nil {
		s.log.Warn("MarkForDeletion: instance not found", "id", instanceID)
		return
	}

	if inst.Phase != PhaseRunning {
		s.log.Warn("MarkForDeletion: instance not in Running phase",
			"id", instanceID, "phase", inst.Phase)
		return
	}

	now := time.Now()
	inst.Phase = PhasePendingDelete
	inst.DeleteRequestedAt = &now
	inst.LastUpdated = &now
}

// GetByVAppHREF returns an instance by its VApp HREF.
func (s *desiredStateStore) GetByVAppHREF(href string) (Instance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inst, ok := s.byVAppHREF[href]
	if !ok {
		return Instance{}, false
	}
	return *inst, true
}

// GetByIntentID returns an instance by its intent ID.
func (s *desiredStateStore) GetByIntentID(intentID string) (Instance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inst, ok := s.instances[intentID]
	if !ok {
		return Instance{}, false
	}
	return *inst, true
}

// GetAll returns a copy of all tracked instances.
func (s *desiredStateStore) GetAll() []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		result = append(result, *inst)
	}
	return result
}

// UpdateInstance applies a mutation function to an instance identified by intentID.
func (s *desiredStateStore) UpdateInstance(intentID string, fn func(inst *Instance)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.instances[intentID]
	if !ok {
		s.log.Warn("UpdateInstance: intent not found", "intentID", intentID)
		return
	}

	fn(inst)

	now := time.Now()
	inst.LastUpdated = &now

	// Update HREF index if HREF is now known
	if inst.ID != "" {
		// If pollVCD raced and added a preexisting duplicate with this HREF
		// before we set it, remove the duplicate from the instances map.
		if existing, ok := s.byVAppHREF[inst.ID]; ok && existing != inst {
			s.log.Debug("removing preexisting duplicate after HREF assignment",
				"intentID", intentID, "duplicateIntentID", existing.IntentID, "href", inst.ID)
			delete(s.instances, existing.IntentID)
		}
		s.byVAppHREF[inst.ID] = inst
	}
}

// AddPreexisting adds an instance that was discovered in VCD but not in the store.
// These instances have no CreatedAt (they were not created by us).
func (s *desiredStateStore) AddPreexisting(href, vappName, vmName, ipAddress, vappStatus, vmStatus, osType string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already tracked
	if _, ok := s.byVAppHREF[href]; ok {
		return
	}

	now := time.Now()
	intentID := href // use HREF as intent ID for preexisting
	inst := &Instance{
		ID:         href,
		IntentID:   intentID,
		Name:       vappName,
		VMName:     vmName,
		Phase:      PhaseRunning,
		IPAddress:  ipAddress,
		VAppStatus: vappStatus,
		VMStatus:   vmStatus,
		OSType:     osType,
		// CreatedAt is nil — this is how we identify preexisting instances
		LastUpdated: &now,
	}
	s.instances[intentID] = inst
	s.byVAppHREF[href] = inst
}

// UpdateFromVCD updates an instance's observed state from VCD polling data.
// Returns false if the instance is not tracked.
func (s *desiredStateStore) UpdateFromVCD(href, vappName, vmName, ipAddress, vappStatus, vmStatus, osType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.byVAppHREF[href]
	if !ok {
		return false
	}

	now := time.Now()
	inst.Name = vappName
	inst.VMName = vmName
	inst.IPAddress = ipAddress
	inst.VAppStatus = vappStatus
	inst.VMStatus = vmStatus
	inst.OSType = osType
	inst.MissedPolls = 0 // reset missed polls counter on successful observation
	inst.LastUpdated = &now

	return true
}

// MarkDisappeared increments the missed polls counter for instances not found in VCD.
// After missedPollsBeforeDisappeared consecutive misses, the instance is marked as Deleted.
// knownHREFs is the set of HREFs observed in the current VCD poll.
func (s *desiredStateStore) MarkDisappeared(knownHREFs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, inst := range s.instances {
		if inst.ID == "" {
			continue // not yet created in VCD
		}
		if inst.Phase == PhaseDeleted {
			continue // already deleted
		}
		if inst.Phase == PhasePendingCreate || inst.Phase == PhaseCreating {
			continue // still being created
		}
		if knownHREFs[inst.ID] {
			continue // found in VCD, all good
		}

		inst.MissedPolls++
		if inst.MissedPolls >= missedPollsBeforeDisappeared {
			s.log.Info("instance disappeared from VCD, marking as deleted",
				"intentID", inst.IntentID, "href", inst.ID, "name", inst.Name,
				"missedPolls", inst.MissedPolls)
			inst.Phase = PhaseDeleted
			inst.DeleteCompletedAt = &now
			inst.LastUpdated = &now
		} else {
			s.log.Warn("instance not found in VCD poll, will retry",
				"intentID", inst.IntentID, "href", inst.ID, "name", inst.Name,
				"missedPolls", inst.MissedPolls, "threshold", missedPollsBeforeDisappeared)
		}
	}
}

// Prune removes PhaseDeleted instances that have been deleted for longer than pruneAfter.
func (s *desiredStateStore) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-pruneAfter)
	for intentID, inst := range s.instances {
		if inst.Phase == PhaseDeleted && inst.DeleteCompletedAt != nil && inst.DeleteCompletedAt.Before(cutoff) {
			s.log.Debug("pruning deleted instance", "intentID", intentID, "href", inst.ID)
			delete(s.byVAppHREF, inst.ID)
			delete(s.instances, intentID)
		}
	}
}

// GetPendingCreates returns instances in PhasePendingCreate whose backoff has elapsed.
func (s *desiredStateStore) GetPendingCreates() []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var result []Instance
	for _, inst := range s.instances {
		if inst.Phase == PhasePendingCreate {
			if inst.NextRetryAfter != nil && now.Before(*inst.NextRetryAfter) {
				continue // backoff not elapsed
			}
			result = append(result, *inst)
		}
	}
	return result
}

// GetPendingDeletes returns instances in PhasePendingDelete whose backoff has elapsed.
func (s *desiredStateStore) GetPendingDeletes() []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var result []Instance
	for _, inst := range s.instances {
		if inst.Phase == PhasePendingDelete {
			if inst.NextRetryAfter != nil && now.Before(*inst.NextRetryAfter) {
				continue
			}
			result = append(result, *inst)
		}
	}
	return result
}

// GetRunningForGC returns Running instances whose VCD DateCreated is older than maxAge.
func (s *desiredStateStore) GetRunningForGC(maxAge time.Duration) []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-maxAge)
	var result []Instance
	for _, inst := range s.instances {
		if inst.Phase == PhaseRunning && inst.CreateCompletedAt != nil && inst.CreateCompletedAt.Before(cutoff) {
			result = append(result, *inst)
		}
	}
	return result
}

// MarkGC marks a Running instance for garbage collection.
func (s *desiredStateStore) MarkGC(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.instances[intentID]
	if !ok {
		return
	}
	if inst.Phase != PhaseRunning {
		return
	}

	now := time.Now()
	inst.Phase = PhasePendingDelete
	inst.GCMarkedAt = &now
	inst.DeleteRequestedAt = &now
	inst.LastUpdated = &now
}

// findLocked finds an instance by either IntentID or VApp HREF. Must be called with lock held.
func (s *desiredStateStore) findLocked(id string) *Instance {
	if inst, ok := s.instances[id]; ok {
		return inst
	}
	if inst, ok := s.byVAppHREF[id]; ok {
		return inst
	}
	return nil
}

// UpdateMetrics updates Prometheus gauges for the store.
func (s *desiredStateStore) UpdateMetrics() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := map[string]int{
		"PendingCreate": 0,
		"Creating":      0,
		"Running":       0,
		"PendingDelete": 0,
		"Deleting":      0,
		"Deleted":       0,
	}
	total := 0

	for _, inst := range s.instances {
		total++
		counts[inst.Phase.String()]++
	}

	StateManagerInstancesTotal.WithLabelValues(s.instanceGroupName).Set(float64(total))

	// Map to fleeting-compatible state names for backward compatibility
	fleetingStateMap := map[string]string{
		"PendingCreate": string(provider.StateCreating),
		"Creating":      string(provider.StateCreating),
		"Running":       string(provider.StateRunning),
		"PendingDelete": string(provider.StateDeleting),
		"Deleting":      string(provider.StateDeleting),
		"Deleted":       string(provider.StateDeleted),
	}

	fleetingCounts := map[string]int{}
	for phase, count := range counts {
		fleetingState := fleetingStateMap[phase]
		fleetingCounts[fleetingState] += count
	}
	for state, count := range fleetingCounts {
		StateManagerInstancesByState.WithLabelValues(s.instanceGroupName, state).Set(float64(count))
	}
}

// HasCreating returns true if any instance is in PhaseCreating.
// Used by pollVCD to avoid adding preexisting duplicates during in-flight creates.
func (s *desiredStateStore) HasCreating() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, inst := range s.instances {
		if inst.Phase == PhaseCreating {
			return true
		}
	}
	return false
}

// Size returns the number of instances that are not deleted.
func (s *desiredStateStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, inst := range s.instances {
		if inst.Phase != PhaseDeleted {
			count++
		}
	}
	return count
}
