/**
 * CGPU Platform API TypeScript Definitions
 * 
 * Target: Next.js Web Portal
 * Issue: #8 (Issue B1: Define the job API and prepare mock responses)
 */

export type JobState = 'PENDING' | 'RUNNING' | 'SUCCEEDED' | 'FAILED';

export type PendingReason =
  | 'WAITING_FOR_CAPACITY'
  | 'QUEUED_FIFO'
  | 'SCHEDULING_IN_PROGRESS'
  | 'PULLING_IMAGE'
  | 'CONTAINER_CREATING'
  | 'NODE_DRAINING_OR_CORDONED';

export type FailureReason =
  | 'CUDA_OUT_OF_MEMORY'
  | 'CONTAINER_NON_ZERO_EXIT'
  | 'INVALID_WORKLOAD_PARAMS'
  | 'DATASET_DOWNLOAD_FAILED'
  | 'SCHEDULING_TIMEOUT'
  | 'NODE_FAILURE'
  | 'CANCELLED_BY_USER';

export type TelemetryStatus = 'AVAILABLE' | 'UNAVAILABLE';

export type DatasetType = 'builtin' | 'http' | 's3' | 'pvc';

export interface DatasetReference {
  type: DatasetType;
  name?: string;
  uri?: string;
  checksum_sha256?: string;
}

/**
 * Payload sent by the Next.js portal to submit a new GPU job.
 */
export interface JobSubmissionRequest {
  request_id: string; // Client-generated UUIDv4 for retry handling
  name: string;
  workload_id: string; // e.g. "pytorch-resnet18-cifar10"
  requested_vram_mib: number; // Positive integer in MiB (e.g. 2048)
  parameters?: Record<string, unknown>;
  dataset?: DatasetReference;
  priority?: number; // 1-10, default 5
  timeout_seconds?: number; // default 3600
}

export interface PlacementDetails {
  node_id: string;
  gpu_id: string;
  assigned_gpu_index: number;
  allocated_vram_mib: number;
  enforcement_mode: string;
  placement_reason: string;
  scheduled_at: string;
}

export interface GPUTelemetrySample {
  status: TelemetryStatus;
  reason?: string;
  message?: string;
  gpu_utilization_pct?: number | null;
  memory_used_mib?: number | null;
  temperature_celsius?: number | null;
  power_watts?: number | null;
  /** Alias used by the GPU inventory mock (07_gpu_inventory.json); prefer power_watts. */
  power_draw_watts?: number | null;
  /** Alias used in the spec's inventory example; prefer gpu_utilization_pct. */
  utilization_pct?: number | null;
  sampled_at: string;
}

export interface ArtifactMetadata {
  artifact_id: string;
  name: string;
  artifact_type: 'checkpoint' | 'model_weights' | 'metrics' | 'logs' | 'tensorboard';
  size_bytes: number;
  checksum_sha256: string;
  download_url: string;
  created_at: string;
}

export interface ExecutionMetrics {
  final_loss?: number | null;
  final_accuracy?: number | null;
  peak_vram_mib?: number | null;
  total_duration_seconds?: number | null;
  epochs_completed?: number | null;
  samples_per_second?: number | null;
}

export interface JobTimestamps {
  submitted_at: string;
  scheduled_at?: string | null;
  started_at?: string | null;
  completed_at?: string | null;
}

export interface PendingDetails {
  message: string;
  queue_position?: number;
  required_vram_mib?: number;
  available_vram_mib?: number;
  pool_total_vram_mib?: number;
  pool_allocated_vram_mib?: number;
  estimated_wait_seconds?: number;
}

export interface FailureDetails {
  exit_code: number;
  error_message: string;
  stacktrace?: string;
  terminated_at: string;
}

