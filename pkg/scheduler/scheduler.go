package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

var (
	ErrNilRequest       = errors.New("placement request cannot be nil")
	ErrInvalidVRAM      = errors.New("requested VRAM must be greater than 0")
	ErrJobNotFound      = errors.New("active allocation for job ID not found")
	ErrGPUNotFound      = errors.New("target GPU not found on specified node")
	ErrNodeNotFound     = errors.New("target node not found")
)

// Candidate represents an evaluated GPU match during binpacking.
type Candidate struct {
	NodeName          string
	KubernetesNode    string
	GPU               *v1.GPUDevice
	RemainingAfterMiB int
}

// PlacementEngine implements the v1.Scheduler interface with atomic binpacking and queueing.
type PlacementEngine struct {
	mu           sync.RWMutex
	nodes        map[string]*v1.GPUNode
	allocations  map[string]*v1.ActiveAllocation // jobID -> allocation
	jobLocations map[string]location             // jobID -> (nodeID, gpuID)
	pendingQueue []*v1.PlacementRequest
}

type location struct {
	nodeID string
	gpuID  string
}

// NewPlacementEngine constructs a thread-safe scheduler instance.
func NewPlacementEngine() *PlacementEngine {
	return &PlacementEngine{
		nodes:        make(map[string]*v1.GPUNode),
		allocations:  make(map[string]*v1.ActiveAllocation),
		jobLocations: make(map[string]location),
		pendingQueue: make([]*v1.PlacementRequest, 0),
	}
}

// RegisterOrUpdateNode registers or updates a GPU worker node in the scheduler inventory.
func (pe *PlacementEngine) RegisterOrUpdateNode(node *v1.GPUNode) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	// Clone node state
	cloned := *node
	cloned.GPUs = make([]v1.GPUDevice, len(node.GPUs))
	for i, g := range node.GPUs {
		cloned.GPUs[i] = g
		if cloned.GPUs[i].FreeVRAMMiB == 0 && cloned.GPUs[i].AllocatedVRAMMiB == 0 {
			cloned.GPUs[i].FreeVRAMMiB = cloned.GPUs[i].TotalVRAMMiB
		}
	}
	pe.nodes[node.NodeID] = &cloned
}

// Schedule attempts atomic placement of a job onto the best-fit GPU.
func (pe *PlacementEngine) Schedule(ctx context.Context, req *v1.PlacementRequest) (*v1.PlacementDecision, error) {
	if req == nil {
		return nil, ErrNilRequest
	}
	if req.RequestedVRAMMiB <= 0 {
		return nil, ErrInvalidVRAM
	}

	pe.mu.Lock()
	defer pe.mu.Unlock()

	now := time.Now().UTC()
	decision := &PlacementDecisionBuilder{
		JobID:             req.JobID,
		DecisionTimestamp: now,
	}

	// 1. Check max hardware capacity across pool
	maxSingleGPUVRAM := 0
	totalOnlineGPUs := 0
	for _, node := range pe.nodes {
		if node.Status == v1.NodeOnline {
			for _, gpu := range node.GPUs {
				if gpu.HealthStatus == v1.HealthHealthy {
					totalOnlineGPUs++
					if gpu.TotalVRAMMiB > maxSingleGPUVRAM {
						maxSingleGPUVRAM = gpu.TotalVRAMMiB
					}
				}
			}
		}
	}

	if totalOnlineGPUs == 0 {
		return decision.Reject(v1.RejectionNoEligibleNodes, 0), nil
	}

	if req.RequestedVRAMMiB > maxSingleGPUVRAM {
		return decision.Reject(v1.RejectionExceedsMaxHardware, len(pe.nodes)), nil
	}

	// 2. Filter candidate GPUs
	var candidates []Candidate
	evaluatedNodes := 0

	for _, node := range pe.nodes {
		evaluatedNodes++
		if node.Status != v1.NodeOnline {
			continue
		}

		// NodeSelector check
		if len(req.NodeSelector) > 0 {
			match := true
			for k, v := range req.NodeSelector {
				if node.Labels == nil || node.Labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		for i := range node.GPUs {
			gpu := &node.GPUs[i]
			if gpu.HealthStatus != v1.HealthHealthy {
				continue
			}

			// Capacity check
			if gpu.FreeVRAMMiB >= req.RequestedVRAMMiB {
				candidates = append(candidates, Candidate{
					NodeName:          node.NodeID,
					KubernetesNode:    node.KubernetesNode,
					GPU:               gpu,
					RemainingAfterMiB: gpu.FreeVRAMMiB - req.RequestedVRAMMiB,
				})
			}
		}
	}

	// 3. Best-Fit Binpacking Strategy:
	// Select candidate that leaves the MINIMUM remaining capacity on the device.
	// This packs workloads tightly, leaving larger contiguous blocks on other cards.
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].RemainingAfterMiB == candidates[j].RemainingAfterMiB {
				// Secondary sort: Prefer higher priority or deterministic GPU index
				return candidates[i].GPU.Index < candidates[j].GPU.Index
			}
			return candidates[i].RemainingAfterMiB < candidates[j].RemainingAfterMiB
		})

		best := candidates[0]

		// Atomic Allocation
		best.GPU.AllocatedVRAMMiB += req.RequestedVRAMMiB
		best.GPU.FreeVRAMMiB -= req.RequestedVRAMMiB

		alloc := v1.ActiveAllocation{
			JobID:            req.JobID,
			JobName:          req.WorkloadID,
			AllocatedVRAMMiB: req.RequestedVRAMMiB,
			AllocatedAt:      now,
		}
		best.GPU.ActiveAllocations = append(best.GPU.ActiveAllocations, alloc)
		pe.allocations[req.JobID] = &alloc
		pe.jobLocations[req.JobID] = location{nodeID: best.NodeName, gpuID: best.GPU.GPUID}

		computeShares := (req.RequestedVRAMMiB * 100) / best.GPU.TotalVRAMMiB
		if computeShares < 1 {
			computeShares = 1
		}

		return &v1.PlacementDecision{
			JobID:            req.JobID,
			Status:           v1.DecisionPlaced,
			AssignedNode:     best.KubernetesNode,
			AssignedGPUID:    best.GPU.GPUID,
			AssignedGPUIndex: best.GPU.Index,
			Quota: &v1.GPUQuota{
				AllocatedVRAMMiB: req.RequestedVRAMMiB,
				EnforcementMode:  "CUDA_INTERCEPTOR",
				ComputeShares:    computeShares,
			},
			PlacementReason:      v1.PlacementBestFitFound,
			EvaluatedNodes:       evaluatedNodes,
			PoolFreeVRAMAfterMiB: pe.calculateTotalFreeVRAMLocked(),
			DecisionTimestamp:    now,
		}, nil
	}

	// 4. No current capacity -> Enqueue in Priority/FIFO waitlist
	pe.enqueuePendingLocked(req)

	return &v1.PlacementDecision{
		JobID:             req.JobID,
		Status:            v1.DecisionPending,
		PendingReason:     v1.ReasonWaitingForCapacity,
		EvaluatedNodes:    evaluatedNodes,
		DecisionTimestamp: now,
	}, nil
}

