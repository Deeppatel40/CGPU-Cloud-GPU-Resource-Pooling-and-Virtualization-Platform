FROM pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime

LABEL maintainer="CGPU Platform Team"
LABEL workload="pytorch-resnet18-cifar10"
LABEL version="1.0.0"

ENV PYTHONUNBUFFERED=1
ENV CGPU_ALLOCATED_VRAM_MIB=2048

WORKDIR /app

# Install lightweight dependencies
RUN pip install --no-cache-dir torchvision

# Copy training script
COPY train.py /app/train.py

# Create outputs mount directory
RUN mkdir -p /outputs/checkpoints /etc/cgpu /data/cifar10

VOLUME ["/outputs", "/etc/cgpu", "/data"]

ENTRYPOINT ["python", "/app/train.py"]

