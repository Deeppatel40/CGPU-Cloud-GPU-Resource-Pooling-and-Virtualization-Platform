import Overview from '@/components/Overview';
import LiveMetrics from '@/components/LiveMetrics';

export default function HomePage() {
  return (
    <>
      <Overview />
      {/* Measured figures from Prometheus, below the API-sourced pool summary.
          Kept on the landing page because "is the GPU actually busy?" is the
          first question anyone opening the portal has, and the API's own
          telemetry field cannot answer it while a worker's address is stale. */}
      <LiveMetrics />
    </>
  );
}
