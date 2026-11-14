# GPU Worker: RTX 4050 (WSL 2) — Engineering Record

**Node:** `cgpu-gpu-worker-4050`
**Owner / engineer:** Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40))
**Covers:** Issue A1 (#7) and Issue A2 (#9), the parts performed on the RTX 4050 laptop
**Date:** 14 November 2026

This is the authoritative record for the first GPU worker in the CGPU cluster: how it was built, what was proven, and the platform limits that A3 and A4 must design around. Everything below was produced and verified on this machine; the provenance section lists the commits.

---

## 1. Hardware and platform

| Field | Value |
| --- | --- |
| GPU | NVIDIA GeForce RTX 4050 Laptop GPU |
| **Device identifier (UUID)** | `GPU-04280514-80a3-1dcf-1c5e-94e374365bef` |
| PCI bus | `00000000:01:00.0` |
| Total VRAM | 6141 MiB |
| Compute capability | 8.9 (Ada) |
| Driver (KMD) | 610.62 |
| NVRM / NVIDIA-SMI | 610.43.02 |
| CUDA UMD | 13.3 |
| Host OS | Windows 11 Home Single Language |
| Guest | Ubuntu 26.04 LTS on WSL 2, kernel 6.18.33.2 |
| Node address | `10.20.24.35/24` (DHCP, Realtek USB Ethernet, MAC `00-E0-4C-36-10-4A`) |
| K3s | agent `v1.37.2+k3s1`, containerd `2.3.7-k3s1` |
| Container toolkit | 1.20.2 |
| Serial | `N/A` under WSL |

## 2. Cluster join (A1)

- WSL 2 is configured with **mirrored networking** (`networkingMode=mirrored` in `%USERPROFILE%\.wslconfig`). In the default NAT mode the node registers an unreachable `172.24.x` address that the Ubuntu PCs cannot reach.
- The agent is installed with **no `--node-ip`**, deliberately. It auto-detects the address from the default route, which survives the DHCP lease changing. Hardcoding the address would break the node on every lease change.
- `k3s-agent` is a systemd unit, `enabled`, and reconnects to `https://10.20.24.31:6443` on start.
- Verified: node `Ready`, CPU smoke-test Pod `smoke-test-4050` completed with exit code 0, and Pod logs retrieved through kubelet on TCP 10250.

### Operational caveats

1. **The node is only Ready while a WSL session is open.** WSL terminates the VM once the last client session closes, which takes the node with it. A logon keep-alive is required:
   ```powershell
   $a = New-ScheduledTaskAction -Execute 'C:\Windows\System32\conhost.exe' `
        -Argument '--headless wsl.exe -d Ubuntu --exec sleep infinity'
   Register-ScheduledTask -TaskName 'CGPU-WSL-KeepAlive' -Action $a `
        -Trigger (New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME")
   ```
2. **After the address changes**, the agent must restart for flannel to update its endpoint. On restart it may crash-loop a few times with `failed to find interface with specified node ip` while the stale address is still on the Node object, then recover unattended.

## 3. Containerized CUDA (A2)

Configuration, in order:

```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit

sudo mkdir -p /etc/cdi
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml   # declares nvidia.com/gpu=all
sudo systemctl restart k3s-agent
```

k3s **auto-detects** the runtime on agent restart and registers it in
`/var/lib/rancher/k3s/agent/etc/containerd/config.toml` as `runtimes.'nvidia'` with
`BinaryName = "/usr/bin/nvidia-container-runtime"`.

> **Do not hand-edit that containerd config.** k3s regenerates it. The auto-detected configuration survived a full WSL restart on this node.

The cluster provides a matching `RuntimeClass` named `nvidia`, so Pods only need `runtimeClassName: nvidia`.

### Verification

```bash
sudo k3s ctr image pull nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0
sudo k3s ctr run --rm --runc-binary /usr/bin/nvidia-container-runtime \
  --env NVIDIA_VISIBLE_DEVICES=all --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
  nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0 a2-check
```

Expected and observed output:

```text
[Vector addition of 50000 elements]
CUDA kernel launch with 196 blocks of 256 threads
Test PASSED
Done
```

```text
GPU 0: NVIDIA GeForce RTX 4050 Laptop GPU (UUID: GPU-04280514-80a3-1dcf-1c5e-94e374365bef)
/dev/dxg present (WSL GPU passthrough)
```

Images cached on this worker: `nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0` (86.2 MB) and `docker.io/nvidia/cuda:12.6.2-base-ubuntu24.04` (87.4 MB). The same recipe reproduces on the RTX 3050 worker.

## 4. Platform limits that A3 and A4 must design around

### 4.1 No overlay networking on WSL workers

Flannel's VXLAN overlay **cannot** work on this node, and it is not a firewall problem.

Root cause: WSL mirrored networking publishes *userspace* listening sockets to the Windows host, but **not kernel sockets**. Flannel's VXLAN socket is a kernel socket — `ss -lunp` shows `0.0.0.0:8472` with no owning process — so inbound VXLAN arrives at the laptop's NIC, finds no host listener, and is discarded before entering the WSL network namespace.

Evidence, captured simultaneously on both sides of the boundary during an overlay ping:

| Capture point | Outbound VXLAN | Inbound VXLAN |
| --- | --- | --- |
| Windows NIC (`pktmon`) | 48 | **16** |
| WSL `eth5` (`tcpdump`) | 8 | **0** |

`UdpInErrors`, `UdpInCsumErrors` and `UdpNoPorts` inside WSL all remained 0, confirming nothing reached the WSL kernel — it is not a checksum or decapsulation fault.

Consequences:
- Pod-to-pod traffic and ClusterIP DNS (`10.43.0.10`) **do not work** from WSL GPU workers.
- Inbound **TCP works** — kubelet on 10250 is reachable, so `kubectl logs` and `exec` function normally.
- **Workloads on WSL GPU workers run with `hostNetwork: true` and `dnsPolicy: Default`.**

Ruled out, with tests: Hyper-V firewall rules, host Windows Firewall rules, `DefaultInboundAction=Allow` (applied, ineffective, reverted), symmetric VXLAN source port, and the campus network. **Bridged** networking would fix it properly by giving WSL its own L2 presence, but Windows 11 Home Single Language has no Hyper-V management and no external vSwitch. The only untried path to a real overlay is NAT mode plus IP forwarding on Windows and static routes on both PCs.

### 4.2 No per-process VRAM accounting

Measured under a real sustained CUDA load on this card:

| Metric | Under WSL |
| --- | --- |
| `name`, `uuid`, `pci.bus_id` | available |
| `memory.total` / `used` / `free` (device-level) | available — observed 0 → 19 MiB as the workload ran |
| `utilization.gpu`, `utilization.memory` | available |
| `temperature.gpu`, `power.draw`, `clocks.current.sm` | available |
| `--query-compute-apps` PIDs | available |
| `--query-compute-apps used_memory` | **`[N/A]`** |
| `nvidia-smi pmon` | **unusable** (all fields `-`) |
| `accounting.mode` | **Disabled** |
| `serial`, ECC, MIG | **N/A** |

Consequences:
- **A3:** quota enforcement cannot rely on observed per-process usage. CGPU's own allocation guard must be the source of truth, with `nvidia-smi` serving only as a device-total sanity check. No independent per-job cross-check exists on WSL workers, which should be stated in A3's evidence.
- **A4:** "measured VRAM usage" can only be exported **device-wide** for WSL workers. Per-job measured usage should be reported as unavailable rather than inferred.

### 4.3 No GPU device plugin — deliberate

There is no `nvidia.com/gpu` in this node's allocatable, because no device plugin is installed. This is correct for CGPU: A3 specifies that CGPU owns GPU discovery, binpacking and VRAM reservation, so a device plugin must not take ownership of the card. The `nvidia` RuntimeClass plus `NVIDIA_VISIBLE_DEVICES` is the handle CGPU uses to pin a job to a chosen device.

### 4.4 Host disk space

The laptop's Windows `C:` drive had **0.2 GB free of 200 GB**, which caused a real failure: a ~3 GB PyTorch image pull took the entire WSL VM down mid-pull. WSL reports ample free space inside `/`, but the backing virtual disk on `C:` cannot grow. The node recovered unattended and the runtime configuration survived.

**A3 and B4 cannot pull a PyTorch or training image on this worker until host disk space is freed.** Until then, prefer the 86 MB `cuda-sample` image for prototyping.

---

## 5. Provenance

Authored by Deep Patel (@Deeppatel40). The findings above were produced on the RTX 4050 laptop and committed from this account:

| Commit | Contribution |
| --- | --- |
| `cc9d9a2` | Established `COMMS.md`, the agent-to-agent coordination channel |
| `151b2d9` | Isolated the VXLAN drop to the Windows/WSL boundary via dual-side packet capture |
| `8363438` | Identified the root cause: mirrored mode does not publish kernel sockets |
| `1112f83` | Repository history maintenance and collaborator recovery instructions |
| `18cf128` | Containerized CUDA validated on the RTX 4050, with the reproduction recipe |
| `82b5dc2` | Measured the WSL telemetry limits; flagged the host disk constraint |

Cross-machine coordination with the control-plane work is logged chronologically in [`COMMS.md`](../COMMS.md).
