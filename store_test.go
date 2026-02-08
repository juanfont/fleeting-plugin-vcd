package vcd

import (
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore() *desiredStateStore {
	return newDesiredStateStore(hclog.NewNullLogger(), "test-group")
}

func TestStore_AddCreateIntent(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")

	inst, ok := s.GetByIntentID("intent-1")
	require.True(t, ok)
	assert.Equal(t, "intent-1", inst.IntentID)
	assert.Equal(t, PhasePendingCreate, inst.Phase)
	assert.NotNil(t, inst.CreatedAt)
	assert.Empty(t, inst.ID) // no HREF yet
}

func TestStore_AddCreateIntent_Multiple(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	s.AddCreateIntent("b")
	s.AddCreateIntent("c")

	all := s.GetAll()
	assert.Len(t, all, 3)
	assert.Equal(t, 3, s.Size())
}

func TestStore_GetByIntentID_NotFound(t *testing.T) {
	s := newTestStore()

	_, ok := s.GetByIntentID("nonexistent")
	assert.False(t, ok)
}

func TestStore_GetByVAppHREF(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	// Simulate creation completing — set HREF via UpdateInstance
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = "https://vcd/vapp-1"
		inst.Phase = PhaseRunning
	})

	inst, ok := s.GetByVAppHREF("https://vcd/vapp-1")
	require.True(t, ok)
	assert.Equal(t, "intent-1", inst.IntentID)
	assert.Equal(t, PhaseRunning, inst.Phase)
}

func TestStore_GetByVAppHREF_NotFound(t *testing.T) {
	s := newTestStore()

	_, ok := s.GetByVAppHREF("https://vcd/nonexistent")
	assert.False(t, ok)
}

func TestStore_UpdateInstance(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseCreating
		inst.Name = "my-vapp"
	})

	inst, ok := s.GetByIntentID("intent-1")
	require.True(t, ok)
	assert.Equal(t, PhaseCreating, inst.Phase)
	assert.Equal(t, "my-vapp", inst.Name)
	assert.NotNil(t, inst.LastUpdated)
}

func TestStore_UpdateInstance_SetsHREFIndex(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")

	// HREF not set yet — should not be findable by HREF
	_, ok := s.GetByVAppHREF("https://vcd/vapp-1")
	assert.False(t, ok)

	// Set HREF
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = "https://vcd/vapp-1"
	})

	// Now findable by HREF
	inst, ok := s.GetByVAppHREF("https://vcd/vapp-1")
	require.True(t, ok)
	assert.Equal(t, "intent-1", inst.IntentID)
}

func TestStore_UpdateInstance_NonexistentNoOp(t *testing.T) {
	s := newTestStore()

	// Should not panic or crash
	s.UpdateInstance("nonexistent", func(inst *Instance) {
		inst.Phase = PhaseRunning
	})

	assert.Equal(t, 0, s.Size())
}

func TestStore_MarkForDeletion(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = "https://vcd/vapp-1"
		inst.Phase = PhaseRunning
	})

	// Mark by intent ID
	s.MarkForDeletion("intent-1")

	inst, ok := s.GetByIntentID("intent-1")
	require.True(t, ok)
	assert.Equal(t, PhasePendingDelete, inst.Phase)
	assert.NotNil(t, inst.DeleteRequestedAt)
}

func TestStore_MarkForDeletion_ByHREF(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = "https://vcd/vapp-1"
		inst.Phase = PhaseRunning
	})

	// Mark by VApp HREF
	s.MarkForDeletion("https://vcd/vapp-1")

	inst, _ := s.GetByIntentID("intent-1")
	assert.Equal(t, PhasePendingDelete, inst.Phase)
}

func TestStore_MarkForDeletion_NotRunning(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	// Still in PhasePendingCreate — should not be marked for deletion

	s.MarkForDeletion("intent-1")

	inst, _ := s.GetByIntentID("intent-1")
	assert.Equal(t, PhasePendingCreate, inst.Phase)
}

