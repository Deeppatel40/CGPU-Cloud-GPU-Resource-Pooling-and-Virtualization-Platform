package v1

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// JobState represents the primary lifecycle status of a workload.
type JobState string

const (
	StatePending   JobState = "PENDING"
	StateRunning   JobState = "RUNNING"
	StateSucceeded JobState = "SUCCEEDED"
	StateFailed    JobState = "FAILED"
)

// PendingReason represents the granular cause for a job waiting in PENDING.
type PendingReason string

const (
	ReasonWaitingForCapacity     PendingReason = "WAITING_FOR_CAPACITY"
	ReasonQueuedFIFO             PendingReason = "QUEUED_FIFO"
	ReasonSchedulingInProgress   PendingReason = "SCHEDULING_IN_PROGRESS"
	ReasonPullingImage           PendingReason = "PULLING_IMAGE"
	ReasonContainerCreating      PendingReason = "CONTAINER_CREATING"
	ReasonNodeDrainingOrCordoned PendingReason = "NODE_DRAINING_OR_CORDONED"
)

// FailureReason represents the root cause of a failed workload.
type FailureReason string

const (
	ReasonCUDAOutOfMemory       FailureReason = "CUDA_OUT_OF_MEMORY"
	ReasonContainerNonZeroExit  FailureReason = "CONTAINER_NON_ZERO_EXIT"
	ReasonInvalidWorkloadParams FailureReason = "INVALID_WORKLOAD_PARAMS"
	ReasonDatasetDownloadFailed FailureReason = "DATASET_DOWNLOAD_FAILED"
	ReasonSchedulingTimeout     FailureReason = "SCHEDULING_TIMEOUT"
	ReasonNodeFailure           FailureReason = "NODE_FAILURE"
	ReasonCancelledByUser       FailureReason = "CANCELLED_BY_USER"
)

// TelemetryStatus indicates the availability of real-time GPU metrics.
type TelemetryStatus string

const (
	TelemetryAvailable   TelemetryStatus = "AVAILABLE"
	TelemetryUnavailable TelemetryStatus = "UNAVAILABLE"
)

// DatasetType indicates the input source for the workload dataset.
type DatasetType string

const (
	DatasetBuiltin DatasetType = "builtin"
	DatasetHTTP    DatasetType = "http"
	DatasetS3      DatasetType = "s3"
	DatasetPVC     DatasetType = "pvc"
)

// DatasetReference defines the input dataset location and verification.
type DatasetReference struct {
	Type           DatasetType `json:"type" bson:"type"`
	Name           string      `json:"name,omitempty" bson:"name,omitempty"`
	URI            string      `json:"uri,omitempty" bson:"uri,omitempty"`
	ChecksumSHA256 string      `json:"checksum_sha256,omitempty" bson:"checksum_sha256,omitempty"`
}

// JobSubmissionRequest represents the client submission payload.
type JobSubmissionRequest struct {
	RequestID        string                 `json:"request_id"`
	Name             string                 `json:"name"`
	WorkloadID       string                 `json:"workload_id"`
	RequestedVRAMMiB int                    `json:"requested_vram_mib"`
	Parameters       map[string]interface{} `json:"parameters,omitempty"`
	Dataset          *DatasetReference      `json:"dataset,omitempty"`
	Priority         int                    `json:"priority,omitempty"`
	TimeoutSeconds   int                    `json:"timeout_seconds,omitempty"`
}

// Validate checks the structural and boundary constraints of the submission.
func (r *JobSubmissionRequest) Validate(maxNodeVRAMMiB int) error {
	if r.RequestID == "" {
		return errors.New("request_id is required for idempotent retry handling")
	}
	uuidRegex := regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	// Also accept req- prefixed UUIDs for mock and friendly formats
	cleanUUID := r.RequestID
	if len(cleanUUID) > 4 && cleanUUID[:4] == "req-" {
		cleanUUID = cleanUUID[4:]
	}
	if !uuidRegex.MatchString(cleanUUID) {
		return fmt.Errorf("request_id '%s' must be a valid UUIDv4 string", r.RequestID)
	}

	if r.Name == "" {
		return errors.New("name is required")
	}
	if len(r.Name) > 64 {
		return errors.New("name must not exceed 64 characters")
	}

	if r.WorkloadID == "" {
		return errors.New("workload_id is required")
	}

	if r.RequestedVRAMMiB <= 0 {
		return fmt.Errorf("requested_vram_mib must be a positive integer greater than 0, got %d", r.RequestedVRAMMiB)
	}
	if maxNodeVRAMMiB > 0 && r.RequestedVRAMMiB > maxNodeVRAMMiB {
		return fmt.Errorf("requested_vram_mib (%d MiB) exceeds maximum GPU capacity in cluster (%d MiB)", r.RequestedVRAMMiB, maxNodeVRAMMiB)
	}

	if r.Priority == 0 {
		r.Priority = 5
	}
	if r.Priority < 1 || r.Priority > 10 {
		return fmt.Errorf("priority must be between 1 and 10, got %d", r.Priority)
	}

	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 3600
	} else if r.TimeoutSeconds < 60 {
		return fmt.Errorf("timeout_seconds must be at least 60 seconds, got %d", r.TimeoutSeconds)
	}

	return nil
}

