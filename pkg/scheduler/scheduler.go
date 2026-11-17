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
	ErrNilRequest   = errors.New("placement request cannot be nil")
	ErrMissingJobID = errors.New("job_id is required")
	ErrInvalidVRAM  = errors.New("requested VRAM must be greater than 0")
	ErrJobNotFound  = errors.New("active allocation for job ID not found")
	ErrGPUNotFound  = errors.New("target GPU not found on specified node")
	ErrNodeNotFound = errors.New("target node not found")
)

// Candidate represents an evaluated GPU match during binpacking.
type Candidate struct {
	NodeName          string
	KubernetesNode    string
	GPU               *v1.GPUDevice
	RemainingAfterMiB int
}

// PlacementEngine implements the v1.Scheduler interface with atomic binpacking and queueing.
//
// Every job ID the engine knows about is in exactly one of two places: allocations (it holds
// VRAM on a GPU) or pendingQueue (it is waiting for capacity). decisions mirrors the latest
// decision for each of them so that a retried Schedule call or a status lookup returns the
// same answer instead of reserving VRAM twice.
type PlacementEngine struct {
	mu                sync.RWMutex
	nodes             map[string]*v1.GPUNode
	allocations       map[string]*v1.ActiveAllocation // jobID -> allocation
	jobLocations      map[string]location             // jobID -> (nodeID, gpuID)
	decisions         map[string]*v1.PlacementDecision
	pendingQueue      []*v1.PlacementRequest
	contextReserveMiB int

	statePath  string // empty = no persistence
	persistErr error
}

type location struct {
	nodeID string
	gpuID  string
}

// NewPlacementEngine constructs a thread-safe scheduler instance with a default 512 MiB context reserve.
func NewPlacementEngine() *PlacementEngine {
	return &PlacementEngine{
		nodes:             make(map[string]*v1.GPUNode),
		allocations:       make(map[string]*v1.ActiveAllocation),
		jobLocations:      make(map[string]location),
		decisions:         make(map[string]*v1.PlacementDecision),
		pendingQueue:      make([]*v1.PlacementRequest, 0),
		contextReserveMiB: 512, // Driver context overhead reserve (A3 evidence)
	}
}

// SetContextReserve sets the reserve amount subtracted from total VRAM for driver overhead.
func (pe *PlacementEngine) SetContextReserve(reserveMiB int) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.contextReserveMiB = reserveMiB
}

// RegisterOrUpdateNode registers or updates a GPU worker node in the scheduler inventory.
//
// Free VRAM is always derived as total - driver reserve - allocated; any FreeVRAMMiB the
// caller sets is ignored. (Previously a caller-supplied FreeVRAMMiB equal to the full card
// size bypassed the reserve entirely, letting jobs fill the last 512 MiB the CUDA context
// needs.) Re-registering a node keeps the scheduler's own reservations on GPUs that still exist.
func (pe *PlacementEngine) RegisterOrUpdateNode(node *v1.GPUNode) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	prev := pe.nodes[node.NodeID]
	cloned := *node
	cloned.GPUs = make([]v1.GPUDevice, len(node.GPUs))
	for i, g := range node.GPUs {
		g.AllocatedVRAMMiB = 0
		g.ActiveAllocations = nil
		if prev != nil {
			for _, pg := range prev.GPUs {
				if pg.GPUID == g.GPUID {
					g.AllocatedVRAMMiB = pg.AllocatedVRAMMiB
					g.ActiveAllocations = pg.ActiveAllocations
				}
			}
		}
		if g.ActiveAllocations == nil {
			g.ActiveAllocations = []v1.ActiveAllocation{}
		}
		g.FreeVRAMMiB = g.TotalVRAMMiB - pe.contextReserveMiB - g.AllocatedVRAMMiB
		if g.FreeVRAMMiB < 0 {
			g.FreeVRAMMiB = 0
		}
		cloned.GPUs[i] = g
	}
	pe.nodes[node.NodeID] = &cloned
}

// SetNodeStatus marks a registered node ONLINE/OFFLINE/MAINTENANCE. Offline nodes keep their
// existing allocations (the jobs may still be running) but receive no new placements.
// It reports whether the status changed. When a node comes back online, queued jobs that now
// fit are placed and returned.
func (pe *PlacementEngine) SetNodeStatus(nodeID string, status v1.NodeStatus) (bool, []*v1.PlacementDecision) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	node, ok := pe.nodes[nodeID]
	if !ok || node.Status == status {
		return false, nil
	}
	node.Status = status
	var dispatched []*v1.PlacementDecision
	if status == v1.NodeOnline {
		dispatched = pe.dispatchPendingLocked()
	}
	pe.persistLocked()
	return true, dispatched
}

