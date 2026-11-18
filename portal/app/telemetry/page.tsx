import type { Metadata } from 'next';
import LiveMetrics from '@/components/LiveMetrics';

export const metadata: Metadata = { title: 'Live telemetry' };

export default function TelemetryPage() {
  return (
    <>
      <div className="page-head">
        <h1>Live telemetry</h1>
        <p className="muted small">
          Measured GPU and per-job figures, read from Prometheus. This path does not depend on the
          job API&apos;s per-node agent probe, so it keeps working when a worker&apos;s recorded
          address is stale.
        </p>
      </div>
      <LiveMetrics />
    </>
  );
}