// PlacementDetails holds the assignment results made by the scheduler.
type PlacementDetails struct {
	NodeID            string    `json:"node_id" bson:"node_id"`
	GPUID             string    `json:"gpu_id" bson:"gpu_id"`
	AssignedGPUIndex  int       `json:"assigned_gpu_index" bson:"assigned_gpu_index"`
	AllocatedVRAMMiB  int       `json:"allocated_vram_mib" bson:"allocated_vram_mib"`
	EnforcementMode   string    `json:"enforcement_mode" bson:"enforcement_mode"`
	PlacementReason   string    `json:"placement_reason" bson:"placement_reason"`
	ScheduledAt       time.Time `json:"scheduled_at" bson:"scheduled_at"`
}

// GPUTelemetrySample represents live hardware metrics during execution.
type GPUTelemetrySample struct {
	Status             TelemetryStatus `json:"status"`
	Reason             string          `json:"reason,omitempty"`
	Message            string          `json:"message,omitempty"`
	GPUUtilizationPct  *int            `json:"gpu_utilization_pct,omitempty"`
	MemoryUsedMiB      *int            `json:"memory_used_mib,omitempty"`
	TemperatureCelsius *int            `json:"temperature_celsius,omitempty"`
	PowerWatts         *float64        `json:"power_watts,omitempty"`
	SampledAt          time.Time       `json:"sampled_at"`
}

// ArtifactMetadata describes a checkpoint or output file produced by a job.
type ArtifactMetadata struct {
	ArtifactID     string    `json:"artifact_id" bson:"artifact_id"`
	Name           string    `json:"name" bson:"name"`
	ArtifactType   string    `json:"artifact_type" bson:"artifact_type"`
	SizeBytes      int64     `json:"size_bytes" bson:"size_bytes"`
	ChecksumSHA256 string    `json:"checksum_sha256" bson:"checksum_sha256"`
	DownloadURL    string    `json:"download_url" bson:"download_url"`
	CreatedAt      time.Time `json:"created_at" bson:"created_at"`
}

// ExecutionMetrics captures post-run statistics. Fields are workload-specific;
// unrecognised keys in the uploaded metrics.json are silently dropped.
type ExecutionMetrics struct {
	// ResNet / training workloads
	FinalLoss            *float64 `json:"final_loss,omitempty" bson:"final_loss,omitempty"`
	FinalAccuracy        *float64 `json:"final_accuracy,omitempty" bson:"final_accuracy,omitempty"`
	PeakVRAMMiB          *int     `json:"peak_vram_mib,omitempty" bson:"peak_vram_mib,omitempty"`
	TotalDurationSeconds *float64 `json:"total_duration_seconds,omitempty" bson:"total_duration_seconds,omitempty"`
	EpochsCompleted      *int     `json:"epochs_completed,omitempty" bson:"epochs_completed,omitempty"`
	SamplesPerSecond     *float64 `json:"samples_per_second,omitempty" bson:"samples_per_second,omitempty"`
	// matmul benchmark workload
	GFLOPs          *float64 `json:"gflops,omitempty" bson:"gflops,omitempty"`
	MatrixSize       *int64   `json:"matrix_size,omitempty" bson:"matrix_size,omitempty"`
	Iterations       *int64   `json:"iterations,omitempty" bson:"iterations,omitempty"`
	ElapsedSeconds   *float64 `json:"elapsed_seconds,omitempty" bson:"elapsed_seconds,omitempty"`
	WorkingSetMiB    *int64   `json:"working_set_mib,omitempty" bson:"working_set_mib,omitempty"`
	ResultVerified   *bool    `json:"result_verified,omitempty" bson:"result_verified,omitempty"`
	ResultElement0   *float64 `json:"result_element0,omitempty" bson:"result_element0,omitempty"`
	ExpectedElement0 *float64 `json:"expected_element0,omitempty" bson:"expected_element0,omitempty"`
	ResultSummary    *string  `json:"result_summary,omitempty" bson:"result_summary,omitempty"`
}

