'use client';

import Link from 'next/link';
import type { GPUDevice, GPUInventoryResponse, GPUNode, GPUTelemetrySample } from '@/types/api';
import { getInventory } from '@/lib/api';
import { fmtDate, fmtGiB, fmtMiB } from '@/lib/format';
import { usePolling } from '@/lib/usePolling';
import { DRIVER_RESERVE_MIB } from '@/lib/workloads';
import { CATEGORY_ORDER, CATEGORY_LABEL, PLATFORM_SERVICES, type ServiceEntry } from '@/lib/services';
import { ErrorAlert, HealthBadge, NodeStatusBadge, Updated } from './ui';

const POLL_MS = 5000;

const telemetryOk = (t: GPUTelemetrySample | undefined | null, node: GPUNode) =>
  node.status === 'ONLINE' && t?.status === 'AVAILABLE' && t.memory_used_mib != null;

export interface PoolSummary {
  nodes: number;
  onlineNodes: number;
  offlineNodes: number;
  gpus: number;
  capacity: number;
  allocatable: number;
  reserved: number;
  free: number;
  offlineCapacity: number;
  measured: number | null;
  reportingGpus: number;
}

export function summarize(inv: GPUInventoryResponse): PoolSummary {
  let gpus = 0;
  let capacity = 0;
  let reserved = 0;
  let free = 0;
  let offlineCapacity = 0;
  let measured = 0;
  let reportingGpus = 0;
  let onlineNodes = 0;
  for (const n of inv.nodes) {
    if (n.status === 'ONLINE') onlineNodes++;
    for (const g of n.gpus) {
      gpus++;
      capacity += g.total_vram_mib;
      reserved += g.allocated_vram_mib;
      free += g.free_vram_mib;
      if (n.status !== 'ONLINE') offlineCapacity += g.total_vram_mib;
      if (telemetryOk(g.telemetry, n)) {
        measured += g.telemetry!.memory_used_mib!;
        reportingGpus++;
      }
    }
  }
  return {
    nodes: inv.nodes.length || inv.total_nodes,
    onlineNodes,
    offlineNodes: inv.nodes.length - onlineNodes,
    gpus: gpus || inv.total_gpus,
    capacity: capacity || inv.aggregate_vram_mib,
    allocatable: Math.max(0, capacity - gpus * DRIVER_RESERVE_MIB),
    reserved: inv.nodes.length ? reserved : inv.allocated_vram_mib,
    free: inv.nodes.length ? free : inv.free_vram_mib,
    offlineCapacity,
    measured: reportingGpus > 0 ? measured : null,
    reportingGpus,
  };
}

export function PoolTiles({ s }: { s: PoolSummary }) {
  return (
    <div className="tiles">
      <div className="tile">
        <div className="tile-label">Worker nodes</div>
        <div className="tile-value">
          {s.onlineNodes}
          <span className="muted" style={{ fontSize: 15 }}> / {s.nodes} online</span>
        </div>
        <div className="tile-sub" style={s.offlineNodes ? { color: 'var(--err)' } : undefined}>
          {s.offlineNodes ? `${s.offlineNodes} offline` : 'all online'} · {s.gpus} GPU{s.gpus === 1 ? '' : 's'}
        </div>
      </div>
      <div className="tile">
        <div className="tile-label">Capacity (total VRAM)</div>
        <div className="tile-value">{fmtGiB(s.capacity)}</div>
        <div className="tile-sub">
          {fmtMiB(s.capacity)}
          {s.offlineCapacity > 0 && ` · ${fmtMiB(s.offlineCapacity)} offline`}
        </div>
      </div>
      <div className="tile">
        <div className="tile-label">
          <i style={{ width: 9, height: 9, borderRadius: 2, background: 'var(--reserved)', display: 'inline-block' }} />
          Reserved by scheduler
        </div>
        <div className="tile-value">{fmtGiB(s.reserved)}</div>
        <div className="tile-sub">{fmtMiB(s.reserved)} allocated to jobs</div>
      </div>
      <div className="tile">
        <div className="tile-label">Free (unreserved)</div>
        <div className="tile-value">{fmtGiB(s.free)}</div>
        <div className="tile-sub">
          {fmtMiB(s.free)} · allocatable {fmtMiB(s.allocatable)}
        </div>
      </div>
      <div className="tile">
        <div className="tile-label">
          <i style={{ width: 9, height: 4, background: 'var(--measured)', display: 'inline-block' }} />
          Measured usage
        </div>
        <div className="tile-value">
          {s.measured != null ? fmtGiB(s.measured) : <span className="unavailable">unavailable</span>}
        </div>
        <div className="tile-sub">
          {s.reportingGpus}/{s.gpus} GPU{s.gpus === 1 ? '' : 's'} reporting telemetry
          {s.measured != null && ` · ${fmtMiB(s.measured)}`}
        </div>
      </div>
    </div>
  );
}

