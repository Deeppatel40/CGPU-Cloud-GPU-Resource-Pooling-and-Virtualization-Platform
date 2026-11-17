import type { Metadata, Viewport } from 'next';
import Nav from '@/components/Nav';
import './globals.css';

export const metadata: Metadata = {
  title: { default: 'CGPU Portal', template: '%s · CGPU Portal' },
  description: 'Submit GPU training jobs and inspect the pooled GPU cluster.',
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  colorScheme: 'dark light',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Nav />
        <main className="container">{children}</main>
        <footer className="footer container">
          CGPU · Cloud GPU Resource Pooling &amp; Virtualization · API{' '}
          <code>{process.env.NEXT_PUBLIC_CGPU_API_URL}</code>
        </footer>
      </body>
    </html>
  );
}
