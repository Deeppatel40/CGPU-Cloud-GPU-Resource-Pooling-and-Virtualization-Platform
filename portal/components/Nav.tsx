'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';

const LINKS = [
  { href: '/', label: 'Overview' },
  { href: '/submit', label: 'Submit job' },
  { href: '/jobs', label: 'Jobs' },
  { href: '/gpus', label: 'GPU inventory' },
  { href: '/telemetry', label: 'Live telemetry' },
];

export default function Nav() {
  const pathname = usePathname() ?? '/';
  return (
    <header className="topbar">
      <div className="container topbar-inner">
        <Link href="/" className="brand">
          <span className="brand-mark" aria-hidden>
            ▦
          </span>
          CGPU <span className="brand-sub">GPU pool portal</span>
        </Link>
        <nav className="nav">
          {LINKS.map((l) => {
            const active = l.href === '/' ? pathname === '/' : pathname.startsWith(l.href);
            return (
              <Link key={l.href} href={l.href} className={active ? 'nav-link active' : 'nav-link'}>
                {l.label}
              </Link>
            );
          })}
        </nav>
      </div>
    </header>
  );
}
