package vcd

import (
	"context"
	"time"
)

// Phase represents the lifecycle phase of an instance.
type Phase int

const (
	PhasePendingCreate Phase = iota
	PhaseCreating
	PhaseRunning
	PhasePendingDelete
	PhaseDeleting
	PhaseDeleted
)

func (p Phase) String() string {
	switch p {
	case PhasePendingCreate:
		return "PendingCreate"
	case PhaseCreating:
		return "Creating"
	case PhaseRunning:
		return "Running"
	case PhasePendingDelete:
		return "PendingDelete"
	case PhaseDeleting:
		return "Deleting"
	case PhaseDeleted:
		return "Deleted"
	default:
		return "Unknown"
	}
}

// Instance represents a tracked VCD instance with its full lifecycle state.
type Instance struct {
	ID       string // VApp HREF (empty until VApp created)
	IntentID string // UUID, assigned at Increase time
	Name     string // VApp name
	VMName   string
	Phase    Phase
	IPAddress  string
	VAppStatus string
	VMStatus   string
	OSType     string // Cached OS type from VM spec (e.g. "windows9Server64Guest")

	// Timestamps
	CreatedAt         *time.Time
	CreateStartedAt   *time.Time
	CreateCompletedAt *time.Time
	DeleteRequestedAt *time.Time
	DeleteStartedAt   *time.Time
	DeleteCompletedAt *time.Time
	GCMarkedAt        *time.Time
	LastUpdated       *time.Time

	// Retry
	RetryCount     int
	LastError      string
	NextRetryAfter *time.Time

	// MissedPolls tracks how many consecutive polls this instance was not found in VCD.
	// Used to avoid marking instances as disappeared due to eventual consistency.
	MissedPolls int
}

// VCDInstanceGroup manages a group of VCD instances with reconciliation.
type VCDInstanceGroup interface {
	// Increase requests n new instances. Returns intent IDs.
	Increase(n int) []string

	// Decrease marks instances for deletion.
	Decrease(instanceIDs []string)

	// Instances returns all tracked instances from cache.
	Instances() []Instance

	// Instance returns a specific instance by VApp HREF from cache.
	Instance(id string) (Instance, bool)

	// Shutdown stops the reconciler, waits for in-flight ops, cleans up all instances.
	Shutdown(ctx context.Context) error
}