// Release deallocates VRAM and checks the pending queue for unblocked jobs.
func (pe *PlacementEngine) Release(ctx context.Context, jobID string, nodeID string, gpuID string, vramMiB int) error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	loc, exists := pe.jobLocations[jobID]
	if !exists {
		// Fallback to provided arguments if caller supplies them
		if nodeID != "" && gpuID != "" {
			loc = location{nodeID: nodeID, gpuID: gpuID}
		} else {
			return ErrJobNotFound
		}
	}

	node, nodeExists := pe.nodes[loc.nodeID]
	if !nodeExists {
		return ErrNodeNotFound
	}

	var targetGPU *v1.GPUDevice
	for i := range node.GPUs {
		if node.GPUs[i].GPUID == loc.gpuID {
			targetGPU = &node.GPUs[i]
			break
		}
	}

	if targetGPU == nil {
		return ErrGPUNotFound
	}

	// Determine actual allocated VRAM
	reclaimAmount := vramMiB
	if alloc, ok := pe.allocations[jobID]; ok {
		if reclaimAmount <= 0 {
			reclaimAmount = alloc.AllocatedVRAMMiB
		}
		delete(pe.allocations, jobID)
	}
	delete(pe.jobLocations, jobID)

	// Clean up GPU active allocations list
	newActive := make([]v1.ActiveAllocation, 0, len(targetGPU.ActiveAllocations))
	for _, a := range targetGPU.ActiveAllocations {
		if a.JobID != jobID {
			newActive = append(newActive, a)
		}
	}
	targetGPU.ActiveAllocations = newActive

	// Reclaim memory atomically
	targetGPU.AllocatedVRAMMiB -= reclaimAmount
	if targetGPU.AllocatedVRAMMiB < 0 {
		targetGPU.AllocatedVRAMMiB = 0
	}
	targetGPU.FreeVRAMMiB = targetGPU.TotalVRAMMiB - targetGPU.AllocatedVRAMMiB

	return nil
}

