# CGPU device plugin image
# =============================================================================
# Runs the VRAM-unit device plugin as a DaemonSet, so every GPU node advertises
# cgpu.dev/vram-512mib to kube-scheduler without being set up by hand.
#
# Owner / engineer: Deep Patel (@Deeppatel40)
#
# On the CUDA base image because the plugin shells out to nvidia-smi for GPU
# discovery, which the NVIDIA container runtime injects at run time. The binary
# is a static Go build, so nothing else is needed.
FROM nvidia/cuda:12.6.2-base-ubuntu24.04

LABEL maintainer="CGPU Platform Team"
LABEL component="cgpu-device-plugin"
LABEL version="1.0.0"

ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=utility

COPY cgpu-device-plugin /usr/local/bin/cgpu-device-plugin
RUN chmod 0755 /usr/local/bin/cgpu-device-plugin

ENTRYPOINT ["/usr/local/bin/cgpu-device-plugin"]
