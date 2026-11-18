'use client';

import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { useEffect, useMemo, useRef, useState } from 'react';
import type { GPUInventoryResponse, ValidationErrorDetail, WorkloadDefinition } from '@/types/api';
import { ApiError, describeError, getInventory, listWorkloads, submitJob, type WorkloadCatalog } from '@/lib/api';
import { fmtMiB, uuidv4 } from '@/lib/format';
import {
  coerceParameters,
  mapServerErrors,
  PRIORITY_MAX,
  PRIORITY_MIN,
  TIMEOUT_MIN,
  validateSubmission,
  type FieldErrors,
  type SubmissionForm,
} from '@/lib/validation';

function defaultsFor(w: WorkloadDefinition | undefined): Pick<SubmissionForm, 'requested_vram_mib' | 'parameters'> {
  return {
    requested_vram_mib: w ? String(w.default_vram_mib) : '',
    parameters: Object.fromEntries((w?.parameters ?? []).map((p) => [p.name, String(p.default)])),
  };
}

function defaultName(w: WorkloadDefinition | undefined): string {
  const base = (w?.workload_id ?? 'job').replace(/^pytorch-/, '').slice(0, 40);
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${base}-${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
}

/** Largest free VRAM on any GPU of an ONLINE node, i.e. the biggest job that can start right now. */
function largestFreeSlice(inv: GPUInventoryResponse | undefined): number | null {
  if (!inv) return null;
  let best = -1;
  for (const n of inv.nodes) {
    if (n.status !== 'ONLINE') continue;
    for (const g of n.gpus) best = Math.max(best, g.free_vram_mib);
  }
  return best < 0 ? 0 : best;
}

type Outcome =
  | { kind: 'validation'; message: string; code: string; general: ValidationErrorDetail[] }
  | { kind: 'error'; error: unknown; retryable: boolean };

export default function SubmitForm({ skipClientValidation = false }: { skipClientValidation?: boolean }) {
  const router = useRouter();
  const [catalog, setCatalog] = useState<WorkloadCatalog | null>(null);
  const [inventory, setInventory] = useState<GPUInventoryResponse | undefined>();
  const [form, setForm] = useState<SubmissionForm>({
    workload_id: '',
    name: '',
    requested_vram_mib: '',
    priority: '5',
    timeout_seconds: '3600',
    parameters: {},
  });
  const [touched, setTouched] = useState(false);
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [retryNote, setRetryNote] = useState<string | null>(null);

  // Idempotency key: generated once per submission attempt of a given form
  // content, reused for automatic and manual retries, cleared on any edit.
  const requestIdRef = useRef<string | null>(null);
  const [requestId, setRequestId] = useState<string | null>(null);
  const inFlight = useRef(false);

  useEffect(() => {
    let alive = true;
    listWorkloads().then((c) => {
      if (!alive) return;
      setCatalog(c);
      const first = c.items[0];
      setForm((f) => ({ ...f, workload_id: first?.workload_id ?? '', name: defaultName(first), ...defaultsFor(first) }));
    });
    getInventory()
      .then((inv) => alive && setInventory(inv))
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  const workload = useMemo(
    () => catalog?.items.find((w) => w.workload_id === form.workload_id),
    [catalog, form.workload_id],
  );
  const clientErrors = useMemo(
    () => (skipClientValidation ? {} : validateSubmission(form, workload)),
    [form, workload, skipClientValidation],
  );
  const errors: FieldErrors = { ...(touched ? clientErrors : {}), ...serverErrors };
  const freeNow = largestFreeSlice(inventory);
  const vramNum = Number(form.requested_vram_mib);

  function edit(patch: Partial<SubmissionForm>, keys: string[]) {
    if (submitting) return;
    setForm((f) => ({ ...f, ...patch }));
    // Any change to the payload makes it a different submission.
    requestIdRef.current = null;
    setRequestId(null);
    setServerErrors((e) => {
      const next = { ...e };
      for (const k of keys) delete next[k];
      return next;
    });
  }

  function onWorkloadChange(id: string) {
    const w = catalog?.items.find((x) => x.workload_id === id);
    edit({ workload_id: id, ...defaultsFor(w) }, ['workload_id', 'requested_vram_mib', ...Object.keys(form.parameters).map((k) => `parameters.${k}`)]);
  }

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (inFlight.current) return; // double-click guard
    setTouched(true);
    setOutcome(null);
    if (Object.keys(clientErrors).length > 0 || !workload) return;

    if (!requestIdRef.current) requestIdRef.current = uuidv4();
    const request_id = requestIdRef.current;
    setRequestId(request_id);

    inFlight.current = true;
    setSubmitting(true);
    setRetryNote(null);
    try {
      const body = {
        request_id,
        name: form.name.trim(),
        workload_id: workload.workload_id,
        requested_vram_mib: Number(form.requested_vram_mib),
        parameters: coerceParameters(form, workload),
        priority: form.priority.trim() === '' ? 5 : Number(form.priority),
        timeout_seconds: form.timeout_seconds.trim() === '' ? 3600 : Number(form.timeout_seconds),
      };
      const res = await submitJob(body, {
        onRetry: (n) => setRetryNote(`Network problem — retrying (attempt ${n + 1}) with the same request ID…`),
      });
      requestIdRef.current = null;
      router.push(`/jobs/${encodeURIComponent(res.job.job_id)}?submitted=${res.created ? 'created' : 'existing'}`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 400 && err.body) {
        const mapped = mapServerErrors(err.body.details, workload);
        setServerErrors(mapped.fields);
        setOutcome({ kind: 'validation', message: err.body.message, code: err.body.error_code, general: mapped.general });
        // The server rejected the payload, nothing was created: a corrected
        // form gets a fresh request ID (edits clear it).
      } else {
        const retryable = err instanceof ApiError && (err.network || err.status >= 500 || err.status === 0);
        setOutcome({ kind: 'error', error: err, retryable });
      }
    } finally {
      inFlight.current = false;
      setSubmitting(false);
      setRetryNote(null);
    }
  }

  if (!catalog) return <div className="card skeleton">Loading workload catalog…</div>;

  const fieldProps = (key: string) => ({ className: `field${errors[key] ? ' invalid' : ''}` });
  const Err = ({ k }: { k: string }) => (errors[k] ? <span className="error">{errors[k]}</span> : null);

  return (
    <form onSubmit={onSubmit} noValidate className="stack">
      {skipClientValidation && (
        <div className="alert warn">
          <div className="alert-title">Client-side validation disabled (demo mode)</div>
          <p className="muted">Invalid values are sent as-is so the API&apos;s 400 field errors can be demonstrated.</p>
        </div>
      )}
      {catalog.source === 'builtin' && (
        <div className={`alert ${catalog.warning ? 'warn' : ''}`}>
          <div className="alert-title">Using the built-in workload catalog</div>
          <p className="muted">
            {catalog.warning ?? 'The API does not expose GET /api/v1/workloads, so the approved demo workload is used.'}
          </p>
        </div>
      )}

      {workload?.pending_api_deploy && (
        <div className="alert warn" role="status">
          <div className="alert-title">
            {workload.name} is not deployed on this API yet
            <span className="chip">cannot submit</span>
          </div>
          <p>
            This workload exists in the repository but the running API does not list it, so a
            submission would be rejected as an unknown workload. The parameters below are shown
            so the workload can be reviewed.
          </p>
          <p className="muted small">
            To enable it: redeploy <code>cgpu-api</code> from a checkout that includes{' '}
            <code>{workload.workload_id}</code> in <code>pkg/jobs/catalog.go</code>, and import its
            image on each GPU node.
          </p>
        </div>
      )}

      {outcome?.kind === 'validation' && (
        <div className="alert err" role="alert">
          <div className="alert-title">
            The API rejected the submission <span className="chip">400 {outcome.code}</span>
          </div>
          <p>{outcome.message} Fields with problems are highlighted below.</p>
          {outcome.general.length > 0 && (
            <ul>
              {outcome.general.map((d, i) => (
                <li key={i}>
                  <code>{d.field}</code>: {d.message}
                  {d.rejected_value !== undefined && (
                    <span className="muted"> (got {JSON.stringify(d.rejected_value)})</span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {outcome?.kind === 'error' && (
        <div className="alert err" role="alert">
          <div className="alert-title">
            Submission failed
            {outcome.error instanceof ApiError && outcome.error.status > 0 && (
              <span className="chip">HTTP {outcome.error.status}</span>
            )}
          </div>
          <p>{describeError(outcome.error)}</p>
          {outcome.retryable && requestId && (
            <p className="muted small">
              Retrying re-sends request ID <code>{requestId}</code>; if the first attempt actually reached the API, the
              existing job is returned instead of creating a duplicate.
            </p>
          )}
        </div>
      )}

      <fieldset>
        <legend>Workload</legend>
        <div className="form-grid">
          <div {...fieldProps('workload_id')}>
            <label htmlFor="workload_id">Approved workload</label>
            <select id="workload_id" value={form.workload_id} onChange={(e) => onWorkloadChange(e.target.value)}>
              {catalog.items.map((w) => (
                <option key={w.workload_id} value={w.workload_id}>
                  {w.name} ({w.workload_id})
                  {w.pending_api_deploy ? ' — not yet deployed' : ''}
                </option>
              ))}
            </select>
            <Err k="workload_id" />
          </div>
          <div {...fieldProps('name')}>
            <label htmlFor="name">Job name</label>
            <input
              id="name"
              value={form.name}
              maxLength={80}
              autoComplete="off"
              onChange={(e) => edit({ name: e.target.value }, ['name'])}
            />
            <span className="hint">1–64 characters: letters, digits, - and _</span>
            <Err k="name" />
          </div>
        </div>
        {workload && <p className="muted small" style={{ marginBottom: 0 }}>{workload.description}</p>}
      </fieldset>

      {workload && workload.parameters.length > 0 && (
        <fieldset>
          <legend>Parameters</legend>
          <div className="form-grid">
            {workload.parameters.map((p) => {
              const k = `parameters.${p.name}`;
              const v = form.parameters[p.name] ?? '';
              const set = (val: string) => edit({ parameters: { ...form.parameters, [p.name]: val } }, [k]);
              return (
                <div key={p.name} {...fieldProps(k)}>
                  <label htmlFor={`p-${p.name}`}>
                    {p.name} <span className="chip">{p.type}</span>
                  </label>
                  {p.type === 'enum' ? (
                    <select id={`p-${p.name}`} value={v} onChange={(e) => set(e.target.value)}>
                      {(p.options ?? []).map((o) => (
                        <option key={o} value={o}>
                          {o}
                        </option>
                      ))}
                    </select>
                  ) : (
                    <input
                      id={`p-${p.name}`}
                      value={v}
                      inputMode={p.type === 'integer' ? 'numeric' : p.type === 'number' ? 'decimal' : 'text'}
                      onChange={(e) => set(e.target.value)}
                    />
                  )}
                  <span className="hint">
                    {p.description}
                    {(p.min != null || p.max != null) && ` Range ${p.min ?? '−∞'}–${p.max ?? '∞'}.`}
                  </span>
                  <Err k={k} />
                </div>
              );
            })}
          </div>
        </fieldset>
      )}

      <fieldset>
        <legend>Resources &amp; scheduling</legend>
        <div className="form-grid">
          <div {...fieldProps('requested_vram_mib')}>
            <label htmlFor="vram">VRAM request</label>
            <div className="input-suffix">
              <input
                id="vram"
                inputMode="numeric"
                value={form.requested_vram_mib}
                onChange={(e) => edit({ requested_vram_mib: e.target.value }, ['requested_vram_mib'])}
              />
              <span>MiB</span>
            </div>
            {workload && (
              <input
                type="range"
                aria-label="VRAM request slider"
                min={workload.min_vram_mib}
                max={workload.max_vram_mib}
                step={128}
                value={Number.isFinite(vramNum) && vramNum > 0 ? Math.min(vramNum, workload.max_vram_mib) : workload.default_vram_mib}
                onChange={(e) => edit({ requested_vram_mib: e.target.value }, ['requested_vram_mib'])}
              />
            )}
            {workload && (
              <span className="hint">
                Default {fmtMiB(workload.default_vram_mib)} · recommended ≥ {fmtMiB(workload.min_vram_mib)} · max{' '}
                {fmtMiB(workload.max_vram_mib)}. This is a scheduler reservation (the job&apos;s GPU slice).
              </span>
            )}
            {workload && !errors.requested_vram_mib && vramNum > 0 && vramNum < workload.min_vram_mib && (
              <span className="hint" style={{ color: 'var(--warn)' }}>
                Below the workload&apos;s recommended minimum — the job may fail with CUDA out-of-memory.
              </span>
            )}
            {freeNow != null && !errors.requested_vram_mib && vramNum > freeNow && (
              <span className="hint" style={{ color: 'var(--warn)' }}>
                Largest free slice right now is {fmtMiB(freeNow)}; this job will wait in the queue
                (WAITING_FOR_CAPACITY) until capacity frees up.
              </span>
            )}
            <Err k="requested_vram_mib" />
          </div>
          <div {...fieldProps('priority')}>
            <label htmlFor="priority">Priority</label>
            <input
              id="priority"
              inputMode="numeric"
              value={form.priority}
              onChange={(e) => edit({ priority: e.target.value }, ['priority'])}
            />
            <span className="hint">
              {PRIORITY_MIN} (lowest) – {PRIORITY_MAX} (highest), default 5
            </span>
            <Err k="priority" />
          </div>
          <div {...fieldProps('timeout_seconds')}>
            <label htmlFor="timeout">Timeout</label>
            <div className="input-suffix">
              <input
                id="timeout"
                inputMode="numeric"
                value={form.timeout_seconds}
                onChange={(e) => edit({ timeout_seconds: e.target.value }, ['timeout_seconds'])}
              />
              <span>sec</span>
            </div>
            <span className="hint">At least {TIMEOUT_MIN} s, default 3600 s</span>
            <Err k="timeout_seconds" />
          </div>
        </div>
      </fieldset>

      <div className="form-actions">
        {/* Blocked rather than left to fail: the running API has no entry for this
            workload, so a submission would be rejected as an unknown workload. Saying
            so here is more useful than relaying that error after the fact. */}
        <button type="submit" className="btn primary" disabled={submitting || !!workload?.pending_api_deploy}>
          {submitting ? 'Submitting…' : outcome?.kind === 'error' && outcome.retryable ? 'Retry submission' : 'Submit job'}
        </button>
        <Link href="/jobs" className="btn">
          Cancel
        </Link>
        {retryNote && <span className="muted small">{retryNote}</span>}
        {touched && Object.keys(clientErrors).length > 0 && (
          <span className="error small" style={{ color: 'var(--err)' }}>
            Fix {Object.keys(clientErrors).length} field{Object.keys(clientErrors).length > 1 ? 's' : ''} before
            submitting.
          </span>
        )}
      </div>
      {requestId && (
        <p className="muted small">
          Request ID (idempotency key): <code>{requestId}</code>
        </p>
      )}
    </form>
  );
}
