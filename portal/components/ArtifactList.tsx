'use client';

import { useEffect, useState } from 'react';
import type { ArtifactMetadata, JobState } from '@/types/api';
import { artifactHref, describeError, getJobArtifacts } from '@/lib/api';
import { fmtBytes, fmtDate } from '@/lib/format';

const TYPE_LABEL: Record<string, string> = {
  checkpoint: 'Checkpoint',
  model_weights: 'Model weights',
  metrics: 'Metrics',
  logs: 'Logs',
  tensorboard: 'TensorBoard',
};

/**
 * Lists result files via GET /api/v1/jobs/{id}/artifacts, falling back to the
 * `artifacts` embedded in the job status if that call fails. Download links
 * are the API's relative `download_url`s, proxied same-origin.
 */
export default function ArtifactList({
  jobId,
  status,
  embedded,
}: {
  jobId: string;
  status: JobState;
  embedded: ArtifactMetadata[];
}) {
  const [items, setItems] = useState<ArtifactMetadata[]>(embedded);
  const [error, setError] = useState<string | null>(null);
  const embeddedCount = embedded.length;

  useEffect(() => {
    if (status === 'PENDING') return;
    let alive = true;
    getJobArtifacts(jobId)
      .then((r) => {
        if (!alive) return;
        setItems(r.artifacts);
        setError(null);
      })
      .catch((e) => alive && setError(describeError(e)));
    return () => {
      alive = false;
    };
    // Re-fetch when the job changes state or new artifacts are indexed.
  }, [jobId, status, embeddedCount]);

  const shown = items.length > 0 ? items : embedded;

  return (
    <div className="card">
      <div className="card-head">
        <h2>Results &amp; checkpoints</h2>
        <span className="muted small">{shown.length} file{shown.length === 1 ? '' : 's'}</span>
      </div>
      {error && shown.length === 0 && <p className="small" style={{ color: 'var(--err)' }}>{error}</p>}
      {shown.length === 0 ? (
        <p className="muted" style={{ margin: 0 }}>
          {status === 'SUCCEEDED'
            ? 'This job produced no downloadable artifacts.'
            : status === 'FAILED'
              ? 'No artifacts were saved before the job failed.'
              : 'Checkpoints and results appear here as the workload writes them.'}
        </p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>File</th>
                <th>Type</th>
                <th className="num">Size</th>
                <th>Created</th>
                <th>SHA-256</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {shown.map((a) => (
                <tr key={a.artifact_id}>
                  <td className="mono">{a.name}</td>
                  <td>{TYPE_LABEL[a.artifact_type] ?? a.artifact_type}</td>
                  <td className="num nowrap">{fmtBytes(a.size_bytes)}</td>
                  <td className="nowrap">{fmtDate(a.created_at)}</td>
                  <td>
                    <span className="chip" title={a.checksum_sha256}>
                      {a.checksum_sha256?.slice(0, 12)}…
                    </span>
                  </td>
                  <td>
                    <a className="btn small primary" href={artifactHref(a.download_url)} download={a.name}>
                      Download
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
