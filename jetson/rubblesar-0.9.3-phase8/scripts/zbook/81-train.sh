#!/usr/bin/env bash
# Phase 2 - ZBook: train the body-part segmentation model on the GPU in Docker.
#
#   scripts/zbook/81-train.sh [--dataset DIR] [--model yolo11s-seg|yolo11n-seg] [--epochs N]
#                             [--batch N] [--name NAME] [--quick] [--rebuild]
#
# --dataset DIR  the folder holding data.yaml. Default: data/datasets/rubblesar-v1 (made by
#                80-dataset.sh), or data/datasets/rubblesar-v1-quick with --quick (80-dataset.sh --quick).
#                A relative path may be given from the release folder or from the current folder.
# --model        starting weights: yolo11s-seg (default, more accurate) or yolo11n-seg (faster on the Jetson)
# --epochs N     default 80 (training stops early when it stops improving)
# --quick        3 epochs on 10% of the data: checks the whole chain in about 5 minutes
# --rebuild      rebuild the training image
#
# First run downloads the PyTorch image (about 8 GB) and builds rubblesar-train.
# Full training takes about 1-3 hours. Run it inside 'tmux' so a closed terminal
# does not stop it (tmux new -s train; reattach later with: tmux attach -t train).
# Output: data/runs/<name>/ (Ultralytics run with plots) and data/runs/<name>/export/{model.onnx,model.json}
source "$(dirname "$0")/../lib/common.sh"
need_data_dir

DATASET=""
MODEL=yolo11s-seg
EPOCHS=80
BATCH=16
NAME=""
QUICK=0
REBUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dataset) DATASET="${2:-}"; shift 2 ;;
    --model) MODEL="${2:-}"; shift 2 ;;
    --epochs) EPOCHS="${2:-}"; shift 2 ;;
    --batch) BATCH="${2:-}"; shift 2 ;;
    --name) NAME="${2:-}"; shift 2 ;;
    --quick) QUICK=1; shift ;;
    --rebuild) REBUILD=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--dataset DIR] [--model yolo11s-seg|yolo11n-seg] [--epochs N] [--batch N] [--name NAME] [--quick] [--rebuild]" ;;
  esac
done
case "$MODEL" in
  yolo11s-seg) WSUM="$YOLO11S_SEG_SHA256" ;;
  yolo11n-seg) WSUM="$YOLO11N_SEG_SHA256" ;;
  *) die E-SETUP-ARG "--model must be yolo11s-seg or yolo11n-seg" ;;
esac
[[ "$EPOCHS" =~ ^[0-9]+$ && "$BATCH" =~ ^[0-9]+$ ]] || die E-SETUP-ARG "--epochs and --batch take numbers"
# Default dataset: the one 80-dataset.sh makes (the -quick one with --quick).
if [[ -z "$DATASET" ]]; then
  DATASET="$SAR_ROOT/data/datasets/rubblesar-v1"
  [[ $QUICK -eq 1 ]] && DATASET="$DATASET-quick"
