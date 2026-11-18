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
  {
    // Added as the light alternative to the ResNet workload: a 353 MB image against 11.9 GB,
    // and no dataset at all. The ResNet demo needs a 170 MB download whose upstream host
    // serves at a few hundred bytes per second, so for showing pooling, sharing and quota
    // enforcement this one gets to a busy GPU in seconds instead of hours.
    workload_id: 'gpu-matmul',
    name: 'GPU matmul benchmark',
    description:
      'Sustained tiled SGEMM that saturates the GPU and reports achieved GFLOP/s. No dataset needed, so it starts immediately — the quickest way to show a GPU under real load and two jobs sharing one card under separate VRAM quotas. Verifies its own arithmetic and writes metrics.json.',
    default_vram_mib: 1024,
    min_vram_mib: 256,
    max_vram_mib: 5629,
    parameters: [
      {
        name: 'matrix_size',
        type: 'integer',
        default: 4096,
        min: 256,
        max: 16384,
        description:
          'Side length N of the N×N matrices. Work scales as N³ and memory as N², and the workload shrinks N automatically if it will not fit the VRAM quota.',
      },
      {
        name: 'seconds',
        type: 'integer',
        default: 120,
        min: 10,
        max: 3600,
        description: 'How long to keep the GPU busy.',
      },
      {
        name: 'vram_mib',
        type: 'integer',
        default: 0,
        min: 0,
        max: 5629,
        description:
          'Optional cap on the working set, for holding a deliberate amount of VRAM. 0 sizes it from the matrix size.',
      },
    ],
  },
];

/** Per-GPU VRAM held back for the driver/display; allocatable = total − reserve. */
export const DRIVER_RESERVE_MIB = 512;
