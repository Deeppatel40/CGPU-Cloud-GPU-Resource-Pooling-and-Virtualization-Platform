# Approved Demonstration Workload: PyTorch ResNet-18 CIFAR-10

- **Workload Identifier**: `pytorch-resnet18-cifar10`
- **Default Container Image**: `cgpu/workloads/pytorch-resnet18-cifar10:1.0.0`
- **Recommended VRAM Request**: **`2048` MiB** (positive integer in MiB)
- **Target Node**: WSL2 / Kubernetes Worker with NVIDIA GeForce RTX 4050 Laptop GPU (6,144 MiB total VRAM)

---

## 1. Why this Workload?

To prove that the CGPU platform provides real GPU resource pooling, intelligent scheduling, and fractional virtualization:
1. **Multi-Tenancy Demonstration**:
   - An RTX 4050 has 6,144 MiB of VRAM.
   - Traditional GPU schedulers allocate 1 full GPU to 1 job (wasting ~4 GB).
   - With CGPU's fractional virtualization layer, **two or three 2,048 MiB jobs** can run concurrently on the same RTX 4050 without out-of-memory crashes.
2. **Standardized Deep Learning Flow**:
   - Downloads/loads standard CIFAR-10 dataset (or generates synthetic mini-batches for offline testing).
   - Trains ResNet-18 with convolutional layers, batch normalization, and cross-entropy loss.
   - Periodically saves model checkpoints (`checkpoint_epoch_{N}.pt`).
   - Produces structured output metrics (`metrics.json`) and streaming logs.

---

## 2. Input Specification

Workload parameters can be passed in two ways:
1. **Mounted JSON File** (default for Kubernetes): `/etc/cgpu/params.json`
2. **Environment Variable**: `CGPU_JOB_PARAMS='{"epochs": 5, "batch_size": 64, "learning_rate": 0.001}'`

### Parameter Schema
```json
{
  "epochs": 5,
  "batch_size": 64,
  "learning_rate": 0.001,
  "optimizer": "adam",
  "dataset": {
    "type": "builtin",
    "name": "cifar10",
    "uri": "builtin://cifar10"
  },
  "save_every_epochs": 1,
  "seed": 42
}
```

---

## 3. Output Specification

When running inside the container:
- Model checkpoints are written to: `/outputs/checkpoints/`
  - `/outputs/checkpoints/checkpoint_epoch_1.pt`
  - `/outputs/checkpoints/checkpoint_epoch_2.pt`
  - ...
  - `/outputs/checkpoints/model_final.pt`
- Summary metrics are written to: `/outputs/metrics.json`
- Standard logs are streamed to `stdout` and `stderr`.

### Metrics Schema (`/outputs/metrics.json`)
```json
{
  "final_loss": 0.2842,
  "final_accuracy": 91.42,
  "peak_vram_mib": 1840,
  "total_duration_seconds": 184.2,
  "epochs_completed": 5,
  "samples_per_second": 328.4
}
```

---

## 4. Checkpoint Artifacts Integration

The CGPU Job Manager indexes files written to `/outputs/checkpoints/` upon job completion. These are exposed to the Next.js UI via:
- `GET /api/v1/jobs/{job_id}/artifacts`
- `GET /api/v1/jobs/{job_id}/artifacts/{artifact_id}/download`

