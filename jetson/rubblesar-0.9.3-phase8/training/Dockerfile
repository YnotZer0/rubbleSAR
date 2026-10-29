# RubbleSAR training image (ZBook GPU)                           RubbleSAR phase 2
#
# Built by scripts/zbook/81-train.sh, which first downloads and checksum-verifies
# the starting weights and font into the build context (weights/, Arial.ttf).
# Ultralytics is AGPL-3.0 (plan decision D2: the code stays in house).

ARG BASE_IMAGE=pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime
FROM ${BASE_IMAGE}

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    MKL_THREADING_LAYER=GNU \
    OMP_NUM_THREADS=1 \
    YOLO_CONFIG_DIR=/opt/rubblesar/config \
    MPLCONFIGDIR=/tmp/matplotlib

# OpenCV (pulled in by ultralytics) needs these libraries.
RUN apt-get update && \
    apt-get install -y --no-install-recommends libgl1 libglib2.0-0 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

ARG ULTRALYTICS_VERSION=8.3.253
RUN pip install "ultralytics==${ULTRALYTICS_VERSION}" "onnx>=1.12.0,<2.0.0" "onnxslim>=0.1.71" "onnxruntime-gpu>=1.20,<2" && \
    mkdir -p /opt/rubblesar/config/Ultralytics && \
    pip freeze > /opt/rubblesar/pip-freeze.txt

# Starting weights (YOLO11 segmentation, COCO-pretrained) and the AMP-check model,
# so training needs no downloads at run time.
COPY weights/ /opt/rubblesar/weights/
COPY Arial.ttf /opt/rubblesar/config/Ultralytics/Arial.ttf
COPY rubblesar_train.py /opt/rubblesar/rubblesar_train.py

WORKDIR /opt/rubblesar/weights
ENTRYPOINT ["python", "/opt/rubblesar/rubblesar_train.py"]
