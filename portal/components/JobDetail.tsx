'use client';

import Link from 'next/link';
import { useEffect, useState } from 'react';
import type { GPUTelemetrySample, JobStatusResponse } from '@/types/api';
import { ApiError, cancelJob, describeError, getJob } from '@/lib/api';
import { fmtDate, fmtDuration, fmtInt, fmtMiB, fmtNumber, isTerminal } from '@/lib/format';
import { failureReason, pendingReason, subStatusText } from '@/lib/reasons';
import { usePolling } from '@/lib/usePolling';
import { ErrorAlert, JobStatusBadge, Updated } from './ui';
import LogViewer from './LogViewer';
import ArtifactList from './ArtifactList';

const POLL_MS = 3000;

function durationOf(job: JobStatusResponse): number | null {
  const m = job.execution_metrics?.total_duration_seconds;
  if (m != null) return m;
  const start = job.timestamps.started_at ? Date.parse(job.timestamps.started_at) : NaN;
  if (Number.isNaN(start)) return null;
  const end = job.timestamps.completed_at ? Date.parse(job.timestamps.completed_at) : Date.now();
  return (end - start) / 1000;
}

function PendingPanel({ job }: { job: JobStatusResponse }) {
  const r = pendingReason(job.pending_reason ?? job.sub_status);
  const d = job.pending_details;
  return (
    <div className="alert warn">
      <div className="alert-title">
        Queued: {r?.title ?? 'Waiting to be scheduled'}
        {job.pending_reason && <span className="chip">{job.pending_reason}</span>}
      </div>
      {r?.help && <p className="muted">{r.help}</p>}
      {d?.message && <p>{d.message}</p>}
      {d && (
        <dl className="kv" style={{ marginTop: 10 }}>
          {d.queue_position != null && (
            <>
              <dt>Queue position</dt>
              <dd>#{d.queue_position}</dd>
            </>
          )}
          {d.required_vram_mib != null && (
            <>
              <dt>Requested VRAM</dt>
              <dd>{fmtMiB(d.required_vram_mib)}</dd>
            </>
          )}
          {d.available_vram_mib != null && (
            <>
              <dt>Largest free slice</dt>
              <dd>{fmtMiB(d.available_vram_mib)}</dd>
            </>
          )}
          {d.pool_total_vram_mib != null && (
            <>
              <dt>Pool reserved / total</dt>
              <dd>
                {fmtMiB(d.pool_allocated_vram_mib)} / {fmtMiB(d.pool_total_vram_mib)}
              </dd>
            </>
          )}
          {d.estimated_wait_seconds != null && (
            <>
              <dt>Estimated wait</dt>
              <dd>~{fmtDuration(d.estimated_wait_seconds)}</dd>
            </>
          )}
        </dl>
      )}
    </div>
  );
}

function FailurePanel({ job }: { job: JobStatusResponse }) {
  const r = failureReason(job.failure_reason);
  const d = job.failure_details;
  return (
    <div className="alert err">
      <div className="alert-title">
        Failed: {r?.title ?? 'Job failed'}
        {job.failure_reason && <span className="chip">{job.failure_reason}</span>}
        {d && <span className="chip">exit code {d.exit_code}</span>}
      </div>
      {r?.help && <p className="muted">{r.help}</p>}
      {d?.error_message && (
        <p>
          <code>{d.error_message}</code>
        </p>
      )}
      {d?.terminated_at && <p className="muted small">Terminated at {fmtDate(d.terminated_at)}</p>}
      {d?.stacktrace && (
        <details style={{ marginTop: 8 }}>
          <summary>Stack trace</summary>
          <pre className="stack">{d.stacktrace}</pre>
        </details>
      )}
    </div>
  );
}

function TelemetryValue({ t }: { t: GPUTelemetrySample | null | undefined }) {
  if (!t || t.status !== 'AVAILABLE') {
    return (
      <span className="unavailable" title={t?.message ?? undefined}>
        unavailable{t?.reason ? ` (${t.reason})` : ''}
      </span>
    );
  }
  return null;
}

