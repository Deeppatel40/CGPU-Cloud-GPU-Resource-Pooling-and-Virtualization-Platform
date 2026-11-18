package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// stateVersion guards against loading a file written by an incompatible scheduler build.
const stateVersion = 1

// persistedState is the on-disk snapshot of everything the engine cannot rediscover:
// which job holds VRAM on which GPU, and which jobs are waiting. Node inventory is not
// saved; it comes from bootstrap/registration on every start.
type persistedState struct {
	Version     int                    `json:"version"`
	SavedAt     time.Time              `json:"saved_at"`
	Allocations []persistedAllocation  `json:"allocations"`
	Pending     []*v1.PlacementRequest `json:"pending"`
}

type persistedAllocation struct {
	NodeID     string                `json:"node_id"`
	GPUID      string                `json:"gpu_id"`
	Allocation v1.ActiveAllocation   `json:"allocation"`
	Decision   *v1.PlacementDecision `json:"decision"`
}

// EnableStateFile loads any existing snapshot from path, then writes the snapshot back after
// every state change. Call it after all nodes are registered: allocations that reference a
// node or GPU no longer in the inventory are dropped (and logged) on load.
//
// A missing file is not an error (first start). A corrupt file is: the caller should stop
// rather than silently start with an empty pool while jobs are still running on the GPUs.
func (pe *PlacementEngine) EnableStateFile(path string) error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// fresh start
	case err != nil:
		return fmt.Errorf("read state file: %w", err)
	default:
		var st persistedState
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("parse state file %s: %w", path, err)
		}
		if st.Version != stateVersion {
			return fmt.Errorf("state file %s has version %d, want %d", path, st.Version, stateVersion)
		}
		pe.restoreLocked(&st)
	}

	pe.statePath = path
	pe.persistLocked()
	return pe.persistErr
}

func (pe *PlacementEngine) restoreLocked(st *persistedState) {
	for _, pa := range st.Allocations {
		node, ok := pe.nodes[pa.NodeID]
		if !ok {
			log.Printf("[CGPU-Scheduler] state: dropping allocation %s: node %s not registered", pa.Allocation.JobID, pa.NodeID)
			continue
		}
		var gpu *v1.GPUDevice
		for i := range node.GPUs {
			if node.GPUs[i].GPUID == pa.GPUID {
				gpu = &node.GPUs[i]
			}
		}
		if gpu == nil {
			log.Printf("[CGPU-Scheduler] state: dropping allocation %s: GPU %s not on node %s", pa.Allocation.JobID, pa.GPUID, pa.NodeID)
			continue
		}
		if _, dup := pe.allocations[pa.Allocation.JobID]; dup {
			continue
		}
		pe.applyAllocationLocked(pa.NodeID, gpu, pa.Allocation)
		if pa.Decision != nil {
			d := *pa.Decision
			pe.decisions[pa.Allocation.JobID] = &d
		}
	}

	for _, req := range st.Pending {
		if req == nil || req.JobID == "" {
			continue
		}
		if _, held := pe.allocations[req.JobID]; held {
			continue
		}
		pe.pendingQueue = append(pe.pendingQueue, req)
		pe.decisions[req.JobID] = &v1.PlacementDecision{
			JobID:             req.JobID,
			Status:            v1.DecisionPending,
			PendingReason:     v1.ReasonWaitingForCapacity,
			DecisionTimestamp: req.SubmittedAt,
		}
	}
	sortQueue(pe.pendingQueue)
	log.Printf("[CGPU-Scheduler] state: restored %d allocation(s) and %d queued job(s)", len(pe.allocations), len(pe.pendingQueue))
}

// persistLocked writes the snapshot atomically (temp file + rename) so a crash mid-write
// never leaves a truncated file. Failures are logged and kept in persistErr rather than
// failing the scheduling call: the in-memory decision is already made and correct.
func (pe *PlacementEngine) persistLocked() {
	if pe.statePath == "" {
		return
	}
	st := persistedState{
		Version:     stateVersion,
		SavedAt:     time.Now().UTC(),
		Allocations: make([]persistedAllocation, 0, len(pe.allocations)),
		Pending:     pe.pendingQueue,
	}
	for id, a := range pe.allocations {
		loc := pe.jobLocations[id]
		st.Allocations = append(st.Allocations, persistedAllocation{
			NodeID: loc.nodeID, GPUID: loc.gpuID, Allocation: *a, Decision: pe.decisions[id],
		})
	}

	err := writeFileAtomic(pe.statePath, st)
	if err != nil && pe.persistErr == nil {
		log.Printf("[CGPU-Scheduler] state: failed to save %s: %v", pe.statePath, err)
	}
	pe.persistErr = err
}

// PersistError reports the most recent state-file write error, or nil.
func (pe *PlacementEngine) PersistError() error {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return pe.persistErr
}

func writeFileAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cgpu-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
