/**
 * CGPU platform service registry — every component and the port it listens on.
 * Used by the GPU inventory page to show a "Platform services" panel.
 */

export interface ServiceEntry {
  name: string;
  description: string;
  host: string;
  port: number;
  path?: string;
  /** URL to open in a new tab */
  url: string;
  category: 'portal' | 'api' | 'monitoring' | 'worker';
}

const PC1 = '10.20.24.31';
const PC2 = '10.20.24.32';

export const PLATFORM_SERVICES: ServiceEntry[] = [
  // ── Portal / server ────────────────────────────────────────────────────────
  {
    name: 'CGPU Portal',
    description: 'Web interface (nginx → Next.js)',
    host: PC2,
    port: 80,
    url: `http://${PC2}`,
    category: 'portal',
  },
  {
    name: 'CGPU Portal (direct)',
    description: 'Next.js dev server on PC2',
    host: PC2,
    port: 3000,
    url: `http://${PC2}:3000`,
    category: 'portal',
  },
  {
    name: 'CGPU REST API',
    description: 'Job submission and status API on PC2',
    host: PC2,
    port: 8080,
    path: '/api/v1/gpus',
    url: `http://${PC2}:8080`,
    category: 'api',
  },
  // ── Control plane ──────────────────────────────────────────────────────────
  {
    name: 'CGPU Scheduler',
    description: 'GPU placement engine on PC1',
    host: PC1,
    port: 8082,
    path: '/api/v1/nodes',
    url: `http://${PC1}:8082`,
    category: 'api',
  },
  {
    name: 'Kubernetes API',
    description: 'k3s control plane on PC1',
    host: PC1,
    port: 6443,
    url: `https://${PC1}:6443`,
    category: 'api',
  },
  // ── Monitoring ─────────────────────────────────────────────────────────────
  {
    name: 'Prometheus',
    description: 'Metrics database (NodePort 30090 on PC1)',
    host: PC1,
    port: 30090,
    path: '/targets',
    url: `http://${PC1}:30090`,
    category: 'monitoring',
  },
  {
    name: 'Grafana',
    description: 'Dashboards (NodePort 30300 on PC1) · admin/admin',
    host: PC1,
    port: 30300,
    url: `http://${PC1}:30300`,
    category: 'monitoring',
  },
  // ── GPU worker agents ──────────────────────────────────────────────────────
  {
    name: 'Worker Agent · RTX 4050',
    description: 'Telemetry exporter on Deep\'s laptop (10.20.24.35)',
    host: '10.20.24.35',
    port: 9400,
    path: '/metrics',
    url: `http://10.20.24.35:9400`,
    category: 'worker',
  },
  {
    name: 'Worker Agent · RTX 3050',
    description: 'Telemetry exporter on Subrat\'s laptop (10.1.144.120)',
    host: '10.1.144.120',
    port: 9400,
    path: '/metrics',
    url: `http://10.1.144.120:9400`,
    category: 'worker',
  },
];

export const CATEGORY_LABEL: Record<ServiceEntry['category'], string> = {
  portal: 'Portal & Server',
  api: 'API & Control Plane',
  monitoring: 'Monitoring',
  worker: 'GPU Worker Agents',
};

export const CATEGORY_ORDER: ServiceEntry['category'][] = [
  'portal',
  'api',
  'monitoring',
  'worker',
];
