'use client';

/**
 * Live telemetry panel, sourced from Prometheus rather than the job API.
 *
 * The API's own per-GPU `telemetry` field is produced by probing each worker at
 * the address the scheduler holds, which is hardcoded - so on a laptop fleet it
 * reports AGENT_UNREACHABLE and every measured value renders "unavailable".
 * Prometheus scrapes the same agents and cannot go stale that way, so this
 * panel shows real numbers on a page where the API's own fields are blank.
 *
 * It also surfaces what the API does not expose at all: per-job VRAM against
 * the quota the guard is enforcing, the high-water mark, and denial counts.
 */

import { usePolling } from '@/lib/usePolling';
import { fetchMetrics, MetricsError, bytesToMiB, type GpuLive, type JobLive } from '@/lib/metrics';
import { Updated } from './ui';

const REFRESH_MS = 5000;

const fmt = (v: number | undefined, unit = '', digits = 0) =>
  v == null ? <span className="unavailable">unavailable</span> : `${v.toFixed(digits)}${unit}`;

const mib = (v: number | undefined) =>
  v == null ? <span className="unavailable">unavailable</span> : `${Math.round(v).toLocaleString()} MiB`;

/** Utilisation/VRAM bar. Width is driven by data, so it animates as values move. */
function Meter({ pct, tone = 'reserved' }: { pct: number; tone?: 'reserved' | 'measured' }) {
  const clamped = Math.max(0, Math.min(100, pct));
  return (
    <div className="vbar" style={{ height: 8 }}>
      <div className={`vbar-seg ${tone}`} style={{ width: `${clamped}%`, height: '100%' }} />
    </div>
  );
}

function GpuCard({ node, gpu, activeJobs }: { node: string; gpu: GpuLive; activeJobs?: number }) {
  const used = gpu.memory_used_mib;
  const total = gpu.memory_total_mib;
  const usedPct = used != null && total ? (used / total) * 100 : 0;

  return (
    <div className="node">
      <div className="node-head">
        <div>
          <h3>{node}</h3>
          <div className="muted small mono">{gpu.model ?? gpu.uuid}</div>
        </div>
        <div className="head-actions">
          {gpu.healthy != null && (
            <span className={`badge ${gpu.healthy ? 'ok' : 'err'}`}>
              {gpu.healthy ? 'HEALTHY' : 'UNHEALTHY'}
            </span>
          )}
          {/* Only pulse when work is actually on the card: motion should mean something. */}
          {activeJobs ? <span className="badge info pulse">{activeJobs} RUNNING</span> : null}
        </div>
      </div>

      <div className="node-body">
        <div className="tiles">
          <div className="tile">
            <div className="tile-label">GPU utilisation</div>
            <div className="tile-value">{fmt(gpu.utilization_percent, '%')}</div>
            <Meter pct={gpu.utilization_percent ?? 0} />
          </div>
          <div className="tile">
            <div className="tile-label">VRAM in use</div>
            <div className="tile-value">{mib(used)}</div>
            <Meter pct={usedPct} tone="measured" />
            <div className="tile-sub">
              of {total ? `${total.toLocaleString()} MiB` : '—'}
              {gpu.memory_allocatable_mib != null && (
                <> · {Math.round(gpu.memory_allocatable_mib).toLocaleString()} MiB allocatable</>
              )}
            </div>
          </div>
          <div className="tile">
            <div className="tile-label">Temperature</div>
            <div className="tile-value">{fmt(gpu.temperature_celsius, '°C')}</div>
          </div>
          <div className="tile">
            <div className="tile-label">Power draw</div>
            <div className="tile-value">{fmt(gpu.power_watts, ' W', 1)}</div>
          </div>
        </div>
      </div>
    </div>
  );
}

function JobRow({ j }: { j: JobLive }) {
  const used = bytesToMiB(j.used_bytes);
  const peak = bytesToMiB(j.peak_bytes);
  const quota = bytesToMiB(j.quota_bytes);
  const pct = used != null && quota ? (used / quota) * 100 : 0;
  return (
    <tr>
      <td className="mono nowrap">{j.job_id}</td>
      <td className="nowrap">{mib(used)}</td>
      <td className="nowrap">{mib(quota)}</td>
      <td style={{ minWidth: 120 }}>
        <Meter pct={pct} />
        <div className="muted small">{quota ? `${pct.toFixed(0)}% of quota` : ''}</div>
      </td>
      <td className="nowrap">{mib(peak)}</td>
      <td className="nowrap">
        {j.denials_total ? (
          <span className="badge err">{j.denials_total} DENIED</span>
        ) : (
          <span className="muted">0</span>
        )}
      </td>
    </tr>
  );
}

export default function LiveMetrics() {
  const m = usePolling(() => fetchMetrics(), REFRESH_MS);

  if (m.error instanceof MetricsError) {
    // Monitoring not deployed is a setup state, not a crash: say what to do.
    return (
      <section className="stack">
        <div className="card-head">
          <h2>Live telemetry</h2>
        </div>
        <div className="alert warn" role="status">
          <div className="alert-title">Prometheus is not reachable</div>
          <p>{m.error.message}</p>
          {m.error.hint && <p className="muted small">{m.error.hint}</p>}
        </div>
      </section>
    );
  }

  const snap = m.data;
  const down = snap?.targets.filter((t) => !t.up) ?? [];
  const jobs = (snap?.jobs ?? []).sort((a, b) => (b.used_bytes ?? 0) - (a.used_bytes ?? 0));

  return (
    <section className="stack">
      <div className="card-head">
        <h2>Live telemetry</h2>
        <div className="head-actions">
          <span className="chip">prometheus</span>
          <Updated at={m.updatedAt} live />
        </div>
      </div>

      {m.loading && !snap && <div className="card skeleton">Loading telemetry…</div>}

      {/* A target that is down is why numbers would be missing - surface it
          rather than letting the panel look merely empty. */}
      {down.length > 0 && (
        <div className="alert warn" role="status">
          <div className="alert-title">
            {down.length} scrape target{down.length > 1 ? 's' : ''} down
          </div>
          <p className="muted small mono">{down.map((t) => `${t.job} (${t.instance})`).join(' · ')}</p>
        </div>
      )}

      {snap && snap.nodes.length === 0 && (
        <div className="card empty">
          No GPU metrics yet. Prometheus is reachable but no worker agent has reported — start
          <code> cgpu-worker-agent</code> on a GPU node.
        </div>
      )}

      {snap?.nodes.map((n) =>
        n.gpus.map((g) => <GpuCard key={`${n.node}/${g.uuid}`} node={n.node} gpu={g} activeJobs={n.active_jobs} />),
      )}

      {jobs.length > 0 && (
        <div className="card">
          <div className="card-head">
            <h2>Per-job VRAM against quota</h2>
            <span className="muted small">reported by the allocation guard</span>
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Job</th>
                  <th>Used</th>
                  <th>Quota</th>
                  <th>Share of quota</th>
                  <th>Peak</th>
                  <th>Denials</th>
                </tr>
              </thead>
              <tbody>
                {jobs.map((j) => (
                  <JobRow key={j.job_id} j={j} />
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            On WSL workers the driver reports no per-process VRAM, so these figures come from the
            CGPU guard&apos;s own accounting — it is the only per-job signal available.
          </p>
        </div>
      )}
    </section>
  );
}
