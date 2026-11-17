import type { WorkloadDefinition } from '@/types/api';

/**
 * Built-in fallback catalog, used when GET /api/v1/workloads is not available
 * (e.g. the python mock server returns 404). Mirrors the approved demo
 * workload in docs/api/job-api-specification.md §2.
 */
export const BUILTIN_WORKLOADS: WorkloadDefinition[] = [
  {
    workload_id: 'pytorch-resnet18-cifar10',
    name: 'PyTorch ResNet-18 on CIFAR-10',
    description:
      'Image-classification training (ResNet-18, CIFAR-10). Writes per-epoch checkpoints, final weights and metrics.json as downloadable artifacts.',
    default_vram_mib: 2048,
    min_vram_mib: 256,
    max_vram_mib: 5629,
    parameters: [
      { name: 'epochs', type: 'integer', default: 3, min: 1, max: 50, description: 'Number of training epochs.' },
      { name: 'batch_size', type: 'integer', default: 64, min: 8, max: 512, description: 'Mini-batch size. Larger batches need more VRAM.' },
      { name: 'learning_rate', type: 'number', default: 0.001, description: 'Optimizer learning rate.' },
      { name: 'optimizer', type: 'enum', default: 'adam', options: ['adam', 'sgd'], description: 'Optimizer algorithm.' },
    ],
  },
];

/** Per-GPU VRAM held back for the driver/display; allocatable = total − reserve. */
export const DRIVER_RESERVE_MIB = 512;