func TestStore_MarkForDeletion_NotFound(t *testing.T) {
	s := newTestStore()

	// Should not panic
	s.MarkForDeletion("nonexistent")
}

func TestStore_AddPreexisting(t *testing.T) {
	s := newTestStore()

	s.AddPreexisting("https://vcd/vapp-pre", "pre-vapp", "pre-vm", "10.0.0.1", "POWERED_ON", "POWERED_ON", "centos64Guest")

	inst, ok := s.GetByVAppHREF("https://vcd/vapp-pre")
	require.True(t, ok)
	assert.Equal(t, PhaseRunning, inst.Phase)
	assert.Nil(t, inst.CreatedAt) // preexisting
	assert.Equal(t, "pre-vapp", inst.Name)
	assert.Equal(t, "10.0.0.1", inst.IPAddress)
	assert.Equal(t, "centos64Guest", inst.OSType)

	// Also accessible by intent ID (which is the HREF for preexisting)
	inst2, ok := s.GetByIntentID("https://vcd/vapp-pre")
	require.True(t, ok)
	assert.Equal(t, inst.IntentID, inst2.IntentID)
}

func TestStore_AddPreexisting_Idempotent(t *testing.T) {
	s := newTestStore()

	s.AddPreexisting("https://vcd/vapp-1", "vapp", "vm", "10.0.0.1", "ON", "ON", "linux")
	s.AddPreexisting("https://vcd/vapp-1", "vapp-new", "vm-new", "10.0.0.2", "ON", "ON", "linux")

	// Should still be the first one — second call was a no-op
	all := s.GetAll()
	assert.Len(t, all, 1)
	assert.Equal(t, "vapp", all[0].Name)
}

func TestStore_UpdateFromVCD(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = "https://vcd/vapp-1"
		inst.Phase = PhaseRunning
	})

	updated := s.UpdateFromVCD("https://vcd/vapp-1", "new-name", "new-vm", "10.0.0.5", "POWERED_ON", "POWERED_ON", "centos")
	assert.True(t, updated)

	inst, _ := s.GetByVAppHREF("https://vcd/vapp-1")
	assert.Equal(t, "new-name", inst.Name)
	assert.Equal(t, "10.0.0.5", inst.IPAddress)
}

func TestStore_UpdateFromVCD_NotTracked(t *testing.T) {
	s := newTestStore()

	updated := s.UpdateFromVCD("https://vcd/unknown", "name", "vm", "10.0.0.1", "ON", "ON", "linux")
	assert.False(t, updated)
}

func TestStore_MarkDisappeared(t *testing.T) {
	s := newTestStore()

	// Add two running instances
	s.AddCreateIntent("a")
	s.UpdateInstance("a", func(inst *Instance) {
		inst.ID = "https://vcd/a"
		inst.Phase = PhaseRunning
	})
	s.AddCreateIntent("b")
	s.UpdateInstance("b", func(inst *Instance) {
		inst.ID = "https://vcd/b"
		inst.Phase = PhaseRunning
	})

	onlyA := map[string]bool{"https://vcd/a": true}

	// First missed poll — "b" should NOT be deleted yet
	s.MarkDisappeared(onlyA)
	instB, _ := s.GetByIntentID("b")
	assert.Equal(t, PhaseRunning, instB.Phase)
	assert.Equal(t, 1, instB.MissedPolls)

	// Second missed poll — still not deleted
	s.MarkDisappeared(onlyA)
	instB, _ = s.GetByIntentID("b")
	assert.Equal(t, PhaseRunning, instB.Phase)
	assert.Equal(t, 2, instB.MissedPolls)

	// Third missed poll — NOW it should be marked Deleted
	s.MarkDisappeared(onlyA)
	instB, _ = s.GetByIntentID("b")
	assert.Equal(t, PhaseDeleted, instB.Phase)
	assert.NotNil(t, instB.DeleteCompletedAt)

	// "a" should be unaffected
	instA, _ := s.GetByIntentID("a")
	assert.Equal(t, PhaseRunning, instA.Phase)
	assert.Equal(t, 0, instA.MissedPolls)
}