function VramBar({ gpu, measured }: { gpu: GPUDevice; measured: number | null }) {
  const total = Math.max(1, gpu.total_vram_mib);
  // Real API: free = (total − 512 driver reserve) − reserved, so the remainder
  // is the driver reserve. Derived rather than assumed so the bar always sums
  // to the API's own numbers.
  const driver = Math.max(0, gpu.total_vram_mib - gpu.allocated_vram_mib - gpu.free_vram_mib);
  const pct = (v: number) => `${Math.max(0, Math.min(100, (v / total) * 100))}%`;
  return (
    <div
      className="vbar"
      role="img"
      aria-label={`Reserved ${gpu.allocated_vram_mib} MiB, free ${gpu.free_vram_mib} MiB of ${gpu.total_vram_mib} MiB`}
    >
      <div className="vbar-seg reserved" style={{ width: pct(gpu.allocated_vram_mib) }} title={`Reserved: ${fmtMiB(gpu.allocated_vram_mib)}`} />
      <div className="vbar-seg free" style={{ width: pct(gpu.free_vram_mib) }} title={`Free: ${fmtMiB(gpu.free_vram_mib)}`} />
      {driver > 0 && <div className="vbar-seg driver" style={{ width: pct(driver) }} title={`Driver reserve: ${fmtMiB(driver)}`} />}
      {measured != null && (
        <div className="vbar-measured" style={{ width: pct(measured) }} title={`Measured in use: ${fmtMiB(measured)}`} />
      )}
    </div>
  );
}