// PopNextPending attempts to dequeue the highest priority pending job that can fit now.
func (pe *PlacementEngine) PopNextPending(ctx context.Context) (*v1.PlacementRequest, *v1.PlacementDecision, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	if len(pe.pendingQueue) == 0 {
		return nil, nil, nil
	}

	for idx, req := range pe.pendingQueue {
		// Try placement simulation
		var bestCandidate *Candidate
		for _, node := range pe.nodes {
			if node.Status != v1.NodeOnline {
				continue
			}
			for i := range node.GPUs {
				gpu := &node.GPUs[i]
				if gpu.HealthStatus == v1.HealthHealthy && gpu.FreeVRAMMiB >= req.RequestedVRAMMiB {
					rem := gpu.FreeVRAMMiB - req.RequestedVRAMMiB
					if bestCandidate == nil || rem < bestCandidate.RemainingAfterMiB {
						bestCandidate = &Candidate{
							NodeName:          node.NodeID,
							KubernetesNode:    node.KubernetesNode,
							GPU:               gpu,
							RemainingAfterMiB: rem,
						}
					}
				}
			}
		}

		if bestCandidate != nil {
			// Found placement! Remove from queue
			pe.pendingQueue = append(pe.pendingQueue[:idx], pe.pendingQueue[idx+1:]...)

			// Allocate
			bestCandidate.GPU.AllocatedVRAMMiB += req.RequestedVRAMMiB
			bestCandidate.GPU.FreeVRAMMiB -= req.RequestedVRAMMiB

			now := time.Now().UTC()
			alloc := v1.ActiveAllocation{
				JobID:            req.JobID,
				JobName:          req.WorkloadID,
				AllocatedVRAMMiB: req.RequestedVRAMMiB,
				AllocatedAt:      now,
			}
			bestCandidate.GPU.ActiveAllocations = append(bestCandidate.GPU.ActiveAllocations, alloc)
			pe.allocations[req.JobID] = &alloc
			pe.jobLocations[req.JobID] = location{nodeID: bestCandidate.NodeName, gpuID: bestCandidate.GPU.GPUID}

			computeShares := (req.RequestedVRAMMiB * 100) / bestCandidate.GPU.TotalVRAMMiB
			if computeShares < 1 {
				computeShares = 1
			}

			decision := &v1.PlacementDecision{
				JobID:            req.JobID,
				Status:           v1.DecisionPlaced,
				AssignedNode:     bestCandidate.KubernetesNode,
				AssignedGPUID:    bestCandidate.GPU.GPUID,
				AssignedGPUIndex: bestCandidate.GPU.Index,
				Quota: &v1.GPUQuota{
					AllocatedVRAMMiB: req.RequestedVRAMMiB,
					EnforcementMode:  "CUDA_INTERCEPTOR",
					ComputeShares:    computeShares,
				},
				PlacementReason:      v1.PlacementBestFitFound,
				PoolFreeVRAMAfterMiB: pe.calculateTotalFreeVRAMLocked(),
				DecisionTimestamp:    now,
			}

			return req, decision, nil
		}
	}

	return nil, nil, nil
}

// GetPoolCapacity provides summary statistics.
func (pe *PlacementEngine) GetPoolCapacity(ctx context.Context) (*v1.PoolCapacitySummary, error) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	summary := &v1.PoolCapacitySummary{
		TotalNodes: len(pe.nodes),
		UpdatedAt:  time.Now().UTC(),
	}

	for _, node := range pe.nodes {
		for _, gpu := range node.GPUs {
			summary.TotalGPUs++
			summary.AggregateVRAMMiB += gpu.TotalVRAMMiB
			summary.AllocatedVRAMMiB += gpu.AllocatedVRAMMiB
			summary.FreeVRAMMiB += gpu.FreeVRAMMiB
		}
	}

	return summary, nil
}

// GetPendingQueueLength returns count of queued jobs.
func (pe *PlacementEngine) GetPendingQueueLength() int {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return len(pe.pendingQueue)
}

func (pe *PlacementEngine) calculateTotalFreeVRAMLocked() int {
	free := 0
	for _, node := range pe.nodes {
		if node.Status == v1.NodeOnline {
			for _, gpu := range node.GPUs {
				if gpu.HealthStatus == v1.HealthHealthy {
					free += gpu.FreeVRAMMiB
				}
			}
		}
	}
	return free
}

func (pe *PlacementEngine) enqueuePendingLocked(req *v1.PlacementRequest) {
	pe.pendingQueue = append(pe.pendingQueue, req)
	// Sort queue: Priority DESC, SubmittedAt ASC
	sort.Slice(pe.pendingQueue, func(i, j int) bool {
		if pe.pendingQueue[i].Priority == pe.pendingQueue[j].Priority {
			return pe.pendingQueue[i].SubmittedAt.Before(pe.pendingQueue[j].SubmittedAt)
		}
		return pe.pendingQueue[i].Priority > pe.pendingQueue[j].Priority
	})
}

// Helper builder for decision responses
type PlacementDecisionBuilder struct {
	JobID             string
	DecisionTimestamp time.Time
}

func (b *PlacementDecisionBuilder) Reject(reason v1.RejectionReason, evaluatedNodes int) *v1.PlacementDecision {
	return &v1.PlacementDecision{
		JobID:             b.JobID,
		Status:            v1.DecisionRejected,
		RejectionReason:   reason,
		EvaluatedNodes:    evaluatedNodes,
		DecisionTimestamp: b.DecisionTimestamp,
	}
}
