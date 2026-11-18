import type { Metadata } from 'next';
import JobList from '@/components/JobList';

export const metadata: Metadata = { title: 'Jobs' };

export default async function JobsPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const sp = await searchParams;
  const one = (v: string | string[] | undefined) => (Array.isArray(v) ? v[0] : v);
  const page = Number(one(sp.page));
  return (
    <JobList
      initialStatus={one(sp.status)}
      initialWorkload={one(sp.workload_id)}
      initialPage={Number.isInteger(page) ? page : 1}
    />
  );
}
