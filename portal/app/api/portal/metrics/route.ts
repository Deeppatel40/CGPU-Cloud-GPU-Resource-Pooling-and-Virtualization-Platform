/**
 * Live GPU telemetry for the portal, read from Prometheus.
 *
 * Why this exists alongside the API's own telemetry field:
 *   /api/v1/gpus carries a `telemetry` sample per GPU, but the API produces it
 *   by probing each worker agent at the address the scheduler holds for that
 *   node - and that address is a hardcoded constant (cmd/cgpu-scheduler
 *   bootstrapClusterNodes). The GPU workers are laptops whose IP changes on
 *   every rejoin, so the probe reports AGENT_UNREACHABLE and every measured
 *   figure on the page renders "unavailable".
 *
 *   Prometheus already scrapes the same agents and keeps the history, so
 *   reading from it gives the portal real numbers over a path that cannot go
 *   stale the same way: Prometheus discovers targets itself (or, on a worker,
 *   scrapes over loopback). It also brings per-job VRAM, peaks and denial
 *   counts, which the API does not expose at all.
 *
 * Security notes:
 *   - Queries are allowlisted by key. The browser sends `?keys=gpu,jobs`, never
 *     PromQL, so this endpoint cannot be turned into an open query proxy
 *     against the monitoring network.
 *   - It runs server-side, so Prometheus need not be reachable from browsers
 *     and the portal stays the only exposed origin.
 */
import { NextResponse } from 'next/server';

export const dynamic = 'force-dynamic';

const PROM = (process.env.CGPU_PROMETHEUS_URL || 'http://127.0.0.1:9090').replace(/\/+$/, '');

/** Instant queries the portal is allowed to ask for, by group. */
const QUERIES = {
  gpu: {
    utilization_percent: 'cgpu_gpu_utilization_percent',
    temperature_celsius: 'cgpu_gpu_temperature_celsius',
    power_watts: 'cgpu_gpu_power_draw_watts',
    memory_total_mib: 'cgpu_gpu_memory_total_mib',
    memory_used_mib: 'cgpu_gpu_memory_used_mib',
    memory_allocatable_mib: 'cgpu_gpu_memory_allocatable_mib',
    healthy: 'cgpu_gpu_healthy',
    active_jobs: 'cgpu_gpu_active_jobs',
  },
  jobs: {
    used_bytes: 'cgpu_job_vram_used_bytes',
    peak_bytes: 'cgpu_job_vram_peak_bytes',
    quota_bytes: 'cgpu_job_vram_quota_bytes',
    denials_total: 'cgpu_job_vram_denials_total',
  },
  pool: {
    total_nodes: 'cgpu_pool_total_nodes',
    total_gpus: 'cgpu_pool_total_gpus',
    vram_total_mib: 'cgpu_pool_vram_total_mib',
  },
  targets: {
    // Which scrape targets are alive. This is how the portal can say "telemetry
    // is missing because the agent is down" instead of just showing blanks.
    up: 'up',
  },
} as const;

type Group = keyof typeof QUERIES;

interface PromSample {
  metric: Record<string, string>;
  value: [number, string];
}

async function instant(query: string, signal: AbortSignal): Promise<PromSample[]> {
  const url = `${PROM}/api/v1/query?query=${encodeURIComponent(query)}`;
  const res = await fetch(url, { cache: 'no-store', signal });
  if (!res.ok) throw new Error(`prometheus HTTP ${res.status}`);
  const body = (await res.json()) as {
    status: string;
    data?: { resultType: string; result: PromSample[] };
    error?: string;
  };
  if (body.status !== 'success') throw new Error(body.error || 'prometheus query failed');
  return body.data?.result ?? [];
}

const num = (s: PromSample) => Number(s.value[1]);

export async function GET(req: Request) {
  const requested = new URL(req.url).searchParams.get('keys');
  const groups: Group[] = requested
    ? (requested.split(',').filter((k): k is Group => k in QUERIES) as Group[])
    : (Object.keys(QUERIES) as Group[]);

  // One deadline for the whole fan-out: a slow Prometheus must not hang the
  // page, which polls this every few seconds.
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), 6000);

  try {
    const flat: Array<[Group, string, PromSample[]]> = [];
    await Promise.all(
      groups.flatMap((g) =>
        Object.entries(QUERIES[g]).map(async ([field, expr]) => {
          try {
            flat.push([g, field, await instant(expr, ac.signal)]);
          } catch {
            // A single missing metric family (nothing has reported it yet) is
            // normal and must not fail the whole response.
            flat.push([g, field, []]);
          }
        }),
      ),
    );

    // ---- shape into something the UI can render directly ----
    // Keyed by node, then by GPU uuid, so a multi-GPU worker stays correct.
    // Accumulators carry identifying strings alongside the numeric fields, so
    // the index signature has to admit both rather than numbers alone.
    type GpuAcc = { uuid: string; model?: string; [field: string]: number | string | undefined };
    type JobAcc = { job_id: string; node?: string; [field: string]: number | string | undefined };

    const nodes: Record<string, { node: string; gpus: Record<string, GpuAcc>; active_jobs?: number }> = {};
    const jobs: Record<string, JobAcc> = {};
    const pool: Record<string, number> = {};
    const targets: Array<{ job: string; instance: string; up: number }> = [];

    for (const [group, field, samples] of flat) {
      for (const s of samples) {
        if (group === 'gpu') {
          const node = s.metric.node ?? 'unknown';
          const uuid = s.metric.uuid ?? s.metric.gpu ?? '0';
          nodes[node] ??= { node, gpus: {} };
          if (field === 'active_jobs') {
            nodes[node].active_jobs = num(s);
            continue;
          }
          nodes[node].gpus[uuid] ??= { uuid, model: s.metric.model };
          nodes[node].gpus[uuid][field] = num(s);
        } else if (group === 'jobs') {
          const id = s.metric.job_id ?? 'unknown';
          jobs[id] ??= { job_id: id, node: s.metric.node };
          jobs[id][field] = num(s);
        } else if (group === 'pool') {
          pool[field] = num(s);
        } else if (group === 'targets') {
          targets.push({
            job: s.metric.job ?? '?',
            instance: s.metric.instance ?? '?',
            up: num(s),
          });
        }
      }
    }

    return NextResponse.json(
      {
        source: 'prometheus',
        prometheus_url: PROM,
        scraped_at: new Date().toISOString(),
        nodes: Object.values(nodes).map((n) => ({ ...n, gpus: Object.values(n.gpus) })),
        jobs: Object.values(jobs),
        pool,
        targets,
      },
      { headers: { 'Cache-Control': 'no-store' } },
    );
  } catch (err) {
    // Distinguish "monitoring is not deployed/reachable" from "no data yet":
    // the UI shows a setup hint for the former and an empty state for the latter.
    return NextResponse.json(
      {
        error_code: 'PROMETHEUS_UNREACHABLE',
        message: err instanceof Error ? err.message : 'cannot reach Prometheus',
        prometheus_url: PROM,
        hint:
          'Set CGPU_PROMETHEUS_URL to the Prometheus address and make sure it is running ' +
          '(deploy/monitoring/prometheus.yaml, or the local containers on a worker).',
      },
      { status: 503, headers: { 'Cache-Control': 'no-store' } },
    );
  } finally {
    clearTimeout(timer);
  }
}
