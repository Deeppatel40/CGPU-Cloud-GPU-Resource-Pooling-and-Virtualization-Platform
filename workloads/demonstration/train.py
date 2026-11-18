#!/usr/bin/env python3
"""
CGPU Demonstration Workload: ResNet-18 on CIFAR-10 (real training).

Trains torchvision's ResNet-18 on CIFAR-10 on the GPU slice CGPU assigned, evaluates on
the test set every epoch, saves a checkpoint per epoch plus final weights, and writes
measured metrics (loss, accuracy, throughput, peak VRAM from the CUDA allocator) to
/outputs/metrics.json.

Every number this script reports is measured. If PyTorch or CUDA is unavailable it exits
with an error instead of simulating a run (set CGPU_ALLOW_CPU=1 to permit a slow CPU run
for local testing).

Parameters (from /etc/cgpu/params.json, then CGPU_JOB_PARAMS, then CLI):
  epochs, batch_size, learning_rate, optimizer (adam|sgd), max_steps_per_epoch (0 = full),
  save_every_epochs, seed, data_dir, output_dir
"""

import argparse
import json
import os
import sys
import time
from pathlib import Path


def parse_config():
    config = {
        "epochs": 3,
        "batch_size": 64,
        "learning_rate": 0.001,
        "optimizer": "adam",
        "max_steps_per_epoch": 0,
        "save_every_epochs": 1,
        "seed": 42,
        "data_dir": "/data/cifar10",
        "output_dir": "/outputs",
    }
    mounted = Path("/etc/cgpu/params.json")
    if mounted.exists():
        try:
            config.update(json.loads(mounted.read_text(encoding="utf-8")))
            print(f"[CGPU] Loaded config from {mounted}", flush=True)
        except Exception as e:  # noqa: BLE001
            print(f"[CGPU-WARN] Failed reading {mounted}: {e}", file=sys.stderr, flush=True)
    env_params = os.environ.get("CGPU_JOB_PARAMS")
    if env_params:
        try:
            config.update(json.loads(env_params))
            print("[CGPU] Merged config from CGPU_JOB_PARAMS", flush=True)
        except Exception as e:  # noqa: BLE001
            print(f"[CGPU-WARN] Failed parsing CGPU_JOB_PARAMS: {e}", file=sys.stderr, flush=True)

    p = argparse.ArgumentParser(description="CGPU ResNet-18 workload")
    p.add_argument("--epochs", type=int, default=config["epochs"])
    p.add_argument("--batch-size", type=int, default=config["batch_size"])
    p.add_argument("--lr", type=float, default=config["learning_rate"])
    p.add_argument("--max-steps-per-epoch", type=int, default=config["max_steps_per_epoch"])
    p.add_argument("--data-dir", type=str, default=config["data_dir"])
    p.add_argument("--output-dir", type=str, default=config["output_dir"])
    a, _ = p.parse_known_args()
    config.update(epochs=a.epochs, batch_size=a.batch_size, learning_rate=a.lr,
                  max_steps_per_epoch=a.max_steps_per_epoch, data_dir=a.data_dir, output_dir=a.output_dir)
    return config


def log(msg):
    # Timestamped so logs from two jobs on one GPU can be compared for overlap (#14).
    print(f"{time.strftime('%Y-%m-%dT%H:%M:%S', time.gmtime())}Z {msg}", flush=True)


