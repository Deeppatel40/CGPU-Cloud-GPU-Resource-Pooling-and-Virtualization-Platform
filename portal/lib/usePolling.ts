'use client';

import { useCallback, useEffect, useRef, useState } from 'react';

export interface PollState<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
  updatedAt: number | undefined;
  refresh: () => Promise<void>;
}

/**
 * Fetches `fn` immediately and then every `intervalMs` while `enabled`.
 * Requests never overlap; the last good data is kept when a refresh fails so
 * the page can show a "stale" banner instead of going blank. Changing `key`
 * discards the previous data (e.g. navigating to another job).
 */
export function usePolling<T>(fn: () => Promise<T>, intervalMs: number, enabled = true, key = ''): PollState<T> {
  const fnRef = useRef(fn);
  useEffect(() => {
    fnRef.current = fn;
  });

  const [state, setState] = useState<Omit<PollState<T>, 'refresh'>>({
    data: undefined,
    error: undefined,
    loading: true,
    updatedAt: undefined,
  });
  const genRef = useRef(0);

  const tick = useCallback(async (gen: number) => {
    try {
      const data = await fnRef.current();
      if (gen === genRef.current) setState({ data, error: undefined, loading: false, updatedAt: Date.now() });
    } catch (error) {
      if (gen === genRef.current) setState((s) => ({ ...s, error, loading: false }));
    }
  }, []);

  const lastKey = useRef(key);
  useEffect(() => {
    const gen = ++genRef.current;
    if (lastKey.current !== key) {
      lastKey.current = key;
      setState({ data: undefined, error: undefined, loading: true, updatedAt: undefined });
    }
    let timer: ReturnType<typeof setTimeout> | undefined;
    let cancelled = false;
    const loop = async () => {
      await tick(gen);
      if (!cancelled && enabled) timer = setTimeout(loop, intervalMs);
    };
    void loop();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [key, enabled, intervalMs, tick]);

  const refresh = useCallback(() => tick(genRef.current), [tick]);
  return { ...state, refresh };
}