// Schedule attempts atomic placement of a job onto the best-fit GPU.
//
// Schedule is idempotent per job ID: if the job is already placed or queued, the existing
// decision is returned and nothing new is reserved. Callers (the Go API in B2) can therefore
// safely retry after a timeout.
func (pe *PlacementEngine) Schedule(ctx context.Context, req *v1.PlacementRequest) (*v1.PlacementDecision, error) {
	if req == nil {
		return nil, ErrNilRequest
	}
	if req.JobID == "" {
		return nil, ErrMissingJobID
	}
	if req.RequestedVRAMMiB <= 0 {
		return nil, ErrInvalidVRAM
	}

	pe.mu.Lock()
	defer pe.mu.Unlock()

	if existing, ok := pe.decisions[req.JobID]; ok {
		out := *existing
		return &out, nil
	}

	now := time.Now().UTC()
	if req.SubmittedAt.IsZero() {
		req.SubmittedAt = now
	}
	decision := &PlacementDecisionBuilder{
		JobID:             req.JobID,
		DecisionTimestamp: now,
	}

	// 1. Check max hardware capacity across pool (accounting for driver context reserve).
	// Offline nodes still count here: a request that fits a sleeping laptop's GPU should wait
	// for it rather than be rejected outright.
	maxSingleGPUVRAM := 0
	totalGPUs := 0
	for _, node := range pe.nodes {
		if node.Status == v1.NodeMaintenance || !matchesSelector(node, req.NodeSelector) {
			continue
		}
		for _, gpu := range node.GPUs {
			if gpu.HealthStatus == v1.HealthUnhealthy {
				continue
			}
			totalGPUs++
			allocatable := gpu.TotalVRAMMiB - pe.contextReserveMiB
			if allocatable > maxSingleGPUVRAM {
				maxSingleGPUVRAM = allocatable
			}
		}
	}

	if totalGPUs == 0 {
		return decision.Reject(v1.RejectionNoEligibleNodes, 0), nil
	}

	if req.RequestedVRAMMiB > maxSingleGPUVRAM {
		return decision.Reject(v1.RejectionExceedsMaxHardware, len(pe.nodes)), nil
	}

	// 2-3. Best-fit placement on an online node.
	if placed := pe.placeLocked(req, now); placed != nil {
		pe.persistLocked()
		out := *placed
		return &out, nil
	}

	// 4. No current capacity -> Enqueue in Priority/FIFO waitlist
	pe.enqueuePendingLocked(req)

	pending := &v1.PlacementDecision{
		JobID:             req.JobID,
		Status:            v1.DecisionPending,
		PendingReason:     v1.ReasonWaitingForCapacity,
		EvaluatedNodes:    len(pe.nodes),
		DecisionTimestamp: now,
	}
	pe.decisions[req.JobID] = pending
	pe.persistLocked()
	out := *pending
	return &out, nil
}

// placeLocked runs best-fit selection for req and, if a GPU fits, reserves the VRAM and
// records the decision. It returns nil when nothing fits. Caller holds pe.mu.
func (pe *PlacementEngine) placeLocked(req *v1.PlacementRequest, now time.Time) *v1.PlacementDecision {
	var candidates []Candidate
	for _, node := range pe.nodes {
		if node.Status != v1.NodeOnline || !matchesSelector(node, req.NodeSelector) {
			continue
		}
		for i := range node.GPUs {
			gpu := &node.GPUs[i]
			if gpu.HealthStatus != v1.HealthHealthy {
				continue
			}
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
	if len(candidates) == 0 {
		return nil
	}

	// Best-Fit Binpacking: select the GPU that leaves the MINIMUM remaining capacity.
	// This packs workloads tightly, leaving larger contiguous blocks on other cards.
	// Ties break on node ID then GPU index so the choice does not depend on map order.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.RemainingAfterMiB != b.RemainingAfterMiB {
			return a.RemainingAfterMiB < b.RemainingAfterMiB
		}
		if a.NodeName != b.NodeName {
			return a.NodeName < b.NodeName
		}
		return a.GPU.Index < b.GPU.Index
	})
	best := candidates[0]

	alloc := v1.ActiveAllocation{
		JobID:            req.JobID,
		JobName:          req.WorkloadID,
		AllocatedVRAMMiB: req.RequestedVRAMMiB,
		AllocatedAt:      now,
	}
	pe.applyAllocationLocked(best.NodeName, best.GPU, alloc)

	computeShares := (req.RequestedVRAMMiB * 100) / best.GPU.TotalVRAMMiB
	if computeShares < 1 {
		computeShares = 1
	}

	d := &v1.PlacementDecision{
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
		EvaluatedNodes:       len(pe.nodes),
		PoolFreeVRAMAfterMiB: pe.calculateTotalFreeVRAMLocked(),
		DecisionTimestamp:    now,
	}
	pe.decisions[req.JobID] = d
	return d
}

