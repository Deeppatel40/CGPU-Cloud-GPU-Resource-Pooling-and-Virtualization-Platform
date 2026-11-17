import type { NextConfig } from 'next';

/**
 * Base URL of the CGPU Go API (issue #12). The browser never talks to it
 * directly: every request goes to this Next.js server (same origin) and is
 * proxied, so no CORS configuration is needed and the API can stay bound to
 * localhost on the control-plane machine.
 *
 *   CGPU_API_URL=http://localhost:8080   (default, real Go API)
 *   CGPU_API_URL=http://localhost:8000   (python mock: `python3 api/mocks/server.py 8000`)
 */
const apiUrl = (process.env.CGPU_API_URL || 'http://localhost:8080').replace(/\/+$/, '');

const nextConfig: NextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // Type checking is done by `npm run typecheck`; there is no ESLint setup.
  eslint: { ignoreDuringBuilds: true },
  env: {
    // Exposed to the UI only so the footer can show which backend is in use.
    NEXT_PUBLIC_CGPU_API_URL: apiUrl,
  },
  async rewrites() {
    // Plain array => "afterFiles": route handlers in app/ (the SSE log proxy
    // under /api/portal) win; everything else under /api/v1 is proxied.
    return [
      {
        source: '/api/v1/:path*',
        destination: `${apiUrl}/api/v1/:path*`,
      },
    ];
  },
};

export default nextConfig;
