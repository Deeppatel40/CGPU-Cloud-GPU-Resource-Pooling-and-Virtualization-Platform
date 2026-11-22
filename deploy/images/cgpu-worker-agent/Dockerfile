# CGPU worker agent image
# =============================================================================
# Lets the agent run as a DaemonSet instead of being started by hand on each
# laptop.
#
# Owner / engineer: Deep Patel (@Deeppatel40)
#
# Why: the agent is the only source of GPU and per-job VRAM telemetry, so a
# worker without it shows "unavailable" for every measured figure in the portal
# and contributes nothing to Grafana. Started by hand it dies with the shell,
# and on a WSL worker the whole distro idles out and takes it with it - so a
# node that looks Ready can be silently reporting nothing. As a DaemonSet,
# Kubernetes starts it on every GPU node and restarts it when it dies, which
# also covers nodes that cannot be reached over SSH.
#
# On the CUDA base image because the agent shells out to nvidia-smi, which the
# NVIDIA container runtime injects at run time - the image itself carries no
# driver. The binary is a static Go build, so nothing else is needed.
#
# Build (from the repo root):
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
#       go build -buildvcs=false -o deploy/images/cgpu-worker-agent/cgpu-worker-agent \
#       ./cmd/cgpu-worker-agent
#   docker build -t cgpu/cgpu-worker-agent:1.0.0 deploy/images/cgpu-worker-agent
FROM nvidia/cuda:12.6.2-base-ubuntu24.04

LABEL maintainer="CGPU Platform Team"
LABEL component="cgpu-worker-agent"
LABEL version="1.0.0"

ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility

COPY cgpu-worker-agent /usr/local/bin/cgpu-worker-agent
# Explicit, because the build context is on a Windows drive where the executable
# bit does not survive reliably - a non-executable copy produced "Permission
# denied" at run time once already.
RUN chmod 0755 /usr/local/bin/cgpu-worker-agent

EXPOSE 9400
ENTRYPOINT ["/usr/local/bin/cgpu-worker-agent"]
CMD ["-listen", "0.0.0.0:9400", "-guard-status-dir", "/var/run/cgpu/guard", "-reserve-mib", "512"]