func TestStore_MarkDisappeared_ResetsOnReappearance(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	s.UpdateInstance("a", func(inst *Instance) {
		inst.ID = "https://vcd/a"
		inst.Phase = PhaseRunning
	})

	empty := map[string]bool{}
	withA := map[string]bool{"https://vcd/a": true}

	// Miss 2 polls
	s.MarkDisappeared(empty)
	s.MarkDisappeared(empty)
	inst, _ := s.GetByIntentID("a")
	assert.Equal(t, 2, inst.MissedPolls)

	// Reappear via UpdateFromVCD
	s.UpdateFromVCD("https://vcd/a", "vapp", "vm", "10.0.0.1", "ON", "ON", "linux")
	inst, _ = s.GetByIntentID("a")
	assert.Equal(t, 0, inst.MissedPolls)

	// Now MarkDisappeared again — counter restarted from 0
	s.MarkDisappeared(empty)
	inst, _ = s.GetByIntentID("a")
	assert.Equal(t, 1, inst.MissedPolls)
	assert.Equal(t, PhaseRunning, inst.Phase)

	// Reappear via MarkDisappeared with the HREF present
	s.MarkDisappeared(withA)
	// MissedPolls doesn't reset here — that's done by UpdateFromVCD
	// But the instance shouldn't increment either since it's in knownHREFs
	inst, _ = s.GetByIntentID("a")
	assert.Equal(t, 1, inst.MissedPolls) // unchanged — it was found this time
}

func TestStore_MarkDisappeared_SkipsCreating(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("creating")
	s.UpdateInstance("creating", func(inst *Instance) {
		inst.ID = "https://vcd/creating"
		inst.Phase = PhaseCreating
	})

	// Not in VCD but still Creating — should NOT be marked deleted
	s.MarkDisappeared(map[string]bool{})

	inst, _ := s.GetByIntentID("creating")
	assert.Equal(t, PhaseCreating, inst.Phase)
}

func TestStore_MarkDisappeared_SkipsNoHREF(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("pending")
	// No HREF set — should be skipped

	s.MarkDisappeared(map[string]bool{})

	inst, _ := s.GetByIntentID("pending")
	assert.Equal(t, PhasePendingCreate, inst.Phase)
}

func TestStore_Prune(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	// Mark as deleted with a timestamp in the past
	pastTime := time.Now().Add(-10 * time.Minute)
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseDeleted
		inst.DeleteCompletedAt = &pastTime
	})

	assert.Len(t, s.GetAll(), 1)

	s.Prune()

	assert.Len(t, s.GetAll(), 0)
}

func TestStore_Prune_KeepsRecent(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("intent-1")
	// Mark as deleted with a recent timestamp
	recentTime := time.Now().Add(-1 * time.Minute)
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseDeleted
		inst.DeleteCompletedAt = &recentTime
	})

	s.Prune()

	// Should still be there — too recent to prune
	assert.Len(t, s.GetAll(), 1)
}

func TestStore_Prune_KeepsNonDeleted(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("running")
	s.UpdateInstance("running", func(inst *Instance) {
		inst.Phase = PhaseRunning
	})

	s.Prune()

	assert.Len(t, s.GetAll(), 1)
}

func TestStore_GetPendingCreates(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	s.AddCreateIntent("b")
	s.AddCreateIntent("c")

	// "c" has a future retry — should be excluded
	future := time.Now().Add(1 * time.Hour)
	s.UpdateInstance("c", func(inst *Instance) {
		inst.NextRetryAfter = &future
	})

	// "b" transitions to Creating — should be excluded
	s.UpdateInstance("b", func(inst *Instance) {
		inst.Phase = PhaseCreating
	})

	pending := s.GetPendingCreates()
	assert.Len(t, pending, 1)
	assert.Equal(t, "a", pending[0].IntentID)
}