function Telemetry({ job }: { job: JobStatusResponse }) {
  const t = job.telemetry;
  const ok = t?.status === 'AVAILABLE';
  const num = (v: number | null | undefined, unit: string, digits = 0) =>
    ok && v != null ? `${v.toFixed(digits)} ${unit}` : <span className="unavailable">unavailable</span>;
  return (
    <div className="card">
      <div className="card-head">
        <h2>Measured GPU telemetry</h2>
        {ok ? <span className="badge ok">available</span> : <TelemetryValue t={t} />}
      </div>
      {!ok && t?.message && <p className="muted small">{t.message}</p>}
      {!t && <p className="muted small">No measurements reported for this job.</p>}
      <dl className="kv">
        <dt>VRAM used (measured)</dt>
        <dd>{ok && t?.memory_used_mib != null ? fmtMiB(t.memory_used_mib) : <span className="unavailable">unavailable</span>}</dd>
        <dt>GPU utilization</dt>
        <dd>{num(t?.gpu_utilization_pct ?? t?.utilization_pct, '%')}</dd>
        <dt>Temperature</dt>
        <dd>{num(t?.temperature_celsius, '°C')}</dd>
        <dt>Power</dt>
        <dd>{num(t?.power_watts ?? t?.power_draw_watts, 'W', 1)}</dd>
        <dt>Sampled</dt>
        <dd>{fmtDate(t?.sampled_at)}</dd>
      </dl>
    </div>
  );
}

