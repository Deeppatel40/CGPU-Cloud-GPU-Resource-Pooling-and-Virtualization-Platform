# CGPU — Cloud GPU Resource Pooling and Virtualization Platform ...

<p align="center">

**A cloud-based GPU-as-a-Service platform for intelligent GPU resource pooling, scheduling, and virtualization.**

</p>

<p align="center">

![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?style=for-the-badge\&logo=go\&logoColor=white)
![Next.js](https://img.shields.io/badge/Next.js-15+-000000?style=for-the-badge\&logo=next.js\&logoColor=white)
![MongoDB](https://img.shields.io/badge/MongoDB-8+-47A248?style=for-the-badge\&logo=mongodb\&logoColor=white)
![Kubernetes](https://img.shields.io/badge/Kubernetes-1.30+-326CE5?style=for-the-badge\&logo=kubernetes\&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-Containerized-2496ED?style=for-the-badge\&logo=docker\&logoColor=white)
![CUDA](https://img.shields.io/badge/NVIDIA-CUDA-76B900?style=for-the-badge\&logo=nvidia\&logoColor=white)

</p>

<p align="center">

![GPU](https://img.shields.io/badge/Computing-GPU-red?style=flat-square)
![Cloud](https://img.shields.io/badge/Cloud-IaaS-blue?style=flat-square)
![Resource Pooling](https://img.shields.io/badge/Resource-Pooling-purple?style=flat-square)
![Concurrency](https://img.shields.io/badge/Concurrency-Go%20Goroutines-brightgreen?style=flat-square)
![License](https://img.shields.io/badge/License-Apache--2.0-lightgrey?style=flat-square)

</p>

---

## Overview

**CGPU (Cloud GPU)** is a cloud-based **GPU-as-a-Service (GPUaaS)** platform designed to efficiently manage, virtualize, and share GPU resources across multiple computing nodes.

The platform creates a centralized **GPU resource pool** and allows users to submit GPU workloads without manually selecting a physical GPU. A GPU-aware scheduler identifies suitable resources based on GPU availability, memory, utilization, and allocation status.

CGPU provides a **GPU virtualization and resource-sharing layer** that enables multiple workloads to utilize a physical GPU when sufficient resources are available.

The system is designed around core distributed-system concepts including:

* GPU resource pooling
* GPU virtualization
* GPU-aware scheduling
* Concurrent workload handling
* Transaction-safe resource allocation
* Persistent resource management
* GPU monitoring
* Containerized workload execution

---

## Problem

GPUs are expensive computing resources and are frequently underutilized.

For example:

```text
Physical GPU
┌─────────────────────────────┐
│                             │
│  Job A → 20%                │
│                             │
│  UNUSED → 80%               │
│                             │
└─────────────────────────────┘
```

At the same time, another user may be waiting for a GPU.

CGPU addresses this problem by maintaining a pool of GPU resources and efficiently allocating available GPU capacity to multiple workloads.

```text
                 CGPU GPU Pool
                      │
        ┌─────────────┼─────────────┐
        ▼             ▼             ▼
     GPU Node 1    GPU Node 2    GPU Node 3
        │             │             │
      NVIDIA        NVIDIA        NVIDIA
        GPU           GPU           GPU
        │             │             │
      Jobs          Jobs          Jobs
```

---

# 🏗️ Architecture

CGPU consists of multiple components working together to provide GPU resource pooling, scheduling, virtualization, and workload execution.

```text
                         ┌───────────────────┐
                         │       Users       │
                         └─────────┬─────────┘
                                   │
                                   ▼
                         ┌───────────────────┐
                         │   Next.js Web UI  │
                         └─────────┬─────────┘
                                   │
                                   ▼
                         ┌───────────────────┐
                         │    CGPU API       │
                         │      (Go)         │
                         └─────────┬─────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    │              │              │
                    ▼              ▼              ▼
              ┌──────────┐  ┌────────────┐  ┌────────────┐
              │   Job    │  │   GPU      │  │  Resource  │
              │ Manager  │  │ Scheduler  │  │  Manager   │
              └──────────┘  └─────┬──────┘  └────────────┘
                                   │
                                   ▼
                         ┌───────────────────┐
                         │ GPU Virtualization│
                         │      Layer        │
                         └─────────┬─────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    │              │              │
                    ▼              ▼              ▼
               ┌────────┐     ┌────────┐     ┌────────┐
               │ GPU    │     │ GPU    │     │ GPU    │
               │ Node 1 │     │ Node 2 │     │ Node 3 │
               └────────┘     └────────┘     └────────┘
                    │              │              │
                    ▼              ▼              ▼
               NVIDIA GPU     NVIDIA GPU     NVIDIA GPU

                         │
                         ▼
                  ┌──────────────┐
                  │   MongoDB    │
                  └──────────────┘
```

---

# Key Features

### 🎮 GPU Resource Pooling

Multiple physical GPUs are represented as a centralized resource pool.

The scheduler tracks:

* GPU availability
* GPU memory
* GPU utilization
* Current allocations
* Running workloads
* Node status

---

### 🧩 GPU Virtualization

CGPU provides its own **GPU virtualization and resource-sharing layer** to enable multiple workloads to share a physical GPU.

Instead of:

```text
1 GPU → 1 Job
```

the platform can support:

```text
1 Physical GPU
      │
 ┌────┼────┐
 ▼    ▼    ▼
Job A Job B Job C
```

This improves GPU utilization for workloads that do not require the complete GPU.

The virtualization layer manages GPU resource allocation and enforces the requested resource limits for individual workloads.

---

### ⚙️ GPU-Aware Scheduling

The scheduler selects a suitable GPU based on workload requirements.

Example:

```text
Job Requirement:

GPU Memory: 8 GB
GPU Compute: Required
```

The scheduler evaluates available resources:

```text
GPU 1 → 4 GB available  ❌

GPU 2 → 12 GB available ✅

GPU 3 → 6 GB available  ❌
```

The job is therefore scheduled on **GPU 2**.

---

### 🚀 Concurrent Workloads

Multiple users can submit jobs simultaneously.

CGPU uses Go's concurrency mechanisms to handle concurrent requests and scheduling operations.

```text
User A ──┐
User B ──┤
User C ──┼──► Go Scheduler
User D ──┤
User E ──┘
```

---

### 🔐 Transaction-Safe Allocation

The system prevents multiple users from allocating the same GPU resource simultaneously.

Conceptually:

```text
BEGIN TRANSACTION

Check GPU availability
        ↓
Reserve GPU
        ↓
Create allocation
        ↓
COMMIT
```

If allocation fails:

```text
ROLLBACK
```

This ensures consistent resource state.

---

### 💾 Persistence

MongoDB stores:

* Users
* GPU Nodes
* GPU Resources
* Jobs
* Allocations
* Job Status
* Execution History
* Results

This allows resource and job information to survive application restarts.

---

### 📊 GPU Monitoring

The platform can monitor:

* GPU utilization
* GPU memory utilization
* Running workloads
* Job execution time
* Resource allocation
* Node status

Optional monitoring stack:

```text
GPU Nodes
    │
    ▼
Prometheus
    │
    ▼
Grafana
```

---

# 🧰 Technology Stack

| Layer                 | Technology                    |
| --------------------- | ----------------------------- |
| Frontend              | Next.js                       |
| Backend               | Go (Golang)                   |
| API                   | REST API                      |
| Database              | MongoDB                       |
| Containerization      | Docker                        |
| Orchestration         | Kubernetes                    |
| GPU Computing         | NVIDIA CUDA                   |
| GPU Container Support | NVIDIA Container Toolkit      |
| GPU Management        | NVIDIA GPU Operator           |
| GPU Virtualization    | CGPU GPU Virtualization Layer |
| Monitoring            | Prometheus + Grafana          |
| Version Control       | Git / GitHub                  |

---

# ☁️ Cloud Service Model

CGPU primarily targets:

## **IaaS — Infrastructure as a Service**

The platform provides GPU computing infrastructure as a service.

Instead of users directly managing physical GPU machines, they can request GPU resources through the CGPU platform.

```text
User
 │
 │ GPU Request
 ▼
CGPU Platform
 │
 ▼
GPU Resource Pool
 │
 ▼
Allocated GPU
 │
 ▼
Workload Execution
```

---

# 👨‍💻 Example Use Case & API Contract

A user submits a PyTorch image classification workload requesting fractional GPU capacity:

```json
{
  "request_id": "req-7b3f9408-e4b2-4d56-b072-20c2420a8511",
  "name": "resnet18-cifar10-run-1",
  "workload_id": "pytorch-resnet18-cifar10",
  "requested_vram_mib": 2048,
  "parameters": {
    "epochs": 5,
    "batch_size": 64,
    "learning_rate": 0.001,
    "optimizer": "adam"
  },
  "dataset": {
    "type": "builtin",
    "name": "cifar10",
    "uri": "builtin://cifar10"
  },
  "priority": 5,
  "timeout_seconds": 3600
}
```

Detailed API specifications, OpenAPI contracts, and mock responses are documented in:
* [Job API Specification](docs/api/job-api-specification.md)
* [Scheduler Handoff Specification](docs/api/scheduler-handoff-spec.md)
* [OpenAPI 3.1 Contract](api/openapi/openapi.yaml)
* [Mock Responses Directory](api/mocks/)

CGPU:

1. Receives the request.
2. Validates the user.
3. Creates a job.
4. Checks the GPU resource pool.
5. Finds a suitable GPU.
6. Reserves the required resource.
7. Deploys the workload through Kubernetes.
8. Configures the CGPU GPU virtualization layer when GPU sharing is required.
9. Monitors the workload.
10. Stores the job status and result.
11. Returns the result to the user.

---

# 📈 Evaluation Metrics

The platform can be evaluated using:

| Metric                 | Description                                  |
| ---------------------- | -------------------------------------------- |
| GPU Utilization        | Percentage of GPU capacity being used        |
| GPU Memory Utilization | Amount of VRAM being utilized                |
| Job Execution Time     | Time required to complete a workload         |
| Job Waiting Time       | Time spent waiting for resources             |
| Throughput             | Number of completed jobs per unit time       |
| Scheduling Overhead    | Time required to make allocation decisions   |
| Resource Efficiency    | Effective utilization of allocated resources |
| Concurrent Jobs        | Number of workloads running simultaneously   |
| Allocation Conflicts   | Number of failed/conflicting allocations     |

---

# 🧪 Comparison

CGPU can be evaluated against traditional GPU allocation.

### Traditional Allocation

```text
GPU
 │
 └── Job A

GPU utilization → potentially low
```

### CGPU + GPU Virtualization

```text
GPU
 │
 ├── Job A
 ├── Job B
 └── Job C

GPU utilization → potentially higher
```

The evaluation will determine whether GPU resource pooling and virtualization can improve overall GPU utilization and workload throughput.

---

# 🚀 Getting Started

> **Note:** Setup instructions will be added as the implementation progresses.

### Prerequisites

* Linux-based system
* NVIDIA GPU
* NVIDIA drivers
* CUDA
* Docker
* Kubernetes
* NVIDIA Container Toolkit
* MongoDB
* Go
* Node.js / Next.js

### Clone the Repository

```bash
git clone https://github.com/<your-username>/cgpu.git
cd cgpu
```

---

# 🔬 Project Scope

CGPU focuses on the intersection of:

**Cloud Computing + GPU Computing + GPU Virtualization + Resource Pooling + Scheduling + Distributed Systems**

The project includes the design and implementation of a **custom GPU virtualization and resource-sharing layer** along with the cloud-facing resource management, scheduling, job management, persistence, and user interface.

The virtualization layer is responsible for enabling multiple workloads to share physical GPU resources while managing resource allocation, resource limits, and workload isolation.

CGPU aims to provide an end-to-end platform covering:

* GPU resource discovery
* GPU resource pooling
* GPU-aware scheduling
* GPU virtualization
* Fractional GPU allocation
* GPU memory management
* Multi-tenant workload execution
* Containerized workload deployment
* Resource isolation
* GPU monitoring
* Job lifecycle management
* Persistent resource and workload state

---

# Contributing

Contributions, suggestions, and improvements are welcome.

1. Fork the repository.
2. Create issue branch. Name: `<#issueno>`
3. Make your changes.
4. Add appropriate tests.
5. Submit a pull request.

---

# 📄 License

This project will be released under the **Apache License 2.0**.

---

`gpu` `gpu-computing` `gpu-cloud` `cloud-computing` `gpu-virtualization` `gpu-sharing` `gpu-scheduling` `gpu-resource-management` `resource-pooling` `gpu-as-a-service` `iaas` `kubernetes` `docker` `cuda` `nvidia` `golang` `go` `mongodb` `distributed-systems` `cloud-native` `multi-tenant` `resource-management` `gpu-cluster` `gpu-orchestration` `nextjs` `prometheus` `grafana`
