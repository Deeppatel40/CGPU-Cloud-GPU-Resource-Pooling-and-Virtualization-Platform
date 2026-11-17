/** Human-readable explanations for machine reason codes from the API. */

interface ReasonText {
  title: string;
  help: string;
}

export const PENDING_REASONS: Record<string, ReasonText> = {
  WAITING_FOR_CAPACITY: {
    title: 'Waiting for GPU capacity',
    help: 'No GPU in the pool currently has enough unreserved VRAM for this request. The job will be placed automatically as soon as another job finishes and frees its slice.',
  },
  QUEUED_FIFO: {
    title: 'Queued behind earlier jobs',
    help: 'Jobs are placed in submission order (FIFO). This job starts after the jobs ahead of it are placed.',
  },
  SCHEDULING_IN_PROGRESS: {
    title: 'Scheduler is choosing a GPU',
    help: 'The job was accepted and the placement engine is selecting a node and GPU slice.',
  },
  PULLING_IMAGE: {
    title: 'Pulling the workload image',
    help: 'A GPU slice has been reserved; the worker node is downloading the container image.',
  },
  CONTAINER_CREATING: {
    title: 'Starting the container',
    help: 'The image is present and the container is being created with the GPU slice attached.',
  },
  NODE_DRAINING_OR_CORDONED: {
    title: 'Target node is unavailable',
    help: 'The selected worker node is draining or cordoned (for example it is going to sleep). The job will be re-placed.',
  },
};

export const FAILURE_REASONS: Record<string, ReasonText> = {
  CUDA_OUT_OF_MEMORY: {
    title: 'Out of GPU memory',
    help: 'The workload tried to use more VRAM than its slice allows. Re-submit with a larger VRAM request or a smaller batch size.',
  },
  CONTAINER_NON_ZERO_EXIT: {
    title: 'Workload exited with an error',
    help: 'The training process terminated with a non-zero exit code. Check the logs and the error message below.',
  },
  INVALID_WORKLOAD_PARAMS: {
    title: 'Invalid workload parameters',
    help: 'The workload rejected its parameters at start-up. Check the parameter values and re-submit.',
  },
  DATASET_DOWNLOAD_FAILED: {
    title: 'Dataset download failed',
    help: 'The worker could not fetch the dataset. This is usually a network problem on the worker node.',
  },
  SCHEDULING_TIMEOUT: {
    title: 'Timed out waiting for a GPU',
    help: 'No GPU with enough free VRAM became available before the job timeout. Try a smaller VRAM request or submit when the pool is less busy.',
  },
  NODE_FAILURE: {
    title: 'Worker node failed',
    help: 'The worker node went offline while the job was running (e.g. the laptop went to sleep or lost network). Its GPU reservation was released.',
  },
  CANCELLED_BY_USER: {
    title: 'Cancelled by user',
    help: 'The job was cancelled from the portal or API and its GPU reservation was released.',
  },
};

export const SUB_STATUS_TEXT: Record<string, string> = {
  INITIALIZING: 'Initializing CUDA and GPU slice',
  COMPUTING: 'Training',
  SAVING_CHECKPOINT: 'Saving checkpoint',
  COMPLETED: 'Completed',
  COMPLETED_SUCCESSFULLY: 'Completed',
  TERMINATED: 'Terminated',
};

function humanize(code: string): string {
  return code
    .toLowerCase()
    .split('_')
    .map((w, i) => (i === 0 ? w.charAt(0).toUpperCase() + w.slice(1) : w))
    .join(' ');
}

export function pendingReason(code: string | null | undefined): ReasonText | null {
  if (!code) return null;
  return PENDING_REASONS[code] ?? { title: humanize(code), help: '' };
}

export function failureReason(code: string | null | undefined): ReasonText | null {
  if (!code) return null;
  return FAILURE_REASONS[code] ?? { title: humanize(code), help: '' };
}

export function subStatusText(code: string | null | undefined): string | null {
  if (!code) return null;
  return SUB_STATUS_TEXT[code] ?? PENDING_REASONS[code]?.title ?? humanize(code);
}
