/**
 * Client-side validation mirroring the Go API rules (docs/api/job-api-specification.md §5.1).
 * The server remains authoritative: its 400 `details[]` are mapped back onto
 * the same field keys with mapServerErrors().
 */
import type { ValidationErrorDetail, WorkloadDefinition, WorkloadParameter } from '@/types/api';

export const NAME_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;
export const PRIORITY_MIN = 1;
export const PRIORITY_MAX = 10;
export const TIMEOUT_MIN = 60;

/** Raw (string) values as typed into the form. */
export interface SubmissionForm {
  workload_id: string;
  name: string;
  requested_vram_mib: string;
  priority: string;
  timeout_seconds: string;
  parameters: Record<string, string>;
}

/** Field key -> message. Parameter fields use `parameters.<name>`. */
export type FieldErrors = Record<string, string>;

const isIntString = (s: string) => /^-?\d+$/.test(s.trim());

function parseNumber(s: string): number | null {
  if (s.trim() === '') return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

export function validateParameter(p: WorkloadParameter, raw: string | undefined): string | null {
  const v = (raw ?? '').trim();
  switch (p.type) {
    case 'integer':
    case 'number': {
      if (v === '') return `${p.name} is required.`;
      const n = parseNumber(v);
      if (n === null) return `${p.name} must be a number.`;
      if (p.type === 'integer' && !isIntString(v)) return `${p.name} must be a whole number.`;
      if (p.min != null && n < p.min) return `${p.name} must be ≥ ${p.min}.`;
      if (p.max != null && n > p.max) return `${p.name} must be ≤ ${p.max}.`;
      return null;
    }
    case 'enum':
      if (!p.options?.includes(v)) return `${p.name} must be one of: ${(p.options ?? []).join(', ')}.`;
      return null;
    case 'string':
    default:
      return null;
  }
}

export function validateSubmission(form: SubmissionForm, workload: WorkloadDefinition | undefined): FieldErrors {
  const errors: FieldErrors = {};

  if (!workload) errors.workload_id = 'Select an approved workload.';

  const name = form.name.trim();
  if (name.length === 0) errors.name = 'Job name is required.';
  else if (name.length > 64) errors.name = 'Job name must be at most 64 characters.';
  else if (!NAME_PATTERN.test(name)) errors.name = 'Use only letters, digits, dashes (-) and underscores (_).';

  const vramRaw = form.requested_vram_mib.trim();
  const vram = parseNumber(vramRaw);
  if (vramRaw === '' || vram === null) errors.requested_vram_mib = 'VRAM request is required (MiB).';
  else if (!isIntString(vramRaw)) errors.requested_vram_mib = 'VRAM must be a whole number of MiB.';
  else if (vram <= 0) errors.requested_vram_mib = 'VRAM must be greater than 0 MiB.';
  else if (workload && vram > workload.max_vram_mib)
    errors.requested_vram_mib = `VRAM must be ≤ ${workload.max_vram_mib} MiB for this workload (largest allocatable GPU slice).`;

  const prRaw = form.priority.trim();
  const pr = parseNumber(prRaw);
  if (prRaw !== '') {
    if (pr === null || !isIntString(prRaw) || pr < PRIORITY_MIN || pr > PRIORITY_MAX)
      errors.priority = `Priority must be a whole number from ${PRIORITY_MIN} to ${PRIORITY_MAX}.`;
  }

  const toRaw = form.timeout_seconds.trim();
  const to = parseNumber(toRaw);
  if (toRaw !== '') {
    if (to === null || !isIntString(toRaw)) errors.timeout_seconds = 'Timeout must be a whole number of seconds.';
    else if (to < TIMEOUT_MIN) errors.timeout_seconds = `Timeout must be at least ${TIMEOUT_MIN} seconds.`;
  }

  for (const p of workload?.parameters ?? []) {
    const msg = validateParameter(p, form.parameters[p.name]);
    if (msg) errors[`parameters.${p.name}`] = msg;
  }
  return errors;
}

/** Converts the typed form values into the JSON parameter object. */
export function coerceParameters(form: SubmissionForm, workload: WorkloadDefinition): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const p of workload.parameters) {
    const raw = (form.parameters[p.name] ?? '').trim();
    out[p.name] = p.type === 'integer' || p.type === 'number' ? Number(raw) : raw;
  }
  return out;
}

const TOP_LEVEL_FIELDS = new Set(['workload_id', 'name', 'requested_vram_mib', 'priority', 'timeout_seconds']);

/**
 * Maps server `details[]` onto form field keys. Details for fields the form
 * does not show (e.g. request_id) are returned in `general`.
 */
export function mapServerErrors(
  details: ValidationErrorDetail[] | undefined,
  workload: WorkloadDefinition | undefined,
): { fields: FieldErrors; general: ValidationErrorDetail[] } {
  const fields: FieldErrors = {};
  const general: ValidationErrorDetail[] = [];
  const paramNames = new Set(workload?.parameters.map((p) => p.name) ?? []);
  for (const d of details ?? []) {
    const f = (d.field ?? '').replace(/^\$\.|^body\./, '');
    let key: string | null = null;
    if (TOP_LEVEL_FIELDS.has(f)) key = f;
    else if (f.startsWith('parameters.') && paramNames.has(f.slice('parameters.'.length))) key = f;
    else if (paramNames.has(f)) key = `parameters.${f}`;
    if (key && !fields[key]) fields[key] = d.message;
    else general.push(d);
  }
  return { fields, general };
}
