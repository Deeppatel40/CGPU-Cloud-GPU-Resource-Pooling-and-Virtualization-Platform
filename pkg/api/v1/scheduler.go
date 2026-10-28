package v1

import (
	"context"
	"time"
)

// DecisionType represents the outcome of a scheduler placement evaluation.
type DecisionType string

const (
	DecisionPlaced   DecisionType = "PLACED"
	DecisionPending  DecisionType = "PENDING"
	DecisionRejected DecisionType = "REJECTED"
)

// PlacementReason defines why a placement decision was successful.
type PlacementReason string

const (
	PlacementBestFitFound       PlacementReason = "BEST_FIT_PACKING_FOUND"
	PlacementExclusiveAllocated PlacementReason = "EXCLUSIVE_ALLOCATION"
	PlacementNodeAffinityMet    PlacementReason = "NODE_AFFINITY_MATCHED"
)

// RejectionReason defines unrecoverable placement errors.
type RejectionReason string

const (
	RejectionExceedsMaxHardware RejectionReason = "REQUESTED_VRAM_EXCEEDS_MAX_HARDWARE"
	RejectionNoEligibleNodes    RejectionReason = "NO_ELIGIBLE_NODES"
	RejectionQuotaExceeded      RejectionReason = "TENANT_QUOTA_EXCEEDED"
)

// GPUQuota defines the isolation parameters enforced on the container runtime.
type GPUQuota struct {
	AllocatedVRAMMiB int    `json:"allocated_vram_mib" bson:"allocated_vram_mib"`
	EnforcementMode  string `json:"enforcement_mode" bson:"enforcement_mode"`   // CGPU_VIRTUAL_CGROUP, NVIDIA_MPS, CUDA_INTERCEPTOR
	ComputeShares    int    `json:"compute_shares" bson:"compute_shares"`       // Relative compute weight (e.g. 50% shares)
}

// PlacementRequest is sent by the Job Manager to the Scheduler.
type PlacementRequest struct {
	JobID               string            `json:"job_id"`
	WorkloadID          string            `json:"workload_id"`
	RequestedVRAMMiB    int               `json:"requested_vram_mib"`
	Priority            int               `json:"priority"`
	NodeSelector        map[string]string `json:"node_selector,omitempty"`
	RequiredCUDAVersion string            `json:"required_cuda_version,omitempty"`
	SubmittedAt         time.Time         `json:"submitted_at"`
}

// PlacementDecision is returned by the Scheduler to the Job Manager.
type PlacementDecision struct {
	JobID                  string          `json:"job_id"`
	Status                 DecisionType    `json:"status"`
	AssignedNode           string          `json:"assigned_node,omitempty"`
	AssignedGPUID          string          `json:"assigned_gpu_id,omitempty"`
	AssignedGPUIndex       int             `json:"assigned_gpu_index,omitempty"`
	Quota                  *GPUQuota       `json:"quota,omitempty"`
	PlacementReason        PlacementReason `json:"placement_reason,omitempty"`
	PendingReason          PendingReason   `json:"pending_reason,omitempty"`
	RejectionReason        RejectionReason `json:"rejection_reason,omitempty"`
	EvaluatedNodes         int             `json:"evaluated_nodes"`
	PoolFreeVRAMAfterMiB   int             `json:"pool_free_vram_after_mib,omitempty"`
	DecisionTimestamp      time.Time       `json:"decision_timestamp"`
}

// Scheduler defines the placement interface implemented by Deep & Nishit's placement engine.
type Scheduler interface {
	// Schedule attempts to place a job on an optimal GPU node.
	Schedule(ctx context.Context, req *PlacementRequest) (*PlacementDecision, error)

	// Release deallocates GPU VRAM and decrements pool usage when a job completes or fails.
	Release(ctx context.Context, jobID string, nodeID string, gpuID string, vramMiB int) error

	// GetPoolCapacity returns current aggregate and per-node GPU availability.
	GetPoolCapacity(ctx context.Context) (*PoolCapacitySummary, error)
}

// PoolCapacitySummary gives a quick status of the scheduling pool.
type PoolCapacitySummary struct {
	TotalNodes        int       `json:"total_nodes"`
	TotalGPUs         int       `json:"total_gpus"`
	AggregateVRAMMiB  int       `json:"aggregate_vram_mib"`
	AllocatedVRAMMiB  int       `json:"allocated_vram_mib"`
	FreeVRAMMiB       int       `json:"free_vram_mib"`
	UpdatedAt         time.Time `json:"updated_at"`
}

