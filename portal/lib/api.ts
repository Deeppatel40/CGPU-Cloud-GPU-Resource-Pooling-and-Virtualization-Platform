/**
 * Browser-side client for the CGPU job API.
 *
 * All calls are same-origin (`/api/v1/...`); next.config.ts rewrites them to
 * CGPU_API_URL. Live log streaming goes through the route handler at
 * /api/portal/log-stream/{job_id} instead (see LogViewer).
 */
import type {
  ArtifactListResponse,
  ErrorResponse,
  GPUInventoryResponse,
  JobListQuery,
  JobListResponse,
  JobLogsResponse,
  JobStatusResponse,
  JobSubmissionRequest,
  WorkloadDefinition,
  WorkloadListResponse,
} from '@/types/api';
import { BUILTIN_WORKLOADS } from './workloads';

export const API_BASE = '/api/v1';

export class ApiError extends Error {
  readonly status: number;
  readonly body?: ErrorResponse;
  /** True when the request never got an HTTP response (API/proxy unreachable). */
  readonly network: boolean;

  constructor(status: number, message: string, body?: ErrorResponse, network = false) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
    this.network = network;
  }
}

function isErrorResponse(v: unknown): v is ErrorResponse {
  return (
    typeof v === 'object' &&
    v !== null &&
    typeof (v as ErrorResponse).message === 'string' &&
    typeof (v as ErrorResponse).error_code === 'string'
  );
}

async function request<T>(path: string, init?: RequestInit): Promise<{ data: T; status: number }> {
  let res: Response;
  try {
    res = await fetch(API_BASE + path, {
      cache: 'no-store',
      ...init,
      headers: { Accept: 'application/json', ...(init?.headers ?? {}) },
    });
  } catch {
    throw new ApiError(0, 'Cannot reach the portal server. Check your network connection.', undefined, true);
  }

  const text = await res.text();
  let json: unknown;
  if (text) {
    try {
      json = JSON.parse(text);
    } catch {
      json = undefined;
    }
  }

  if (!res.ok) {
    if (isErrorResponse(json)) {
      throw new ApiError(res.status, json.message, json);
    }
    // The Next.js proxy answers 500/502/504 with a non-JSON body when the
    // upstream Go API is down.
    const unreachable = res.status >= 500 && json === undefined;
    throw new ApiError(
      res.status,
      unreachable
        ? `The CGPU API is unreachable (HTTP ${res.status}). Is the API server running?`
        : `Request failed with HTTP ${res.status}.`,
      undefined,
      unreachable,
    );
  }
  return { data: json as T, status: res.status };
}

const enc = encodeURIComponent;

// ---------------------------------------------------------------------------
// Workload catalog
// ---------------------------------------------------------------------------

export interface WorkloadCatalog {
  items: WorkloadDefinition[];
  source: 'api' | 'builtin';
  /** Set when we fell back to the built-in catalog for a reason other than 404. */
  warning?: string;
}

/**
 * Appends built-in workloads the running API does not list.
 *
 * The API's catalog is authoritative for what can actually be submitted - it is
 * the approved-workload list, and it maps an id to an image and command. But a
 * workload added to this repo only appears there once the API is redeployed,
 * which on this cluster is a separate machine. Hiding it until then makes a new
 * workload look missing rather than undeployed, which is the confusing failure.
 *
 * So extras are shown and flagged instead, and the submit form refuses to send
 * them - a visible entry that silently fails on submit would be worse than not
 * showing it at all.
 */
function withPendingExtras(apiItems: WorkloadDefinition[]): WorkloadDefinition[] {
  const known = new Set(apiItems.map((w) => w.workload_id));
  const extras = BUILTIN_WORKLOADS.filter((w) => !known.has(w.workload_id)).map((w) => ({
    ...w,
    pending_api_deploy: true,
  }));
  return [...apiItems, ...extras];
}

export async function listWorkloads(): Promise<WorkloadCatalog> {
  try {
    const { data } = await request<WorkloadListResponse>('/workloads');
    if (Array.isArray(data?.items) && data.items.length > 0) {
      return { items: withPendingExtras(data.items), source: 'api' };
    }
    return { items: BUILTIN_WORKLOADS, source: 'builtin', warning: 'The API returned an empty workload catalog.' };
  } catch (e) {
    if (e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501)) {
      // Endpoint not implemented (e.g. python mock): use the built-in catalog.
      return { items: BUILTIN_WORKLOADS, source: 'builtin' };
    }
    return {
      items: BUILTIN_WORKLOADS,
      source: 'builtin',
      warning: `Could not load the workload catalog (${describeError(e)}).`,
    };
  }
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

