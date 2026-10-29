#!/usr/bin/env bash
# Phase 2 - ZBook: turn a training run into a model folder, check it and
# install it as the current model.
#
#   scripts/zbook/82-evaluate.sh [--run DIR] [--dataset DIR] [--quick]
#
# --run DIR      training run (default: the newest data/runs/*/ with an export)
# --dataset DIR  the dataset it was trained on (default data/datasets/rubblesar-v1)
# --quick        evaluate every third held-out frame
#
# Steps: package (model.onnx, model.json, test-frame list) -> golden set (reference
# results for the Jetson) -> Ultralytics predictions on the golden tiles (Docker)
# -> evaluation on the held-out scenario -> install to /var/lib/rubblesar/models/<name>.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
source "$SAR_ROOT/scripts/lib/models.sh"
need_data_dir

RUN=""
DATASET="$SAR_ROOT/data/datasets/rubblesar-v1"
QUICK=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --run) RUN="${2:-}"; shift 2 ;;
    --dataset) DATASET="${2:-}"; shift 2 ;;
    --quick) QUICK=(--quick); shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--run DIR] [--dataset DIR] [--quick]" ;;
  esac
done
DATASET="$(resolve_path "$DATASET")"
RUN="$(resolve_path "$RUN")"
if [[ -z "$RUN" ]]; then
  RUN="$(ls -1td "$SAR_ROOT"/data/runs/*/ 2>/dev/null | while read -r d; do [[ -f "$d/export/model.onnx" ]] && { echo "${d%/}"; break; }; done || true)"
  [[ -n "$RUN" ]] || die E-SETUP-MODEL "no training run with an export under data/runs" "run scripts/zbook/81-train.sh first"
fi
[[ -f "$RUN/export/model.json" ]] || die E-SETUP-MODEL "$RUN/export/model.json missing" "training did not finish; re-run 81-train.sh"
[[ -f "$DATASET/data.yaml" ]] || die E-DS-001 "$DATASET is not a dataset" "pass --dataset (the folder given to 81-train.sh)"
INFER="$(tool_bin sar-infer)"
NAME="$(model_name "$RUN/export")"
RUN="$(cd "$RUN" && pwd -P)"   # real paths: Docker mounts them
mkdir -p "$SAR_ROOT/data/models"
MDIR="$(cd "$SAR_ROOT/data/models" && pwd -P)/$NAME"
OVERLAYS="$SAR_ROOT/data/overlays/$NAME"
SCEN="$SCENARIO_ROOT/antakya-demo"
info "$("$INFER" version | head -n1)"
info "run $RUN -> model $NAME"
need_sudo

step "Model folder $MDIR"
rm -rf "$MDIR" "$OVERLAYS"
"$INFER" package --export "$RUN/export" --dataset "$DATASET" --out "$MDIR" | tail -n1

step "Golden set (reference results for the Jetson, ONNX Runtime)"
"$INFER" golden --model "$MDIR" --dataset "$DATASET" | tail -n1

step "Ultralytics predictions on the golden tiles (training container)"
DOCKER=(docker)
docker info >/dev/null 2>&1 || DOCKER=(sudo docker)
IMAGE="$("${DOCKER[@]}" images --format '{{.Repository}}:{{.Tag}}' rubblesar-train 2>/dev/null | head -n1 || true)"
if [[ -n "$IMAGE" && -f "$RUN/export/best.pt" ]]; then
  "${DOCKER[@]}" run --rm --gpus all -v "$MDIR/golden:/golden" -v "$RUN/export:/export:ro" \
    -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" "$IMAGE" \
    predict --model /export/best.pt --images /golden --out /golden/ultralytics.json \
    || warn "the Ultralytics comparison could not be made (P2-03 will warn); details above"
else
  warn "no rubblesar-train image or best.pt: skipping the Ultralytics comparison (P2-03 will warn)"
fi

step "Evaluation on the held-out scenario (ONNX Runtime on the CPU, about 3-6 minutes)"
"$INFER" eval --model "$MDIR" --scenario "$SCEN" --json "$MDIR/eval-zbook.json" --overlays "$OVERLAYS" "${QUICK[@]}" | tail -n 25

step "Installing into $MODEL_ROOT/$NAME"
install_model "$MDIR" "$NAME"

echo
echo "    Pictures of detections (white: labelled parts, colour: model): $OVERLAYS/"
finish "scripts/zbook/90-phase2-test.sh, then copy the model to the Jetson: scripts/zbook/85-push-model.sh --jetson user@<jetson-ip>"