function GpuCard({ gpu, node }: { gpu: GPUDevice; node: GPUNode }) {
  const t = gpu.telemetry;
  const ok = telemetryOk(t, node);
  const measured = ok ? t!.memory_used_mib! : null;
  const unavailableWhy =
    node.status !== 'ONLINE'
      ? `node ${node.status.toLowerCase()}`
      : !t
        ? 'no telemetry reported'
        : t.reason ?? t.message ?? 'telemetry unavailable';
  const allocatable = Math.max(0, gpu.total_vram_mib - DRIVER_RESERVE_MIB);
  const tv = (v: number | null | undefined, unit: string, d = 0) =>
    ok && v != null ? `${v.toFixed(d)} ${unit}` : <span className="unavailable">unavailable</span>;

  return (
    <div className="gpu" id={gpu.gpu_id}>
      <div className="gpu-title">
        <div>
          <h3>
            GPU {gpu.index} · {gpu.name}
          </h3>
          <div className="muted small">
            <span className="mono">{gpu.gpu_id}</span>
            {gpu.driver_version && ` · driver ${gpu.driver_version}`}
            {gpu.cuda_version && ` · CUDA ${gpu.cuda_version}`}
            {gpu.compute_capability && ` · sm ${gpu.compute_capability}`}
            {gpu.pci_bus_id && ` · PCI ${gpu.pci_bus_id}`}
          </div>
        </div>
        <HealthBadge status={gpu.health_status} />
      </div>

      <VramBar gpu={gpu} measured={measured} />
      <div className="legend">
        <span>
          <i className="reserved" />
          Reserved by scheduler
        </span>
        <span>
          <i className="free" />
          Free
        </span>
        <span>
          <i className="driver" />
          Driver reserve ({DRIVER_RESERVE_MIB} MiB)
        </span>
        <span>
          <i className="measured" />
          Measured in use
        </span>
      </div>

      <div className="vram-stats">
        <div className="vram-stat">
          <div className="label">Capacity</div>
          <div className="value">{fmtMiB(gpu.total_vram_mib)}</div>
        </div>
        <div className="vram-stat">
          <div className="label">Allocatable</div>
          <div className="value">{fmtMiB(allocatable)}</div>
        </div>
        <div className="vram-stat">
          <div className="label">Reserved</div>
          <div className="value">{fmtMiB(gpu.allocated_vram_mib)}</div>
        </div>
        <div className="vram-stat">
          <div className="label">Free</div>
          <div className="value">{fmtMiB(gpu.free_vram_mib)}</div>
        </div>
        <div className="vram-stat">
          <div className="label">Measured used</div>
          <div className="value">
            {measured != null ? fmtMiB(measured) : <span className="unavailable" title={t?.message}>unavailable</span>}
          </div>
          {measured == null && <div className="muted small">{unavailableWhy}</div>}
        </div>
        <div className="vram-stat">
          <div className="label">Utilization</div>
          <div className="value">{tv(t?.gpu_utilization_pct ?? t?.utilization_pct, '%')}</div>
        </div>
        <div className="vram-stat">
          <div className="label">Temp / Power</div>
          <div className="value">
            {tv(t?.temperature_celsius, '°C')} / {tv(t?.power_watts ?? t?.power_draw_watts, 'W', 1)}
          </div>
        </div>
      </div>
      {ok && t?.sampled_at && <div className="muted small" style={{ marginTop: 6 }}>Telemetry sampled {fmtDate(t.sampled_at)}</div>}

      <div style={{ marginTop: 14 }}>
        <div className="muted small" style={{ fontWeight: 600, marginBottom: 4 }}>
          Active allocations ({gpu.active_allocations?.length ?? 0})
        </div>
        {gpu.active_allocations?.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Job</th>
                  <th className="num">Reserved VRAM</th>
                  <th>Since</th>
                  <th>Container</th>
                </tr>
              </thead>
              <tbody>
                {gpu.active_allocations.map((a) => (
                  <tr key={a.job_id}>
                    <td>
                      <Link href={`/jobs/${encodeURIComponent(a.job_id)}`}>{a.job_name || a.job_id}</Link>
                      {a.job_name && <div className="muted small mono">{a.job_id}</div>}
                    </td>
                    <td className="num">{fmtMiB(a.allocated_vram_mib)}</td>
                    <td className="nowrap">{fmtDate(a.allocated_at)}</td>
                    <td className="mono small muted">{a.container_id ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="muted small" style={{ margin: 0 }}>
            No jobs hold a slice of this GPU.
          </p>
        )}
      </div>
    </div>
  );
}

function NodeCard({ node }: { node: GPUNode }) {
  const offline = node.status !== 'ONLINE';
  return (
    <section className={`node${node.status === 'OFFLINE' ? ' offline' : ''}`} id={node.node_id}>
      <div className="node-head">
        <div>
          <h3 style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
            <span className="mono">{node.node_id}</span>
            <NodeStatusBadge status={node.status} />
          </h3>
          <div className="muted small">
            host {node.hostname}
            {node.ip_address && ` · ${node.ip_address}`}
            {node.kubernetes_node && node.kubernetes_node !== node.node_id && ` · k8s ${node.kubernetes_node}`}
          </div>
        </div>
        {node.labels && (
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            {Object.entries(node.labels).map(([k, v]) => (
              <span key={k} className="chip">
                {k}={v}
              </span>
            ))}
          </div>
        )}
      </div>
      <div className="node-body">
        {offline && (
          <div className={`alert ${node.status === 'OFFLINE' ? 'err' : 'warn'}`}>
            <div className="alert-title">
              {node.status === 'OFFLINE' ? 'Node offline' : 'Node in maintenance'}
            </div>
            <p className="muted">
              {node.status === 'OFFLINE'
                ? 'The worker is not reachable (laptop asleep, shut down or disconnected). New jobs will not be placed here; reservations below are the last known state and measurements are unavailable.'
                : 'The node is cordoned for maintenance; no new jobs will be placed on it.'}
            </p>
          </div>
        )}
        {node.gpus.length === 0 ? (
          <p className="muted">No GPUs reported by this node.</p>
        ) : (
          node.gpus.map((g) => <GpuCard key={g.gpu_id} gpu={g} node={node} />)
        )}
      </div>
    </section>
  );
}

const CATEGORY_COLORS: Record<ServiceEntry['category'], string> = {
  portal:     'var(--accent)',
  api:        '#4fc3f7',
  monitoring: '#ff9800',
  worker:     'var(--neon)',
};

function ServicePanel() {
  const groups = CATEGORY_ORDER.map((cat) => ({
    cat,
    label: CATEGORY_LABEL[cat],
    services: PLATFORM_SERVICES.filter((s) => s.category === cat),
  }));

  return (
    <div className="card" style={{ marginTop: 24 }}>
      <div className="card-head">
        <h2>Platform services &amp; ports</h2>
        <span className="muted small">{PLATFORM_SERVICES.length} endpoints</span>
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 20 }}>
        {groups.map(({ cat, label, services }) => (
          <div key={cat}>
            <div style={{
              fontSize: 11,
              fontWeight: 700,
              letterSpacing: '0.08em',
              textTransform: 'uppercase',
              color: CATEGORY_COLORS[cat],
              marginBottom: 10,
              paddingBottom: 4,
              borderBottom: `1px solid ${CATEGORY_COLORS[cat]}33`,
            }}>
              {label}
            </div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {services.map((svc) => (
                <div key={svc.url} style={{
                  background: 'var(--surface2)',
                  borderRadius: 6,
                  padding: '8px 10px',
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  gap: 8,
                }}>
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontWeight: 600, fontSize: 13, color: 'var(--fg)' }}>{svc.name}</div>
                    <div className="muted small" style={{ marginTop: 2 }}>{svc.description}</div>
                    <div style={{ marginTop: 4, display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' }}>
                      <span className="chip mono" style={{ fontSize: 11 }}>{svc.host}</span>
                      <span className="chip" style={{
                        fontSize: 11,
                        fontWeight: 700,
                        color: CATEGORY_COLORS[cat],
                        background: `${CATEGORY_COLORS[cat]}1a`,
                        border: `1px solid ${CATEGORY_COLORS[cat]}55`,
                      }}>:{svc.port}</span>
                    </div>
                  </div>
                  <a
                    href={svc.url + (svc.path ?? '')}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="btn small"
                    style={{ whiteSpace: 'nowrap', flexShrink: 0 }}
                  >
                    Open ↗
                  </a>
                </div>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

export default function GpuInventory() {
  const poll = usePolling(getInventory, POLL_MS);
  const inv = poll.data;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>GPU inventory</h1>
          <p>
            Pooled worker GPUs. <b>Reserved</b> is what the scheduler has allocated to jobs; <b>measured</b> is what
            the GPU actually reports in use.
          </p>
        </div>
        <div className="head-actions">
          <Updated at={poll.updatedAt} live />
          <button className="btn small" type="button" onClick={() => void poll.refresh()}>
            Refresh
          </button>
        </div>
      </div>
      {poll.error != null && <ErrorAlert error={poll.error} stale={!!inv} title="Could not load GPU inventory" />}
      {!inv && !poll.error && <div className="card skeleton">Loading inventory…</div>}
      {inv && (
        <>
          <PoolTiles s={summarize(inv)} />
          {inv.nodes.length === 0 ? (
            <div className="card empty">No worker nodes have registered with the pool.</div>
          ) : (
            inv.nodes.map((n) => <NodeCard key={n.node_id} node={n} />)
          )}
        </>
      )}
      <ServicePanel />
    </>
  );
}