const RETRYABLE_STATUS = new Set([0, 502, 503, 504]);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * POST /api/v1/jobs with automatic retries on transport failures.
 *
 * The caller generates `request_id` once per form submission; every retry here
 * (and any manual "retry" by the user) re-sends the identical body, so the API
 * returns the already-created job (200) instead of creating a duplicate (201).
 */
export async function submitJob(
  body: JobSubmissionRequest,
  opts: { retries?: number; onRetry?: (attempt: number) => void } = {},
): Promise<{ job: JobStatusResponse; created: boolean; status: number }> {
  const retries = opts.retries ?? 2;
  let lastErr: unknown;
  for (let attempt = 0; attempt <= retries; attempt++) {
    if (attempt > 0) {
      opts.onRetry?.(attempt);
      await sleep(500 * 2 ** (attempt - 1));
    }
    try {
      const { data, status } = await request<JobStatusResponse>('/jobs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Request-Id': body.request_id },
        body: JSON.stringify(body),
      });
      return { job: data, created: status === 201, status };
    } catch (e) {
      lastErr = e;
      if (!(e instanceof ApiError) || !RETRYABLE_STATUS.has(e.status)) throw e;
    }
  }
  throw lastErr;
}

export async function listJobs(q: JobListQuery = {}): Promise<JobListResponse> {
  const params = new URLSearchParams();
  if (q.status) params.set('status', q.status);
  if (q.workload_id) params.set('workload_id', q.workload_id);
  if (q.page) params.set('page', String(q.page));
  if (q.limit) params.set('limit', String(q.limit));
  const qs = params.toString();
  const { data } = await request<JobListResponse>(`/jobs${qs ? `?${qs}` : ''}`);
  return {
    items: Array.isArray(data?.items) ? data.items : [],
    pagination: data?.pagination ?? { current_page: 1, page_size: q.limit ?? 20, total_items: 0, total_pages: 1 },
  };
}

export async function getJob(jobId: string): Promise<JobStatusResponse> {
  const { data } = await request<JobStatusResponse>(`/jobs/${enc(jobId)}`);
  return data;
}

export async function cancelJob(jobId: string): Promise<Partial<JobStatusResponse>> {
  const { data } = await request<Partial<JobStatusResponse>>(`/jobs/${enc(jobId)}`, { method: 'DELETE' });
  return data ?? {};
}

export async function getJobLogs(jobId: string, tail = 500): Promise<JobLogsResponse> {
  const { data } = await request<JobLogsResponse>(`/jobs/${enc(jobId)}/logs?tail=${tail}`);
  return { job_id: data?.job_id ?? jobId, lines: Array.isArray(data?.lines) ? data.lines : [] };
}

export async function getJobArtifacts(jobId: string): Promise<ArtifactListResponse> {
  const { data } = await request<ArtifactListResponse>(`/jobs/${enc(jobId)}/artifacts`);
  return { job_id: data?.job_id ?? jobId, artifacts: Array.isArray(data?.artifacts) ? data.artifacts : [] };
}

/** Same-origin SSE endpoint (route handler that proxies `logs?stream=true`). */
export function logStreamUrl(jobId: string, tail = 500): string {
  return `/api/portal/log-stream/${enc(jobId)}?tail=${tail}`;
}

/**
 * Artifact download links are relative API paths (e.g. /api/v1/jobs/.../download),
 * which the rewrite proxies. Absolute URLs (signed URLs) are passed through.
 */
export function artifactHref(downloadUrl: string): string {
  if (/^https?:\/\//i.test(downloadUrl)) return downloadUrl;
  return downloadUrl.startsWith('/') ? downloadUrl : `/${downloadUrl}`;
}

// ---------------------------------------------------------------------------
// GPU inventory
// ---------------------------------------------------------------------------

export async function getInventory(): Promise<GPUInventoryResponse> {
  const { data } = await request<GPUInventoryResponse>('/gpus');
  return { ...data, nodes: Array.isArray(data?.nodes) ? data.nodes : [] };
}

// ---------------------------------------------------------------------------

export function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.body ? `${e.message} [${e.body.error_code}]` : e.message;
  }
  if (e instanceof Error) return e.message;
  return String(e);
}