export interface JobStatusResponse {
  job_id: string;
  request_id: string;
  name: string;
  workload_id: string;
  status: JobState;
  sub_status?: string;
  pending_reason?: PendingReason | null;
  pending_details?: PendingDetails | null;
  failure_reason?: FailureReason | null;
  failure_details?: FailureDetails | null;
  requested_vram_mib: number;
  priority: number;
  parameters?: Record<string, unknown>;
  dataset?: DatasetReference;
  placement?: PlacementDetails | null;
  telemetry?: GPUTelemetrySample | null;
  execution_metrics?: ExecutionMetrics | null;
  artifacts: ArtifactMetadata[];
  timestamps: JobTimestamps;
}

export interface JobSummary {
  job_id: string;
  name: string;
  workload_id: string;
  status: JobState;
  requested_vram_mib: number;
  node_id?: string | null;
  submitted_at: string;
  duration_seconds?: number | null;
}

export interface PaginationMetadata {
  current_page: number;
  page_size: number;
  total_items: number;
  total_pages: number;
}

export interface JobListResponse {
  items: JobSummary[];
  pagination: PaginationMetadata;
}

export interface LogLine {
  timestamp: string;
  stream: 'stdout' | 'stderr';
  message: string;
}

export interface JobLogsResponse {
  job_id: string;
  lines: LogLine[];
}

export interface ActiveAllocation {
  job_id: string;
  job_name: string;
  allocated_vram_mib: number;
  container_id?: string;
  allocated_at: string;
}

export interface GPUDevice {
  gpu_id: string;
  index: number;
  name: string;
  pci_bus_id?: string;
  driver_version?: string;
  cuda_version?: string;
  compute_capability?: string;
  total_vram_mib: number;
  allocated_vram_mib: number;
  free_vram_mib: number;
  health_status: 'HEALTHY' | 'DEGRADED' | 'UNHEALTHY';
  telemetry?: GPUTelemetrySample;
  active_allocations: ActiveAllocation[];
}

export interface GPUNode {
  node_id: string;
  hostname: string;
  status: 'ONLINE' | 'OFFLINE' | 'MAINTENANCE';
  ip_address?: string;
  kubernetes_node: string;
  labels?: Record<string, string>;
  gpus: GPUDevice[];
}

export interface GPUInventoryResponse {
  total_nodes: number;
  total_gpus: number;
  aggregate_vram_mib: number;
  allocated_vram_mib: number;
  free_vram_mib: number;
  nodes: GPUNode[];
}

export interface ValidationErrorDetail {
  field: string;
  rejected_value?: unknown;
  message: string;
}

export interface ErrorResponse {
  error_code: string;
  message: string;
  details?: ValidationErrorDetail[];
  timestamp: string;
}


// ---------------------------------------------------------------------------
// Additions for the Next.js portal (Issue #13 / B3)
// ---------------------------------------------------------------------------

export type WorkloadParameterType = 'integer' | 'number' | 'string' | 'enum';

export interface WorkloadParameter {
  name: string;
  type: WorkloadParameterType;
  default: number | string;
  min?: number;
  max?: number;
  options?: string[];
  description: string;
}

/** Entry of the approved workload catalog served by GET /api/v1/workloads. */
export interface WorkloadDefinition {
  workload_id: string;
  name: string;
  description: string;
  default_vram_mib: number;
  min_vram_mib: number;
  max_vram_mib: number;
  parameters: WorkloadParameter[];
  /**
   * Set by the portal, never by the API: true when this entry comes from the
   * built-in catalog and the running API does not list it, so the workload
   * exists in this checkout but the deployed API would reject a submission of
   * it as unknown. Such entries are shown (so a newly added workload is
   * discoverable) but cannot be submitted until the API is redeployed.
   */
  pending_api_deploy?: boolean;
}

/** GET /api/v1/workloads */
export interface WorkloadListResponse {
  items: WorkloadDefinition[];
}

/** GET /api/v1/jobs/{job_id}/artifacts */
export interface ArtifactListResponse {
  job_id: string;
  artifacts: ArtifactMetadata[];
}

/** Query parameters accepted by GET /api/v1/jobs */
export interface JobListQuery {
  status?: JobState;
  workload_id?: string;
  page?: number;
  limit?: number;
}