func TestStore_GetPendingCreates_BackoffElapsed(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	past := time.Now().Add(-1 * time.Minute)
	s.UpdateInstance("a", func(inst *Instance) {
		inst.NextRetryAfter = &past
	})

	pending := s.GetPendingCreates()
	assert.Len(t, pending, 1)
}

func TestStore_GetPendingDeletes(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	s.UpdateInstance("a", func(inst *Instance) {
		inst.ID = "https://vcd/a"
		inst.Phase = PhaseRunning
	})
	s.MarkForDeletion("a")

	s.AddCreateIntent("b")
	s.UpdateInstance("b", func(inst *Instance) {
		inst.ID = "https://vcd/b"
		inst.Phase = PhaseRunning
	})
	s.MarkForDeletion("b")

	// "b" has a future retry
	future := time.Now().Add(1 * time.Hour)
	s.UpdateInstance("b", func(inst *Instance) {
		inst.NextRetryAfter = &future
	})

	pending := s.GetPendingDeletes()
	assert.Len(t, pending, 1)
	assert.Equal(t, "a", pending[0].IntentID)
}

func TestStore_GetRunningForGC(t *testing.T) {
	s := newTestStore()

	// Old instance
	s.AddCreateIntent("old")
	oldTime := time.Now().Add(-25 * time.Hour)
	s.UpdateInstance("old", func(inst *Instance) {
		inst.ID = "https://vcd/old"
		inst.Phase = PhaseRunning
		inst.CreateCompletedAt = &oldTime
	})

	// Recent instance
	s.AddCreateIntent("recent")
	recentTime := time.Now().Add(-1 * time.Hour)
	s.UpdateInstance("recent", func(inst *Instance) {
		inst.ID = "https://vcd/recent"
		inst.Phase = PhaseRunning
		inst.CreateCompletedAt = &recentTime
	})

	candidates := s.GetRunningForGC(24 * time.Hour)
	assert.Len(t, candidates, 1)
	assert.Equal(t, "old", candidates[0].IntentID)
}

func TestStore_GetRunningForGC_SkipsNonRunning(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("deleting")
	oldTime := time.Now().Add(-25 * time.Hour)
	s.UpdateInstance("deleting", func(inst *Instance) {
		inst.Phase = PhaseDeleting
		inst.CreateCompletedAt = &oldTime
	})

	candidates := s.GetRunningForGC(24 * time.Hour)
	assert.Len(t, candidates, 0)
}

func TestStore_MarkGC(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	s.UpdateInstance("a", func(inst *Instance) {
		inst.ID = "https://vcd/a"
		inst.Phase = PhaseRunning
	})

	s.MarkGC("a")

	inst, _ := s.GetByIntentID("a")
	assert.Equal(t, PhasePendingDelete, inst.Phase)
	assert.NotNil(t, inst.GCMarkedAt)
	assert.NotNil(t, inst.DeleteRequestedAt)
}

func TestStore_MarkGC_NotRunning(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	// Still PendingCreate — MarkGC should be a no-op

	s.MarkGC("a")

	inst, _ := s.GetByIntentID("a")
	assert.Equal(t, PhasePendingCreate, inst.Phase)
}

func TestStore_Size(t *testing.T) {
	s := newTestStore()

	assert.Equal(t, 0, s.Size())

	s.AddCreateIntent("a")
	s.AddCreateIntent("b")
	assert.Equal(t, 2, s.Size())

	// Mark one as deleted — shouldn't count
	s.UpdateInstance("a", func(inst *Instance) {
		inst.Phase = PhaseDeleted
	})
	assert.Equal(t, 1, s.Size())
}

