import Link from 'next/link';

export default function NotFound() {
  return (
    <div className="card empty">
      <h1>Page not found</h1>
      <p>
        <Link href="/">Back to the overview</Link>
      </p>
    </div>
  );
}
