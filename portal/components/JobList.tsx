'use client';

import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { useEffect, useState } from 'react';
import type { JobState, JobSummary } from '@/types/api';
import { listJobs, listWorkloads } from '@/lib/api';
import { fmtDate, fmtDuration, fmtMiB } from '@/lib/format';
import { usePolling } from '@/lib/usePolling';
import { ErrorAlert, JobStatusBadge, Updated } from './ui';

const STATES: JobState[] = ['PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED'];
const LIMIT = 20;
const POLL_MS = 5000;

export function JobsTable({ items, compact = false }: { items: JobSummary[]; compact?: boolean }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Job</th>
            <th>Status</th>
            {!compact && <th>Workload</th>}
            <th className="num">VRAM</th>
            <th>Node</th>
            <th>Submitted</th>
            <th className="num">Duration</th>
          </tr>
        </thead>
        <tbody>
          {items.map((j) => (
            <tr key={j.job_id}>
              <td>
                <Link href={`/jobs/${encodeURIComponent(j.job_id)}`}>{j.name}</Link>
                {!compact && <div className="muted small mono">{j.job_id}</div>}
              </td>
              <td>
                <JobStatusBadge status={j.status} />
              </td>
              {!compact && <td className="mono small">{j.workload_id}</td>}
              <td className="num nowrap">{fmtMiB(j.requested_vram_mib)}</td>
              <td className="mono small">{j.node_id ?? <span className="muted">not placed</span>}</td>
              <td className="nowrap">{fmtDate(j.submitted_at)}</td>
              <td className="num">{fmtDuration(j.duration_seconds)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function JobList({
  initialStatus,
  initialWorkload,
  initialPage,
}: {
  initialStatus?: string;
  initialWorkload?: string;
  initialPage?: number;
}) {
  const router = useRouter();
  const [status, setStatus] = useState<string>(STATES.includes(initialStatus as JobState) ? initialStatus! : '');
  const [workload, setWorkload] = useState(initialWorkload ?? '');
  const [page, setPage] = useState(initialPage && initialPage > 0 ? initialPage : 1);
  const [workloadIds, setWorkloadIds] = useState<string[]>([]);

  useEffect(() => {
    listWorkloads().then((c) => setWorkloadIds(c.items.map((w) => w.workload_id)));
  }, []);

  // Keep filters in the URL so views can be shared / reloaded.
  useEffect(() => {
    const qs = new URLSearchParams();
    if (status) qs.set('status', status);
    if (workload) qs.set('workload_id', workload);
    if (page > 1) qs.set('page', String(page));
    const s = qs.toString();
    router.replace(s ? `/jobs?${s}` : '/jobs', { scroll: false });
  }, [status, workload, page, router]);

  const key = `${status}|${workload}|${page}`;
  const poll = usePolling(
    () => listJobs({ status: (status || undefined) as JobState | undefined, workload_id: workload || undefined, page, limit: LIMIT }),
    POLL_MS,
    true,
    key,
  );
  const data = poll.data;
  const pg = data?.pagination;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Jobs</h1>
          <p>All submitted GPU jobs, newest first. Select a job for placement, logs and results.</p>
        </div>
        <div className="head-actions">
          <Updated at={poll.updatedAt} live />
          <Link href="/submit" className="btn primary">
            Submit job
          </Link>
        </div>
      </div>

      <div className="card">
        <div className="filters">
          <div className="field">
            <label htmlFor="f-status">Status</label>
            <select
              id="f-status"
              value={status}
              onChange={(e) => {
                setStatus(e.target.value);
                setPage(1);
              }}
            >
              <option value="">All states</option>
              {STATES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="f-workload">Workload</label>
            <select
              id="f-workload"
              value={workload}
              onChange={(e) => {
                setWorkload(e.target.value);
                setPage(1);
              }}
            >
              <option value="">All workloads</option>
              {[...new Set([...workloadIds, ...(workload ? [workload] : [])])].map((w) => (
                <option key={w} value={w}>
                  {w}
                </option>
              ))}
            </select>
          </div>
        </div>

        {poll.error != null && <ErrorAlert error={poll.error} stale={!!data} title="Could not load jobs" />}
        {!data && !poll.error && <div className="skeleton">Loading jobs…</div>}
        {data && data.items.length === 0 && (
          <div className="empty">
            No jobs{status || workload ? ' match these filters' : ' yet'}. <Link href="/submit">Submit one</Link>.
          </div>
        )}
        {data && data.items.length > 0 && <JobsTable items={data.items} />}

        {pg && (
          <div className="pager">
            <span className="muted small">
              Page {pg.current_page} of {Math.max(1, pg.total_pages)} · {pg.total_items} job{pg.total_items === 1 ? '' : 's'}
            </span>
            <div className="head-actions">
              <button className="btn small" type="button" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
                ← Previous
              </button>
              <button
                className="btn small"
                type="button"
                disabled={page >= (pg.total_pages || 1)}
                onClick={() => setPage((p) => p + 1)}
              >
                Next →
              </button>
            </div>
          </div>
        )}
      </div>
    </>
  );
}
