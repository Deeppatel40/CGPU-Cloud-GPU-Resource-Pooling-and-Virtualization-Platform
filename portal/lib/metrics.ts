/**
 * Browser-side client for the portal's Prometheus-backed telemetry endpoint.
 *
 * Same-origin by design: the route handler at /api/portal/metrics talks to
 * Prometheus server-side, so Prometheus itself never has to be reachable from
 * a browser and the portal stays the only exposed origin.
 */

export interface GpuLive {
  uuid: string;
  model?: string;
  utilization_percent?: number;
  temperature_celsius?: number;
  power_watts?: number;
  memory_total_mib?: number;
  memory_used_mib?: number;
  memory_allocatable_mib?: number;
  healthy?: number;
}

export interface NodeLive {
  node: string;
  gpus: GpuLive[];
  active_jobs?: number;
}

export interface JobLive {
  job_id: string;
  node?: string;
  used_bytes?: number;
  peak_bytes?: number;
  quota_bytes?: number;
  denials_total?: number;
}

export interface ScrapeTarget {
  job: string;
  instance: string;
  up: number;
}

export interface MetricsSnapshot {
  source: 'prometheus';
  prometheus_url: string;
  scraped_at: string;
  nodes: NodeLive[];
  jobs: JobLive[];
  pool: Partial<Record<'total_nodes' | 'total_gpus' | 'vram_total_mib', number>>;
  targets: ScrapeTarget[];
}

/** Thrown when Prometheus is not reachable, carrying the setup hint to show. */
export class MetricsError extends Error {
  readonly hint?: string;
  readonly promUrl?: string;
  constructor(message: string, hint?: string, promUrl?: string) {
    super(message);
    this.name = 'MetricsError';
    this.hint = hint;
    this.promUrl = promUrl;
  }
}

export async function fetchMetrics(keys?: string[]): Promise<MetricsSnapshot> {
  const qs = keys?.length ? `?keys=${encodeURIComponent(keys.join(','))}` : '';
  let res: Response;
  try {
    res = await fetch(`/api/portal/metrics${qs}`, { cache: 'no-store' });
  } catch {
    throw new MetricsError('Cannot reach the portal server.');
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    throw new MetricsError(
      (body?.message as string) || `metrics request failed (HTTP ${res.status})`,
      body?.hint as string | undefined,
      body?.prometheus_url as string | undefined,
    );
  }
  return body as MetricsSnapshot;
}

/** MiB from a byte count, for the per-job figures the guard reports in bytes. */
export const bytesToMiB = (b?: number) => (b == null ? undefined : b / (1024 * 1024));
