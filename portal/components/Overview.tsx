'use client';

import Link from 'next/link';
import { getInventory, listJobs } from '@/lib/api';
import { usePolling } from '@/lib/usePolling';
import { PoolTiles, summarize } from './GpuInventory';
import { JobsTable } from './JobList';
import { ErrorAlert, Updated } from './ui';

export default function Overview() {
  const inv = usePolling(getInventory, 5000);
  const jobs = usePolling(() => listJobs({ page: 1, limit: 8 }), 5000);
  const counts = jobs.data?.items.reduce<Record<string, number>>((acc, j) => {
    acc[j.status] = (acc[j.status] ?? 0) + 1;
    return acc;
  }, {});

  return (
    <>
      <div className="page-head">
        <div>
          <h1>CGPU overview</h1>
          <p>Consumer NVIDIA GPUs pooled into one K3s cluster and shared as VRAM slices.</p>
        </div>
        <div className="head-actions">
          <Updated at={inv.updatedAt} live />
          <Link href="/submit" className="btn primary">
            Submit job
          </Link>
        </div>
      </div>

      {inv.error != null && <ErrorAlert error={inv.error} stale={!!inv.data} title="Could not load GPU pool" />}
      {inv.data ? <PoolTiles s={summarize(inv.data)} /> : !inv.error && <div className="card skeleton">Loading pool…</div>}

      <div className="card">
        <div className="card-head">
          <h2>Recent jobs</h2>
          <div className="head-actions">
            {counts &&
              Object.entries(counts).map(([s, n]) => (
                <span key={s} className="chip">
                  {s} {n}
                </span>
              ))}
            <Link href="/jobs" className="small">
              View all →
            </Link>
          </div>
        </div>
        {jobs.error != null && <ErrorAlert error={jobs.error} stale={!!jobs.data} title="Could not load jobs" />}
        {jobs.data && jobs.data.items.length > 0 && <JobsTable items={jobs.data.items} compact />}
        {jobs.data && jobs.data.items.length === 0 && (
          <div className="empty">
            No jobs yet. <Link href="/submit">Submit the first one</Link>.
          </div>
        )}
      </div>
    </>
  );
}