func TestStore_GetAll_ReturnsCopies(t *testing.T) {
	s := newTestStore()

	s.AddCreateIntent("a")
	all := s.GetAll()

	// Modifying the returned copy should not affect the store
	all[0].Name = "modified"

	inst, _ := s.GetByIntentID("a")
	assert.Empty(t, inst.Name)
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := newTestStore()

	var wg sync.WaitGroup
	n := 100

	// Concurrent writes
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := string(rune('A'+i%26)) + string(rune('0'+i/26))
			s.AddCreateIntent(id)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, n, s.Size())

	// Concurrent reads
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			s.GetAll()
			s.Size()
			s.GetPendingCreates()
		}()
	}
	wg.Wait()
}

func TestStore_HasCreating(t *testing.T) {
	s := newTestStore()

	assert.False(t, s.HasCreating())

	s.AddCreateIntent("a")
	assert.False(t, s.HasCreating()) // PendingCreate, not Creating

	s.UpdateInstance("a", func(inst *Instance) {
		inst.Phase = PhaseCreating
	})
	assert.True(t, s.HasCreating())

	s.UpdateInstance("a", func(inst *Instance) {
		inst.Phase = PhaseRunning
	})
	assert.False(t, s.HasCreating())
}

func TestStore_UpdateInstance_RemovesPreexistingDuplicate(t *testing.T) {
	s := newTestStore()

	// Simulate the race: doCreate is in-flight, pollVCD sees the VApp first
	s.AddCreateIntent("intent-1")

	// pollVCD adds the VApp as preexisting before doCreate finishes
	href := "https://vcd/vapp-1"
	s.AddPreexisting(href, "my-vapp", "my-vm", "10.0.0.1", "POWERED_ON", "POWERED_ON", "linux")

	// Now there are 2 instances: intent-1 and the preexisting (keyed by href)
	assert.Len(t, s.GetAll(), 2)

	// doCreate finishes and sets the HREF on the original intent
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.ID = href
		inst.Phase = PhaseRunning
		inst.Name = "my-vapp"
		inst.IPAddress = "10.0.0.1"
	})

	// The preexisting duplicate should have been removed
	all := s.GetAll()
	assert.Len(t, all, 1)
	assert.Equal(t, "intent-1", all[0].IntentID)

	// HREF index should point to the original intent
	inst, ok := s.GetByVAppHREF(href)
	require.True(t, ok)
	assert.Equal(t, "intent-1", inst.IntentID)
	assert.Equal(t, PhaseRunning, inst.Phase)
}

func TestStore_FullLifecycle(t *testing.T) {
	s := newTestStore()

	// 1. Add create intent
	s.AddCreateIntent("intent-1")
	inst, _ := s.GetByIntentID("intent-1")
	assert.Equal(t, PhasePendingCreate, inst.Phase)

	// 2. Mark as creating
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseCreating
	})
	inst, _ = s.GetByIntentID("intent-1")
	assert.Equal(t, PhaseCreating, inst.Phase)

	// 3. Mark as running with HREF
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseRunning
		inst.ID = "https://vcd/vapp-1"
		inst.Name = "my-vapp"
		inst.IPAddress = "10.0.0.1"
	})
	inst, _ = s.GetByVAppHREF("https://vcd/vapp-1")
	assert.Equal(t, PhaseRunning, inst.Phase)
	assert.Equal(t, "10.0.0.1", inst.IPAddress)

	// 4. Mark for deletion
	s.MarkForDeletion("https://vcd/vapp-1")
	inst, _ = s.GetByIntentID("intent-1")
	assert.Equal(t, PhasePendingDelete, inst.Phase)

	// 5. Mark as deleting
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseDeleting
	})
	inst, _ = s.GetByIntentID("intent-1")
	assert.Equal(t, PhaseDeleting, inst.Phase)

	// 6. Mark as deleted
	pastTime := time.Now().Add(-10 * time.Minute)
	s.UpdateInstance("intent-1", func(inst *Instance) {
		inst.Phase = PhaseDeleted
		inst.DeleteCompletedAt = &pastTime
	})
	assert.Equal(t, 0, s.Size()) // deleted doesn't count

	// 7. Prune
	s.Prune()
	assert.Len(t, s.GetAll(), 0)
}
