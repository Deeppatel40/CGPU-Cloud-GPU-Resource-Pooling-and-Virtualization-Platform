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

# The shared Job template appends `python /opt/cgpu-tools/upload.py /outputs` after the
# workload to collect artifacts, so an image without `python` on PATH cannot upload its
# results - and the failure is quiet, because the template swallows it with `|| echo`.
# Observed exactly that on the first cluster run: the workload finished with RESULT=OK and
# then logged "/bin/sh: 1: python: not found".
#
# The alternative - changing the template to call python3 - would have to be right for
# every workload image, including the conda-based ResNet one, so fixing this image is the
# narrower change.
#
# python3 rather than python3-minimal: minimal omits the standard library, and the uploader
# imports urllib.request, which failed with "No module named 'http'". That cost a second
# round trip, so it is worth stating plainly - "minimal" here means no stdlib, not a
# slimmer stdlib.
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 \
    && ln -sf /usr/bin/python3 /usr/bin/python \
    && rm -rf /var/lib/apt/lists/*

# COPY carries the source file's mode, and the build context lives on a Windows
# drive where the executable bit does not survive reliably: a context file that
# came out -rw-r--r-- produced an image whose entrypoint failed at run time with
#
#   /bin/sh: 1: /usr/local/bin/cgpu_matmul: Permission denied
#
# and, because the workload never ran, the follow-on symptom was the uploader
# reporting "no output files in /outputs" - which points away from the cause.
# Setting the mode here makes the image correct whatever the context file's mode
# happens to be. chmod in a RUN rather than COPY --chmod so the build does not
# require BuildKit.
COPY build/cgpu_matmul /usr/local/bin/cgpu_matmul
RUN chmod 0755 /usr/local/bin/cgpu_matmul

# Mount points the platform's Job template provides: parameters in, results out.
RUN mkdir -p /etc/cgpu /outputs
VOLUME ["/outputs", "/etc/cgpu"]

# Defaults are overridden by /etc/cgpu/params.json when the Job mounts it, and by flags.
ENTRYPOINT ["/usr/local/bin/cgpu_matmul"]
CMD ["--seconds", "120"]
