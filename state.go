package vcd

import (
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/puzpuzpuz/xsync/v4"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

type instanceStateManager struct {
	log   hclog.Logger
	state *xsync.Map[string, instanceData]
}

type instanceData struct {
	InstanceID string
	VAppName   string
	VMName     string
	VAppStatus string
	VMStatus   string
	IPAddress  string

	// This is our translation to the Fleeting state machine
	FleetingState provider.State

	// A basic state machine to track the lifecycle of the instance
	// Using *time.Time allows us to track when state transitions occur
	// nil means the state has never been reached
	CreatedAt   *time.Time
	BootingAt   *time.Time
	BootedAt    *time.Time
	DeletingAt  *time.Time // When deletion process starts
	DeletedAt   *time.Time // When deletion is complete (soft delete marker)
	LastUpdated *time.Time // When this instance data was last modified
}

func newInstanceStateManager(log hclog.Logger) *instanceStateManager {
	state := xsync.NewMap[string, instanceData]()
	return &instanceStateManager{
		log:   log,
		state: state,
	}
}

func (m *instanceStateManager) Get(id string) (bool, instanceData) {
	m.log.Info("[InstanceStateManager] getting instance data", "id", id)
	data, ok := m.state.Load(id)
	if !ok {
		return false, instanceData{}
	}

	// Don't return soft-deleted instances
	if data.DeletedAt != nil {
		m.log.Debug("[InstanceStateManager] instance is soft-deleted", "id", id, "deletedAt", data.DeletedAt)
		return false, instanceData{}
	}

	return true, data
}

func (m *instanceStateManager) Update(id string, fn func(instanceData) instanceData) instanceData {
	data, ok := m.state.Load(id)
	if !ok {
		m.log.Info("[InstanceStateManager] instance data not found", "id", id)
		data = instanceData{
			InstanceID: id,
		}
	}

	newData := fn(data)
	// Always set LastUpdated when we modify the data
	newData.LastUpdated = now()
	m.state.Store(id, newData)
	return newData
}

func (m *instanceStateManager) Prune(id string) {
	m.log.Info("[InstanceStateManager] pruning instance data", "id", id)
	m.state.Delete(id)
}

// GetAll returns all instances including soft-deleted ones (for debugging)
func (m *instanceStateManager) GetAll() []instanceData {
	var instances []instanceData
	m.state.Range(func(key string, value instanceData) bool {
		instances = append(instances, value)
		return true
	})
	return instances
}

func (m *instanceStateManager) GetFleetingState(id string) (bool, provider.State) {
	data, ok := m.state.Load(id)
	if !ok {
		return false, ""
	}

	var state provider.State
	if data.DeletedAt != nil {
		state = provider.StateDeleted
	} else if data.DeletingAt != nil {
		state = provider.StateDeleting
	} else if data.CreatedAt == nil {
		// FIXME(juan): Is this correct?
		state = provider.StateRunning
	} else if data.BootedAt == nil {
		state = provider.StateCreating
	} else if data.BootedAt != nil {
		state = provider.StateRunning
	} else {
		m.log.Warn("unexpected instance status", "id", id, "data", data)
		state = provider.StateTimeout
	}

	return true, state
}

// Helper function to get current time pointer
func now() *time.Time {
	t := time.Now()
	return &t
}
