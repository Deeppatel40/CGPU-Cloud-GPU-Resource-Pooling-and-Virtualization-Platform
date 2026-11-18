/**
 * Same-origin Server-Sent Events proxy for live job logs:
 *   GET /api/portal/log-stream/{job_id}?tail=500
 *     -> ${CGPU_API_URL}/api/v1/jobs/{job_id}/logs?stream=true&tail=500
 *
 * Why not just the next.config rewrite? The rewrite proxy and Next's gzip
 * compression may buffer a long-lived text/event-stream response. Returning
 * the upstream ReadableStream from a route handler with
 * `Cache-Control: no-transform` (which the compression middleware honours)
 * forwards each event as soon as it arrives.
 */
import type { NextRequest } from 'next/server';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

// Next.js bakes rewrites into the build (routes-manifest.json), so use the
// same build-time value here (inlined via next.config `env`) to guarantee the
// log stream and the /api/v1 rewrite always target the same API.
const API_URL = (process.env.NEXT_PUBLIC_CGPU_API_URL || 'http://localhost:8080').replace(/\/+$/, '');

function jsonError(status: number, error_code: string, message: string) {
  return Response.json({ error_code, message, timestamp: new Date().toISOString() }, { status });
}

export async function GET(req: NextRequest, ctx: { params: Promise<{ id: string }> }) {
  const { id } = await ctx.params;
  const tail = req.nextUrl.searchParams.get('tail');
  const qs = new URLSearchParams({ stream: 'true' });
  if (tail && /^\d+$/.test(tail)) qs.set('tail', tail);
  const upstreamUrl = `${API_URL}/api/v1/jobs/${encodeURIComponent(id)}/logs?${qs}`;

  const headers: Record<string, string> = { Accept: 'text/event-stream' };
  const lastEventId = req.headers.get('last-event-id');
  if (lastEventId) headers['Last-Event-ID'] = lastEventId;

  let upstream: Response;
  try {
    upstream = await fetch(upstreamUrl, { headers, cache: 'no-store', signal: req.signal });
  } catch {
    return jsonError(502, 'UPSTREAM_UNREACHABLE', 'The CGPU API is unreachable.');
  }

  const contentType = upstream.headers.get('content-type') ?? '';
  if (!upstream.ok || !upstream.body || !contentType.includes('text/event-stream')) {
    // e.g. the python mock answers JSON for ?stream=true. EventSource treats
    // any non-event-stream response as fatal, and the client falls back to
    // polling the JSON logs endpoint.
    await upstream.body?.cancel().catch(() => undefined);
    return jsonError(
      upstream.ok ? 502 : upstream.status,
      'STREAM_UNAVAILABLE',
      `Live log streaming is not available for job ${id} (upstream HTTP ${upstream.status}, ${contentType || 'no content-type'}).`,
    );
  }

  return new Response(upstream.body, {
    status: 200,
    headers: {
      'Content-Type': 'text/event-stream; charset=utf-8',
      'Cache-Control': 'no-cache, no-transform',
      'X-Accel-Buffering': 'no',
    },
  });
}