export default function JobDetail({ jobId, submitted }: { jobId: string; submitted?: string }) {
  const [enabled, setEnabled] = useState(true);
  const poll = usePolling(() => getJob(jobId), POLL_MS, enabled, jobId);
  const job = poll.data;
  const terminal = isTerminal(job?.status);
  // Stop auto-refresh once the job reaches SUCCEEDED/FAILED.
  useEffect(() => {
    if (job) setEnabled(!terminal);
  }, [job, terminal]);

  const [cancelling, setCancelling] = useState(false);
  const [cancelMsg, setCancelMsg] = useState<{ ok: boolean; text: string } | null>(null);

  async function onCancel() {
    if (!job || !window.confirm(`Cancel job "${job.name}"? Its GPU reservation will be released.`)) return;
    setCancelling(true);
    setCancelMsg(null);
    try {
      const r = await cancelJob(job.job_id);
      setCancelMsg({ ok: true, text: `Cancellation accepted${r.failure_reason ? ` (${r.failure_reason})` : ''}.` });
      await poll.refresh();
    } catch (e) {
      setCancelMsg({ ok: false, text: describeError(e) });
    } finally {
      setCancelling(false);
    }
  }

  if (!job) {
    if (poll.error) {
      const notFound = poll.error instanceof ApiError && poll.error.status === 404;
      return (
        <>
          <ErrorAlert error={poll.error} title={notFound ? 'Job not found' : 'Could not load job'} />
          <Link href="/jobs">← Back to jobs</Link>
        </>
      );
    }
    return <div className="card skeleton">Loading job {jobId}…</div>;
  }

  const p = job.placement;
  const m = job.execution_metrics;
  const sub = subStatusText(job.sub_status);
  const params = job.parameters ? Object.entries(job.parameters) : [];

  return (
    <>
      <div className="page-head">
        <div>
          <p className="small" style={{ marginTop: 0 }}>
            <Link href="/jobs">← Jobs</Link>
          </p>
          <h1 style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
            {job.name} <JobStatusBadge status={job.status} />
          </h1>
          <p>
            <span className="mono">{job.job_id}</span>
            {sub && <> · {sub}</>}
          </p>
        </div>
        <div className="head-actions">
          <Updated at={poll.updatedAt} live={!terminal} />
          {!terminal && (
            <button type="button" className="btn danger" onClick={onCancel} disabled={cancelling}>
              {cancelling ? 'Cancelling…' : 'Cancel job'}
            </button>
          )}
          <Link className="btn" href="/submit">
            New job
          </Link>
        </div>
      </div>

      {submitted && (
        <div className="alert ok">
          <div className="alert-title">
            {submitted === 'existing' ? 'Submission already received' : 'Job submitted successfully'}
            <span className="chip">{submitted === 'existing' ? '200 OK' : '201 Created'}</span>
          </div>
          <p className="muted">
            {submitted === 'existing'
              ? 'The API recognised the request ID from an earlier attempt and returned the existing job — no duplicate was created.'
              : 'The job was accepted by the API and handed to the scheduler. This page refreshes automatically.'}
          </p>
        </div>
      )}
      {cancelMsg && (
        <div className={`alert ${cancelMsg.ok ? 'ok' : 'err'}`}>
          <div className="alert-title">{cancelMsg.ok ? 'Cancel requested' : 'Cancel failed'}</div>
          <p>{cancelMsg.text}</p>
        </div>
      )}
      {poll.error != null && <ErrorAlert error={poll.error} stale />}

      {job.status === 'PENDING' && <PendingPanel job={job} />}
      {job.status === 'FAILED' && <FailurePanel job={job} />}
      {job.status === 'SUCCEEDED' && (
        <div className="alert ok">
          <div className="alert-title">Completed successfully</div>
          <p className="muted">
            Finished {fmtDate(job.timestamps.completed_at)} after {fmtDuration(durationOf(job))}. Results are listed below.
          </p>
        </div>
      )}

      <div className="grid grid-2">
        <div className="card">
          <h2>Placement</h2>
          {p ? (
            <dl className="kv">
              <dt>Worker node</dt>
              <dd>
                <Link href={`/gpus#${encodeURIComponent(p.node_id)}`} className="mono">
                  {p.node_id}
                </Link>
              </dd>
              <dt>GPU</dt>
              <dd>
                <span className="mono">{p.gpu_id}</span> <span className="chip">index {p.assigned_gpu_index}</span>
              </dd>
              <dt>Reserved VRAM</dt>
              <dd>{fmtMiB(p.allocated_vram_mib)}</dd>
              <dt>Enforcement</dt>
              <dd>
                <span className="chip">{p.enforcement_mode}</span>
              </dd>
              {p.placement_reason && (
                <>
                  <dt>Placement reason</dt>
                  <dd>
                    <span className="chip">{p.placement_reason}</span>
                  </dd>
                </>
              )}
              <dt>Scheduled</dt>
              <dd>{fmtDate(p.scheduled_at)}</dd>
            </dl>
          ) : (
            <p className="muted" style={{ margin: 0 }}>
              {job.status === 'PENDING'
                ? 'Not placed yet — no node or GPU has been assigned.'
                : 'This job was never placed on a GPU.'}
            </p>
          )}
        </div>

        <div className="card">
          <h2>Request</h2>
          <dl className="kv">
            <dt>Workload</dt>
            <dd className="mono">{job.workload_id}</dd>
            <dt>Requested VRAM</dt>
            <dd>{fmtMiB(job.requested_vram_mib)}</dd>
            <dt>Priority</dt>
            <dd>{job.priority}</dd>
            {params.length > 0 && (
              <>
                <dt>Parameters</dt>
                <dd>
                  {params.map(([k, v]) => (
                    <span key={k} className="chip" style={{ marginRight: 6, marginBottom: 4 }}>
                      {k}={typeof v === 'object' ? JSON.stringify(v) : String(v)}
                    </span>
                  ))}
                </dd>
              </>
            )}
            {job.dataset && (
              <>
                <dt>Dataset</dt>
                <dd>
                  {job.dataset.type}
                  {job.dataset.name ? ` · ${job.dataset.name}` : ''}
                  {job.dataset.uri ? <span className="muted"> ({job.dataset.uri})</span> : null}
                </dd>
              </>
            )}
            <dt>Request ID</dt>
            <dd className="mono small">{job.request_id}</dd>
          </dl>
        </div>

        <div className="card">
          <h2>Timeline</h2>
          <dl className="kv">
            <dt>Submitted</dt>
            <dd>{fmtDate(job.timestamps.submitted_at)}</dd>
            <dt>Scheduled</dt>
            <dd>{fmtDate(job.timestamps.scheduled_at)}</dd>
            <dt>Started</dt>
            <dd>{fmtDate(job.timestamps.started_at)}</dd>
            <dt>Completed</dt>
            <dd>{fmtDate(job.timestamps.completed_at)}</dd>
            <dt>{terminal ? 'Run time' : 'Running for'}</dt>
            <dd>{fmtDuration(durationOf(job))}</dd>
          </dl>
        </div>

        <div className="card">
          <h2>Execution metrics</h2>
          {m ? (
            <dl className="kv">
              {/* matmul benchmark fields */}
              {m.gflops != null && (
                <>
                  <dt>Throughput</dt>
                  <dd>{m.gflops.toFixed(1)} GFLOP/s</dd>
                </>
              )}
              {m.matrix_size != null && (
                <>
                  <dt>Matrix size</dt>
                  <dd>{m.matrix_size} × {m.matrix_size}</dd>
                </>
              )}
              {m.iterations != null && (
                <>
                  <dt>Iterations</dt>
                  <dd>{fmtInt(m.iterations)}</dd>
                </>
              )}
              {m.working_set_mib != null && (
                <>
                  <dt>Working set</dt>
                  <dd>{fmtMiB(m.working_set_mib)}</dd>
                </>
              )}
              {m.result_verified != null && (
                <>
                  <dt>Result</dt>
                  <dd>
                    <span className={`chip ${m.result_verified ? '' : 'err'}`}>
                      {m.result_verified ? 'arithmetic correct' : 'WRONG RESULT'}
                    </span>
                    {m.result_element0 != null && (
                      <span className="muted small" style={{ marginLeft: 8 }}>
                        C[0][0]={m.result_element0.toFixed(0)} (expected {m.expected_element0?.toFixed(0)})
                      </span>
                    )}
                  </dd>
                </>
              )}
              {m.elapsed_seconds != null && (
                <>
                  <dt>Elapsed</dt>
                  <dd>{fmtDuration(m.elapsed_seconds)}</dd>
                </>
              )}
              {/* ResNet / training fields */}
              {m.final_loss != null && (
                <>
                  <dt>Final loss</dt>
                  <dd>{fmtNumber(m.final_loss, 4)}</dd>
                </>
              )}
              {m.final_accuracy != null && (
                <>
                  <dt>Final accuracy</dt>
                  <dd>{m.final_accuracy.toFixed(2)} %</dd>
                </>
              )}
              {m.peak_vram_mib != null && (
                <>
                  <dt>Peak VRAM (measured)</dt>
                  <dd>
                    {fmtMiB(m.peak_vram_mib)}
                    <span className="muted small"> of {fmtMiB(job.placement?.allocated_vram_mib ?? job.requested_vram_mib)} reserved</span>
                  </dd>
                </>
              )}
              {m.epochs_completed != null && (
                <>
                  <dt>Epochs completed</dt>
                  <dd>{fmtInt(m.epochs_completed)}</dd>
                </>
              )}
              {m.samples_per_second != null && (
                <>
                  <dt>Throughput</dt>
                  <dd>{m.samples_per_second.toFixed(1)} samples/s</dd>
                </>
              )}
              {m.total_duration_seconds != null && (
                <>
                  <dt>Total duration</dt>
                  <dd>{fmtDuration(m.total_duration_seconds)}</dd>
                </>
              )}
            </dl>
          ) : (
            <p className="muted" style={{ margin: 0 }}>
              {terminal ? 'The workload did not report metrics.' : 'Reported by the workload when it finishes.'}
            </p>
          )}
        </div>
      </div>

      <div style={{ marginTop: 16 }} className="stack">
        {(job.placement || job.telemetry) && <Telemetry job={job} />}
        <ArtifactList jobId={job.job_id} status={job.status} embedded={job.artifacts ?? []} />
        <LogViewer jobId={job.job_id} status={job.status} />
      </div>
    </>
  );
}