func (pe *PlacementEngine) applyAllocationLocked(nodeID string, gpu *v1.GPUDevice, alloc v1.ActiveAllocation) {
	gpu.AllocatedVRAMMiB += alloc.AllocatedVRAMMiB
	gpu.FreeVRAMMiB -= alloc.AllocatedVRAMMiB
	gpu.ActiveAllocations = append(gpu.ActiveAllocations, alloc)
	a := alloc
	pe.allocations[alloc.JobID] = &a
	pe.jobLocations[alloc.JobID] = location{nodeID: nodeID, gpuID: gpu.GPUID}
}

// Release deallocates a job's VRAM (or cancels it if it is still queued) and places any
// queued jobs that now fit. It implements v1.Scheduler; use ReleaseJob to also learn which
// queued jobs were dispatched.
//
// nodeID, gpuID and vramMiB are accepted for interface compatibility but ignored: the engine
// releases exactly what it reserved, so a wrong or stale caller value cannot corrupt the pool.
func (pe *PlacementEngine) Release(ctx context.Context, jobID string, nodeID string, gpuID string, vramMiB int) error {
	_, _, err := pe.ReleaseJob(ctx, jobID)
	return err
}

// ReleaseJob frees jobID's reservation, or removes it from the queue if it never got one.
// It returns the VRAM reclaimed (0 for a cancelled queued job) and the decisions for every
// queued job that was placed as a result.
func (pe *PlacementEngine) ReleaseJob(ctx context.Context, jobID string) (int, []*v1.PlacementDecision, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	if _, allocated := pe.allocations[jobID]; !allocated {
		for i, req := range pe.pendingQueue {
			if req.JobID == jobID {
				pe.pendingQueue = append(pe.pendingQueue[:i], pe.pendingQueue[i+1:]...)
				delete(pe.decisions, jobID)
				pe.persistLocked()
				return 0, nil, nil
			}
		}
		return 0, nil, ErrJobNotFound
	}

	reclaimed, err := pe.releaseLocked(jobID)
	if err != nil {
		return 0, nil, err
	}
	dispatched := pe.dispatchPendingLocked()
	pe.persistLocked()
	return reclaimed, dispatched, nil
}

func (pe *PlacementEngine) releaseLocked(jobID string) (int, error) {
	alloc, ok := pe.allocations[jobID]
	if !ok {
		return 0, ErrJobNotFound
	}
	loc := pe.jobLocations[jobID]
	reclaim := alloc.AllocatedVRAMMiB

	delete(pe.allocations, jobID)
	delete(pe.jobLocations, jobID)
	delete(pe.decisions, jobID)

	node, nodeExists := pe.nodes[loc.nodeID]
	if !nodeExists {
		return reclaim, ErrNodeNotFound
	}
	var targetGPU *v1.GPUDevice
	for i := range node.GPUs {
		if node.GPUs[i].GPUID == loc.gpuID {
			targetGPU = &node.GPUs[i]
			break
		}
	}
	if targetGPU == nil {
		return reclaim, ErrGPUNotFound
	}

	newActive := make([]v1.ActiveAllocation, 0, len(targetGPU.ActiveAllocations))
	for _, a := range targetGPU.ActiveAllocations {
		if a.JobID != jobID {
			newActive = append(newActive, a)
		}
	}
	targetGPU.ActiveAllocations = newActive

	targetGPU.AllocatedVRAMMiB -= reclaim
	if targetGPU.AllocatedVRAMMiB < 0 {
		targetGPU.AllocatedVRAMMiB = 0
	}
	targetGPU.FreeVRAMMiB = (targetGPU.TotalVRAMMiB - pe.contextReserveMiB) - targetGPU.AllocatedVRAMMiB
	if targetGPU.FreeVRAMMiB < 0 {
		targetGPU.FreeVRAMMiB = 0
	}
	return reclaim, nil
}

// dispatchPendingLocked places every queued job that fits, in priority/FIFO order. A large
// job at the head of the queue does not block smaller ones behind it (no head-of-line
// blocking); that trades strict fairness for utilisation, which suits short training jobs.
func (pe *PlacementEngine) dispatchPendingLocked() []*v1.PlacementDecision {
	var dispatched []*v1.PlacementDecision
	now := time.Now().UTC()
	remaining := pe.pendingQueue[:0]
	for _, req := range pe.pendingQueue {
		if d := pe.placeLocked(req, now); d != nil {
			out := *d
			dispatched = append(dispatched, &out)
			continue
		}
		remaining = append(remaining, req)
	}
	pe.pendingQueue = remaining
	return dispatched
}

