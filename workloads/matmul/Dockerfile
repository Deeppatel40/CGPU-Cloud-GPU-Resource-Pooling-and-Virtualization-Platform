# CGPU matmul workload image
# =============================================================================
# A deliberately small GPU-saturating workload: sustained tiled SGEMM, no dataset, no
# framework. Built on the CUDA *base* layer, so the image is ~450 MB against the 11.9 GB of
# workloads/demonstration - and it needs no 170 MB dataset download, whose upstream host
# serves at a few hundred bytes per second.
#
# Owner / engineer: Deep Patel (@Deeppatel40)
#
# The binary is compiled outside this Dockerfile (workloads/matmul/build.sh) and copied in,
# rather than built in a builder stage. That is on purpose: a builder stage would pull the
# 4 GB CUDA devel image just to run gcc, when the only CUDA dependency at build time is a
# libcuda stub for linking. The kernel itself travels as committed PTX, so no toolkit is
# needed in the image either - the driver JITs it for whatever GPU it lands on.
#
# Build:
#   ./build.sh build                 # compile the binary into ./build
#   docker build -t cgpu/workloads/gpu-matmul:1.0.0 .
#   docker save cgpu/workloads/gpu-matmul:1.0.0 | sudo k3s ctr images import -
FROM nvidia/cuda:12.6.2-base-ubuntu24.04

LABEL maintainer="CGPU Platform Team"
LABEL workload="gpu-matmul"
LABEL version="1.0.0"
LABEL description="Sustained tiled SGEMM to demonstrate GPU pooling, sharing and VRAM quotas"

# The workload links libcuda dynamically on purpose - that is what lets the CGPU allocation
# guard interpose its allocations - and the real driver is injected by the NVIDIA container
# runtime at run time.
ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility

COPY build/cgpu_matmul /usr/local/bin/cgpu_matmul

# Mount points the platform's Job template provides: parameters in, results out.
RUN mkdir -p /etc/cgpu /outputs
VOLUME ["/outputs", "/etc/cgpu"]

# Defaults are overridden by /etc/cgpu/params.json when the Job mounts it, and by flags.
ENTRYPOINT ["/usr/local/bin/cgpu_matmul"]
CMD ["--seconds", "120"]