fi
# A relative path works from the current folder or from the release folder.
[[ "$DATASET" == /* || -e "$DATASET" || ! -e "$SAR_ROOT/$DATASET" ]] || DATASET="$SAR_ROOT/$DATASET"
if [[ ! -f "$DATASET/data.yaml" ]]; then
  info "looked for: $DATASET/data.yaml"
  info "datasets found in $SAR_ROOT/data/datasets:"
  found=0
  for d in "$SAR_ROOT"/data/datasets/*/; do
    [[ -f "$d/data.yaml" ]] && { info "  ${d%/}"; found=1; }
  done
  [[ $found -eq 1 ]] || info "  (none)"
  if [[ $QUICK -eq 1 ]]; then
    die E-TRN-002 "dataset not found: $DATASET" "build it with scripts/zbook/80-dataset.sh --quick, or pass one of the folders listed above with --dataset"
  fi
  die E-TRN-002 "dataset not found: $DATASET" "build the full dataset with scripts/zbook/80-dataset.sh (15-25 minutes), or pass one of the folders listed above with --dataset"
fi
DATASET="$(cd "$DATASET" && pwd -P)"   # real path: Docker mounts it
if [[ -z "$NAME" ]]; then
  NAME="$(basename "$DATASET")-$MODEL"
  [[ $QUICK -eq 1 ]] && NAME="$NAME-quick"
fi
RUNS="$SAR_ROOT/data/runs"
mkdir -p "$RUNS"
RUNS="$(cd "$RUNS" && pwd -P)"

step "Docker"
need_cmd docker docker.io
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  need_sudo
  sudo docker info >/dev/null 2>&1 || die E-SETUP-DOCKER "docker is not running" "sudo systemctl start docker"
  DOCKER=(sudo docker)
  warn "using 'sudo docker' (add yourself to the docker group to avoid this: sudo usermod -aG docker $USER, then log out and in)"
fi
ok "$("${DOCKER[@]}" version --format 'docker {{.Server.Version}}')"

step "GPU and driver"
need_cmd nvidia-smi nvidia-driver
GPU_LINE="$(nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader | head -n1)" \
  || die E-SETUP-GPU "nvidia-smi failed" "check the NVIDIA driver: nvidia-smi"
DRIVER_MAJOR="$(echo "$GPU_LINE" | awk -F', ' '{split($2,a,"."); print a[1]}')"
BASE_IMAGE="$TRAIN_BASE_IMAGE"
if [[ "${DRIVER_MAJOR:-0}" -lt "$TRAIN_MIN_DRIVER" ]]; then
  BASE_IMAGE="$TRAIN_BASE_IMAGE_FALLBACK"
  warn "driver $DRIVER_MAJOR is older than $TRAIN_MIN_DRIVER: using $BASE_IMAGE"
fi
ok "$GPU_LINE; base image $BASE_IMAGE"

step "Starting weights (checksum-verified)"
fetch "https://github.com/ultralytics/assets/releases/download/v8.3.0/$MODEL.pt" "$MODEL.pt" "$WSUM"
fetch "https://github.com/ultralytics/assets/releases/download/v8.3.0/yolo11n.pt" "yolo11n.pt" "$YOLO11N_SHA256"
fetch "https://github.com/ultralytics/assets/releases/download/v0.0.0/Arial.ttf" "Arial.ttf" "$ARIAL_TTF_SHA256"

step "Training image"
CTX_HASH="$( (cat "$SAR_ROOT/training/Dockerfile" "$SAR_ROOT/training/rubblesar_train.py"; echo "$BASE_IMAGE $ULTRALYTICS_VERSION") | sha256sum | cut -c1-12)"
IMAGE="rubblesar-train:$CTX_HASH"
if [[ $REBUILD -eq 1 ]] || ! "${DOCKER[@]}" image inspect "$IMAGE" >/dev/null 2>&1; then
  CTX="$(mktemp -d)"
  mkdir -p "$CTX/weights"
  cp "$SAR_ROOT/training/Dockerfile" "$SAR_ROOT/training/rubblesar_train.py" "$CTX/"
  cp "$DOWNLOAD_DIR/yolo11n.pt" "$CTX/weights/"
  cp "$DOWNLOAD_DIR/$MODEL.pt" "$CTX/weights/"
  for m in yolo11s-seg yolo11n-seg; do [[ -f "$DOWNLOAD_DIR/$m.pt" ]] && cp "$DOWNLOAD_DIR/$m.pt" "$CTX/weights/"; done
  cp "$DOWNLOAD_DIR/Arial.ttf" "$CTX/"
  info "building $IMAGE from $BASE_IMAGE (first time: about 10 minutes and an 8 GB download)"
  "${DOCKER[@]}" build --build-arg "BASE_IMAGE=$BASE_IMAGE" --build-arg "ULTRALYTICS_VERSION=$ULTRALYTICS_VERSION" -t "$IMAGE" "$CTX" \
    || die E-SETUP-DOCKER "docker build failed" "check internet access (Docker Hub, PyPI) and free disk space ('docker system df')"
  rm -rf "$CTX"
fi
ok "image $IMAGE"
# The weights inside the image must include the chosen model.
"${DOCKER[@]}" run --rm --entrypoint test "$IMAGE" -f "/opt/rubblesar/weights/$MODEL.pt" \
  || die E-SETUP-DOCKER "$MODEL.pt is not in $IMAGE" "re-run with --rebuild"

step "GPU inside Docker"
"${DOCKER[@]}" run --rm --gpus all "$IMAGE" check \
  || die E-TRN-001 "the training container cannot use the GPU" "docker run --rm --gpus all ubuntu nvidia-smi must work; see docs/PHASE2.md (GPU in Docker)"

if [[ $QUICK -eq 1 ]]; then
  step "Quick training run ($MODEL, 3 epochs on 10% of $DATASET)"
else
  step "Training $MODEL for up to $EPOCHS epochs on $DATASET (about 1-3 hours)"
fi
TRAIN_ARGS=(train --data /data/data.yaml --weights "/opt/rubblesar/weights/$MODEL.pt" --out /runs --name "$NAME" --epochs "$EPOCHS" --batch "$BATCH")
[[ $QUICK -eq 1 ]] && TRAIN_ARGS+=(--quick)
"${DOCKER[@]}" run --rm --gpus all --ipc=host \
  -v "$DATASET:/data" -v "$RUNS:/runs" \
  -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" \
  "$IMAGE" "${TRAIN_ARGS[@]}" \
  || die E-TRN-003 "training did not finish" "the block printed by the container above has the details (E-TRN-...)"

EXPORT="$RUNS/$NAME/export"
[[ -f "$EXPORT/model.onnx" && -f "$EXPORT/model.json" ]] || die E-TRN-005 "no model in $EXPORT" "see the container output above"
step "Result"
info "run folder: $RUNS/$NAME (results.png, confusion_matrix.png, val_batch*_pred.jpg)"
python3 - "$EXPORT/model.json" <<'PY' || true
import json, sys
c = json.load(open(sys.argv[1]))
m = c.get("metrics", {})
print(f"    {c['name']}: {c['arch']}, {c['epochs']} epochs in {c['train_minutes']} min on {c['gpu']}")
for split in ("val", "test"):
    if f"{split}.all.mask.map50" in m:
        print(f"    {split:4}: mask mAP50 {m[f'{split}.all.mask.map50']:.3f}  mAP50-95 {m[f'{split}.all.mask.map50_95']:.3f}  "
              f"P {m[f'{split}.all.mask.p']:.3f}  R {m[f'{split}.all.mask.r']:.3f}")
PY
finish "scripts/zbook/82-evaluate.sh --run $RUNS/$NAME --dataset $DATASET$([[ $QUICK -eq 1 ]] && echo " --quick")"