// PopNextPending places the highest-priority queued job that fits now, if any.
// Kept for callers that dispatch one job at a time; ReleaseJob already dispatches everything.
func (pe *PlacementEngine) PopNextPending(ctx context.Context) (*v1.PlacementRequest, *v1.PlacementDecision, error) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	now := time.Now().UTC()
	for idx, req := range pe.pendingQueue {
		if d := pe.placeLocked(req, now); d != nil {
			pe.pendingQueue = append(pe.pendingQueue[:idx], pe.pendingQueue[idx+1:]...)
			pe.persistLocked()
			out := *d
			return req, &out, nil
		}
	}
	return nil, nil, nil
}

// GetJob returns the current decision for a job ID: PLACED with its GPU, or PENDING while
// queued. The API polls this to learn when a queued job has been placed.
func (pe *PlacementEngine) GetJob(jobID string) (*v1.PlacementDecision, bool) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	d, ok := pe.decisions[jobID]
	if !ok {
		return nil, false
	}
	out := *d
	return &out, true
}

// AllocationInfo describes one live reservation, for reconciliation and inspection.
type AllocationInfo struct {
	JobID            string    `json:"job_id"`
	NodeID           string    `json:"node_id"`
	GPUID            string    `json:"gpu_id"`
	AllocatedVRAMMiB int       `json:"allocated_vram_mib"`
	AllocatedAt      time.Time `json:"allocated_at"`
}

// ListAllocations returns every live reservation sorted by job ID.
func (pe *PlacementEngine) ListAllocations() []AllocationInfo {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	out := make([]AllocationInfo, 0, len(pe.allocations))
	for id, a := range pe.allocations {
		loc := pe.jobLocations[id]
		out = append(out, AllocationInfo{
			JobID: id, NodeID: loc.nodeID, GPUID: loc.gpuID,
			AllocatedVRAMMiB: a.AllocatedVRAMMiB, AllocatedAt: a.AllocatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// ReconcileOrphans releases reservations whose job no longer has a live Kubernetes pod.
//
// live is the set of job IDs that have at least one pod in Pending or Running phase.
// Allocations younger than grace are skipped: the API reserves first and creates the Job
// afterwards, so a fresh reservation legitimately has no pod yet. It returns the released
// job IDs and any queued jobs placed into the freed capacity.
func (pe *PlacementEngine) ReconcileOrphans(live map[string]bool, grace time.Duration, now time.Time) ([]string, []*v1.PlacementDecision) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	var released []string
	for id, a := range pe.allocations {
		if live[id] || now.Sub(a.AllocatedAt) < grace {
			continue
		}
		if _, err := pe.releaseLocked(id); err == nil || errors.Is(err, ErrNodeNotFound) || errors.Is(err, ErrGPUNotFound) {
			released = append(released, id)
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	sort.Strings(released)
	dispatched := pe.dispatchPendingLocked()
	pe.persistLocked()
	return released, dispatched
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
			if node.Status == v1.NodeOnline && gpu.HealthStatus == v1.HealthHealthy {
				summary.FreeVRAMMiB += gpu.FreeVRAMMiB
			}
		}
	}

	return summary, nil
}

// GetNodes returns a copy of the node inventory sorted by node ID.
func (pe *PlacementEngine) GetNodes() []v1.GPUNode {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	out := make([]v1.GPUNode, 0, len(pe.nodes))
	for _, n := range pe.nodes {
		c := *n
		c.GPUs = make([]v1.GPUDevice, len(n.GPUs))
		for i, g := range n.GPUs {
			c.GPUs[i] = g
			c.GPUs[i].ActiveAllocations = append(make([]v1.ActiveAllocation, 0, len(g.ActiveAllocations)), g.ActiveAllocations...)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// ContextReserveMiB returns the per-GPU driver context reserve.
func (pe *PlacementEngine) ContextReserveMiB() int {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return pe.contextReserveMiB
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
	cp := *req
	pe.pendingQueue = append(pe.pendingQueue, &cp)
	sortQueue(pe.pendingQueue)
}

// sortQueue orders by Priority DESC, SubmittedAt ASC.
func sortQueue(q []*v1.PlacementRequest) {
	sort.SliceStable(q, func(i, j int) bool {
		if q[i].Priority == q[j].Priority {
			return q[i].SubmittedAt.Before(q[j].SubmittedAt)
		}
		return q[i].Priority > q[j].Priority
	})
}

func matchesSelector(node *v1.GPUNode, selector map[string]string) bool {
	for k, v := range selector {
		if node.Labels == nil || node.Labels[k] != v {
			return false
		}
	}
	return true
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
