import type { Metadata, Viewport } from 'next';
import Nav from '@/components/Nav';
import './globals.css';
// Imported after globals.css on purpose: theme.css is an override layer, so it
// must win on equal specificity. Delete this line to revert to the base palette.
import './theme.css';

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
