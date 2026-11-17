import type { Metadata } from 'next';
import SubmitForm from '@/components/SubmitForm';

export const metadata: Metadata = { title: 'Submit job' };

export default async function SubmitPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const sp = await searchParams;
  // ?novalidate=1 sends the form without client-side checks, to demo the API's 400 field errors.
  const skip = sp.novalidate === '1';
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Submit a GPU job</h1>
          <p>Pick an approved workload, set its parameters and reserve a slice of GPU memory.</p>
        </div>
      </div>
      <SubmitForm skipClientValidation={skip} />
    </>
  );
}
