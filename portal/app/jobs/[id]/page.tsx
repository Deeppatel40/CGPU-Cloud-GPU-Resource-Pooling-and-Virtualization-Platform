import type { Metadata } from 'next';
import JobDetail from '@/components/JobDetail';

type Params = Promise<{ id: string }>;

export async function generateMetadata({ params }: { params: Params }): Promise<Metadata> {
  const { id } = await params;
  return { title: `Job ${decodeURIComponent(id)}` };
}

export default async function JobPage({
  params,
  searchParams,
}: {
  params: Params;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { id } = await params;
  const sp = await searchParams;
  const submitted = typeof sp.submitted === 'string' ? sp.submitted : undefined;
  return <JobDetail jobId={decodeURIComponent(id)} submitted={submitted} />;
}