// JobTimestamps tracks all lifecycle milestones.
type JobTimestamps struct {
	SubmittedAt time.Time  `json:"submitted_at" bson:"submitted_at"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty" bson:"scheduled_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty" bson:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty" bson:"completed_at,omitempty"`
}

// PendingDetails gives actionable insight into why a job is waiting.
type PendingDetails struct {
	Message              string `json:"message"`
	QueuePosition        int    `json:"queue_position,omitempty"`
	RequiredVRAMMiB      int    `json:"required_vram_mib,omitempty"`
	AvailableVRAMMiB     int    `json:"available_vram_mib,omitempty"`
	PoolTotalVRAMMiB     int    `json:"pool_total_vram_mib,omitempty"`
	PoolAllocatedVRAMMiB int    `json:"pool_allocated_vram_mib,omitempty"`
	EstimatedWaitSeconds int    `json:"estimated_wait_seconds,omitempty"`
}

// FailureDetails provides diagnostics when a job enters StateFailed.
type FailureDetails struct {
	ExitCode     int       `json:"exit_code" bson:"exit_code"`
	ErrorMessage string    `json:"error_message" bson:"error_message"`
	Stacktrace   string    `json:"stacktrace,omitempty" bson:"stacktrace,omitempty"`
	TerminatedAt time.Time `json:"terminated_at" bson:"terminated_at"`
}

// JobStatusResponse is the standard response envelope for job queries.
type JobStatusResponse struct {
	JobID            string                 `json:"job_id" bson:"_id"`
	RequestID        string                 `json:"request_id" bson:"request_id"`
	Name             string                 `json:"name" bson:"name"`
	WorkloadID       string                 `json:"workload_id" bson:"workload_id"`
	Status           JobState               `json:"status" bson:"status"`
	SubStatus        string                 `json:"sub_status,omitempty" bson:"sub_status,omitempty"`
	PendingReason    *PendingReason         `json:"pending_reason,omitempty" bson:"pending_reason,omitempty"`
	PendingDetails   *PendingDetails        `json:"pending_details,omitempty" bson:"pending_details,omitempty"`
	FailureReason    *FailureReason         `json:"failure_reason,omitempty" bson:"failure_reason,omitempty"`
	FailureDetails   *FailureDetails        `json:"failure_details,omitempty" bson:"failure_details,omitempty"`
	RequestedVRAMMiB int                    `json:"requested_vram_mib" bson:"requested_vram_mib"`
	Priority         int                    `json:"priority" bson:"priority"`
	Parameters       map[string]interface{} `json:"parameters,omitempty" bson:"parameters,omitempty"`
	Dataset          *DatasetReference      `json:"dataset,omitempty" bson:"dataset,omitempty"`
	Placement        *PlacementDetails      `json:"placement,omitempty" bson:"placement,omitempty"`
	Telemetry        *GPUTelemetrySample    `json:"telemetry,omitempty" bson:"telemetry,omitempty"`
	ExecutionMetrics *ExecutionMetrics      `json:"execution_metrics,omitempty" bson:"execution_metrics,omitempty"`
	Artifacts        []ArtifactMetadata     `json:"artifacts,omitempty" bson:"artifacts,omitempty"`
	Timestamps       JobTimestamps          `json:"timestamps" bson:"timestamps"`
}

// JobSummary provides a lightweight view for history listing.
type JobSummary struct {
	JobID            string   `json:"job_id"`
	Name             string   `json:"name"`
	WorkloadID       string   `json:"workload_id"`
	Status           JobState `json:"status"`
	RequestedVRAMMiB int      `json:"requested_vram_mib"`
	NodeID           *string  `json:"node_id,omitempty"`
	SubmittedAt      time.Time `json:"submitted_at"`
	DurationSeconds  *float64  `json:"duration_seconds,omitempty"`
}

// PaginationMetadata standardizes list pagination across APIs.
type PaginationMetadata struct {
	CurrentPage int `json:"current_page"`
	PageSize    int `json:"page_size"`
	TotalItems  int `json:"total_items"`
	TotalPages  int `json:"total_pages"`
}

// JobListResponse is the paginated envelope for job histories.
type JobListResponse struct {
	Items      []JobSummary       `json:"items"`
	Pagination PaginationMetadata `json:"pagination"`
}

