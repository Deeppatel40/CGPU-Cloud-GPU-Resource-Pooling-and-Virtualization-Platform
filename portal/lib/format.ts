import type { JobState } from '@/types/api';

const nf = new Intl.NumberFormat('en-US');

export function fmtInt(n: number | null | undefined): string {
  return n == null || Number.isNaN(n) ? '—' : nf.format(Math.round(n));
}

export function fmtMiB(n: number | null | undefined): string {
  return n == null || Number.isNaN(n) ? '—' : `${nf.format(Math.round(n))} MiB`;
}

export function fmtGiB(mib: number | null | undefined): string {
  return mib == null || Number.isNaN(mib) ? '—' : `${(mib / 1024).toFixed(1)} GiB`;
}

export function fmtBytes(b: number | null | undefined): string {
  if (b == null || Number.isNaN(b)) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = b;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function fmtDuration(sec: number | null | undefined): string {
  if (sec == null || Number.isNaN(sec)) return '—';
  const s = Math.max(0, Math.round(sec));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m > 0) return `${m}m ${String(r).padStart(2, '0')}s`;
  return `${r}s`;
}

export function fmtDate(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function fmtTime(iso: string | null | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString(undefined, { hour12: false });
}

export function fmtNumber(n: number | null | undefined, digits = 2): string {
  return n == null || Number.isNaN(n) ? '—' : n.toFixed(digits);
}

export const TERMINAL_STATES: ReadonlySet<JobState> = new Set<JobState>(['SUCCEEDED', 'FAILED']);

export function isTerminal(status: JobState | undefined): boolean {
  return status !== undefined && TERMINAL_STATES.has(status);
}

/** UUIDv4. crypto.randomUUID only exists in secure contexts (https/localhost),
 * so fall back to getRandomValues when the portal is opened via a LAN IP. */
export function uuidv4(): string {
  const c: Crypto | undefined = typeof crypto !== 'undefined' ? crypto : undefined;
  if (c && typeof c.randomUUID === 'function') return c.randomUUID();
  const b = new Uint8Array(16);
  if (c && typeof c.getRandomValues === 'function') c.getRandomValues(b);
  else for (let i = 0; i < 16; i++) b[i] = Math.floor(Math.random() * 256);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
