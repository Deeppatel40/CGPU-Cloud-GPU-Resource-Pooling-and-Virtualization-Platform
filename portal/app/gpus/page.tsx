import type { Metadata } from 'next';
import GpuInventory from '@/components/GpuInventory';

export const metadata: Metadata = { title: 'GPU inventory' };

export default function GpusPage() {
  return <GpuInventory />;
}
