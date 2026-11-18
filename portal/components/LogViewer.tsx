'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import type { JobState, LogLine } from '@/types/api';
import { describeError, getJobLogs, logStreamUrl } from '@/lib/api';
import { fmtTime } from '@/lib/format';

const TAIL = 500;
const POLL_MS = 3000;

type Mode = 'loading' | 'live' | 'polling' | 'static' | 'idle';

const lineKey = (l: LogLine) => `${l.timestamp}|${l.stream}|${l.message}`;

/**
 * Job logs. RUNNING jobs are followed live via Server-Sent Events
 * (EventSource -> /api/portal/log-stream/{id}); if the stream cannot be opened
 * or drops, the viewer falls back to polling GET .../logs?tail=500 every 3 s.
 * Finished jobs show a one-off snapshot.
 */
export default function LogViewer({ jobId, status }: { jobId: string; status: JobState }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [mode, setMode] = useState<Mode>('loading');
  const [error, setError] = useState<string | null>(null);
  const [follow, setFollow] = useState(true);
  const boxRef = useRef<HTMLDivElement>(null);
  const seen = useRef<Set<string>>(new Set());

  const replaceAll = useCallback((next: LogLine[]) => {
    seen.current = new Set(next.map(lineKey));
    setLines(next);
  }, []);

  const append = useCallback((l: LogLine) => {
    const k = lineKey(l);
    if (seen.current.has(k)) return;
    seen.current.add(k);
    setLines((prev) => {
      const next = [...prev, l];
      return next.length > 5000 ? next.slice(next.length - 5000) : next;
    });
  }, []);

  /** Loads the JSON tail. With `keepLonger`, lines already streamed are kept
   * unless the snapshot is at least as complete (avoids losing SSE lines if
   * the JSON endpoint lags behind the stream). */
  const snapshot = useCallback(async (keepLonger = false) => {
    try {
      const res = await getJobLogs(jobId, TAIL);
      if (!keepLonger || res.lines.length >= seen.current.size) replaceAll(res.lines);
      setError(null);
    } catch (e) {
      setError(describeError(e));
    }
  }, [jobId, replaceAll]);

  const running = status === 'RUNNING';

  useEffect(() => {
    let cancelled = false;
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setInterval> | null = null;

    const startPolling = () => {
      if (cancelled || timer) return;
      setMode('polling');
      timer = setInterval(() => void snapshot(), POLL_MS);
    };

    (async () => {
      await snapshot();
      if (cancelled) return;
      if (!running) {
        setMode(status === 'PENDING' ? 'idle' : 'static');
        return;
      }
      if (typeof EventSource === 'undefined') return startPolling();

      es = new EventSource(logStreamUrl(jobId, TAIL));
      es.onopen = () => !cancelled && setMode('live');
      es.onmessage = (ev) => {
        try {
          const parsed = JSON.parse(ev.data) as LogLine | LogLine[];
          (Array.isArray(parsed) ? parsed : [parsed]).forEach((l) => l && typeof l.message === 'string' && append(l));
        } catch {
          append({ timestamp: new Date().toISOString(), stream: 'stdout', message: String(ev.data) });
        }
      };
      es.addEventListener('end', () => {
        es?.close();
        es = null;
        if (!cancelled) {
          setMode('static');
          void snapshot(true);
        }
      });
      es.onerror = () => {
        // Stream unavailable (e.g. mock API returns JSON) or dropped: fall back.
        es?.close();
        es = null;
        startPolling();
      };
    })();

    return () => {
      cancelled = true;
      es?.close();
      if (timer) clearInterval(timer);
    };
  }, [jobId, running, status, snapshot, append]);

  // Keep the newest line in view while following.
  useEffect(() => {
    const el = boxRef.current;
    if (el && follow) el.scrollTop = el.scrollHeight;
  }, [lines, follow]);

  const onScroll = () => {
    const el = boxRef.current;
    if (!el) return;
    setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
  };

  const modeLabel: Record<Mode, React.ReactNode> = {
    loading: <span className="badge neutral">loading</span>,
    live: <span className="badge info pulse">live stream</span>,
    polling: <span className="badge warn pulse">polling every 3 s</span>,
    static: <span className="badge neutral">final</span>,
    idle: <span className="badge neutral">waiting for job to start</span>,
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Logs</h2>
        <div className="head-actions">
          {modeLabel[mode]}
          <span className="muted small">
            {lines.length} line{lines.length === 1 ? '' : 's'} (last {TAIL})
          </span>
          <button type="button" className="btn small" onClick={() => void snapshot()}>
            Reload
          </button>
        </div>
      </div>
      {error && <p className="small" style={{ color: 'var(--err)' }}>Could not load logs: {error}</p>}
      <div className="logbox" ref={boxRef} onScroll={onScroll} aria-live="polite">
        {lines.length === 0 ? (
          <div className="logline">
            <span className="ts" />
            <span style={{ color: '#6b7690' }}>
              {status === 'PENDING' ? 'No output yet — the job has not started.' : 'No log output.'}
            </span>
          </div>
        ) : (
          lines.map((l, i) => (
            <div key={i} className={`logline ${l.stream === 'stderr' ? 'stderr' : ''}`}>
              <span className="ts" title={l.timestamp}>
                {fmtTime(l.timestamp)}
              </span>
              <span>{l.message}</span>
            </div>
          ))
        )}
      </div>
      {!follow && lines.length > 0 && (
        <p className="small muted" style={{ marginBottom: 0 }}>
          Auto-scroll paused.{' '}
          <button type="button" className="btn small" onClick={() => setFollow(true)}>
            Jump to latest
          </button>
        </p>
      )}
    </div>
  );
}