def main():
    cfg = parse_config()
    out = Path(cfg["output_dir"])
    ckpt_dir = out / "checkpoints"
    ckpt_dir.mkdir(parents=True, exist_ok=True)

    try:
        import torch
        import torch.nn as nn
        import torchvision
        import torchvision.transforms as T
    except ImportError as e:
        print(f"[CGPU-ERROR] PyTorch/torchvision not installed: {e}", file=sys.stderr, flush=True)
        return 2

    if torch.cuda.is_available():
        device = torch.device("cuda")
    elif os.environ.get("CGPU_ALLOW_CPU") == "1":
        device = torch.device("cpu")
    else:
        print("[CGPU-ERROR] CUDA is not available in this container (check runtimeClassName: nvidia).",
              file=sys.stderr, flush=True)
        return 3

    torch.manual_seed(int(cfg["seed"]))
    quota = os.environ.get("CGPU_ALLOCATED_VRAM_MIB", "unset")
    log("=" * 64)
    log("CGPU workload: ResNet-18 on CIFAR-10")
    log(f"job={os.environ.get('CGPU_JOB_ID', 'local')} device={device} "
        f"gpu={torch.cuda.get_device_name(0) if device.type == 'cuda' else 'cpu'} quota={quota} MiB")
    if device.type == "cuda":
        free_b, total_b = torch.cuda.mem_get_info()
        # Under the CGPU guard, total reports the job's quota, not the physical card.
        log(f"cudaMemGetInfo: free={free_b // 2**20} MiB total={total_b // 2**20} MiB")
    log(f"config={json.dumps(cfg)}")
    log("=" * 64)

    norm = T.Normalize((0.4914, 0.4822, 0.4465), (0.2470, 0.2435, 0.2616))
    train_tf = T.Compose([T.RandomCrop(32, padding=4), T.RandomHorizontalFlip(), T.ToTensor(), norm])
    test_tf = T.Compose([T.ToTensor(), norm])
    t0 = time.time()
    train_set = torchvision.datasets.CIFAR10(cfg["data_dir"], train=True, download=True, transform=train_tf)
    test_set = torchvision.datasets.CIFAR10(cfg["data_dir"], train=False, download=True, transform=test_tf)
    log(f"dataset ready in {time.time() - t0:.1f}s ({len(train_set)} train / {len(test_set)} test)")

    pin = device.type == "cuda"
    train_loader = torch.utils.data.DataLoader(train_set, batch_size=int(cfg["batch_size"]), shuffle=True,
                                               num_workers=2, pin_memory=pin, drop_last=True)
    test_loader = torch.utils.data.DataLoader(test_set, batch_size=256, shuffle=False, num_workers=2, pin_memory=pin)

    model = torchvision.models.resnet18(num_classes=10).to(device)
    if cfg["optimizer"] == "sgd":
        opt = torch.optim.SGD(model.parameters(), lr=float(cfg["learning_rate"]), momentum=0.9, weight_decay=5e-4)
    else:
        opt = torch.optim.Adam(model.parameters(), lr=float(cfg["learning_rate"]))
    loss_fn = nn.CrossEntropyLoss()
    if device.type == "cuda":
        torch.cuda.reset_peak_memory_stats()

    max_steps = int(cfg["max_steps_per_epoch"]) or len(train_loader)
    epochs = int(cfg["epochs"])
    run_start = time.time()
    samples = 0
    train_time = 0.0
    final_loss = final_acc = None

    for epoch in range(1, epochs + 1):
        model.train()
        ep_start = time.time()
        running, seen = 0.0, 0
        for step, (x, y) in enumerate(train_loader, 1):
            if step > max_steps:
                break
            x, y = x.to(device, non_blocking=True), y.to(device, non_blocking=True)
            opt.zero_grad(set_to_none=True)
            loss = loss_fn(model(x), y)
            loss.backward()
            opt.step()
            running += loss.item() * x.size(0)
            seen += x.size(0)
            if step % 50 == 0 or step == max_steps:
                mem = f" vram_alloc={torch.cuda.memory_allocated() // 2**20}MiB" if device.type == "cuda" else ""
                log(f"epoch {epoch}/{epochs} step {step}/{max_steps} loss={running / seen:.4f}{mem}")
        if device.type == "cuda":
            torch.cuda.synchronize()
        ep_time = time.time() - ep_start
        train_time += ep_time
        samples += seen

        model.eval()
        correct = total = 0
        with torch.no_grad():
            for x, y in test_loader:
                x, y = x.to(device, non_blocking=True), y.to(device, non_blocking=True)
                correct += (model(x).argmax(1) == y).sum().item()
                total += y.size(0)
        final_loss = running / max(seen, 1)
        final_acc = 100.0 * correct / total
        log(f"epoch {epoch}/{epochs} done: train_loss={final_loss:.4f} test_acc={final_acc:.2f}% "
            f"({seen / ep_time:.0f} img/s, {ep_time:.1f}s)")

        if epoch % int(cfg["save_every_epochs"]) == 0 or epoch == epochs:
            path = ckpt_dir / f"checkpoint_epoch_{epoch}.pt"
            torch.save({"epoch": epoch, "model_state_dict": model.state_dict(),
                        "optimizer_state_dict": opt.state_dict(), "train_loss": final_loss,
                        "test_accuracy": final_acc, "config": cfg}, path)
            log(f"[Checkpoint] saved {path} ({path.stat().st_size / 2**20:.1f} MiB)")

    final_path = ckpt_dir / "model_final.pt"
    torch.save(model.state_dict(), final_path)
    log(f"[Checkpoint] saved final weights to {final_path}")

    peak = int(torch.cuda.max_memory_allocated() // 2**20) if device.type == "cuda" else None
    metrics = {
        "final_loss": round(final_loss, 4),
        "final_accuracy": round(final_acc, 2),
        "peak_vram_mib": peak,
        "total_duration_seconds": round(time.time() - run_start, 2),
        "epochs_completed": epochs,
        "samples_per_second": round(samples / train_time, 1) if train_time else None,
    }
    (out / "metrics.json").write_text(json.dumps(metrics, indent=2), encoding="utf-8")
    log(f"[CGPU] metrics: {json.dumps(metrics)}")
    log("[CGPU] workload finished successfully")
    return 0


if __name__ == "__main__":
    sys.exit(main())
