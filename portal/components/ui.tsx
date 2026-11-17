'use client';

import type { GPUNode, GPUDevice, JobState } from '@/types/api';
import { ApiError, describeError } from '@/lib/api';

const JOB_TONE: Record<JobState, string> = {
  PENDING: 'warn',
  RUNNING: 'info',
  SUCCEEDED: 'ok',
  FAILED: 'err',
};

export function JobStatusBadge({ status }: { status: JobState | string }) {
  const tone = JOB_TONE[status as JobState] ?? 'neutral';
  const live = status === 'RUNNING' || status === 'PENDING';
  return <span className={`badge ${tone}${live ? ' pulse' : ''}`}>{status}</span>;
}

export function NodeStatusBadge({ status }: { status: GPUNode['status'] | string }) {
  const tone = status === 'ONLINE' ? 'ok' : status === 'OFFLINE' ? 'err' : 'warn';
  return <span className={`badge ${tone}`}>{status}</span>;
}

export function HealthBadge({ status }: { status: GPUDevice['health_status'] | string }) {
  const tone = status === 'HEALTHY' ? 'ok' : status === 'DEGRADED' ? 'warn' : 'err';
  return <span className={`badge ${tone}`}>{status}</span>;
}

/** Shows an API error; for unreachable-API errors adds a hint about CGPU_API_URL. */
export function ErrorAlert({ error, title = 'Request failed', stale }: { error: unknown; title?: string; stale?: boolean }) {
  const network = error instanceof ApiError && error.network;
  return (
    <div className={`alert ${stale ? 'warn' : 'err'}`} role="alert">
      <div className="alert-title">
        {stale ? 'Showing last known data — refresh failed' : title}
        {error instanceof ApiError && error.status > 0 && <span className="chip">HTTP {error.status}</span>}
      </div>
      <p>{describeError(error)}</p>
      {network && (
        <p className="muted small">
          The portal proxies <code>/api/v1/*</code> to <code>{process.env.NEXT_PUBLIC_CGPU_API_URL}</code>. Set{' '}
          <code>CGPU_API_URL</code> and restart the portal if the API runs elsewhere.
        </p>
      )}
    </div>
  );
}

export function Updated({ at, live }: { at?: number; live?: boolean }) {
  if (!at) return null;
  return (
    <span className="muted small nowrap">
      {live ? 'Auto-refreshing · ' : ''}updated {new Date(at).toLocaleTimeString(undefined, { hour12: false })}
    </span>
  );
}
