#!/usr/bin/env bash
# Phase 2 - Jetson: build the TensorRT engine for a model folder from the ZBook
# and install it as the current model.
#
#   scripts/jetson/80-engine.sh --from DIR    model folder copied from the ZBook (85-push-model.sh or USB)
#   scripts/jetson/80-engine.sh --stub        stand-in model: tests TensorRT on this Jetson before training is done
#   [--batch N]       largest batch of tiles per call (default 6 = one 1080p frame)
#   [--workspace MB]  TensorRT builder workspace (default 1024)
#
# Building takes about 5-15 minutes (FP16). An engine only works on the TensorRT
# version and GPU it was built on, so it is always built here, never copied.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
source "$SAR_ROOT/scripts/lib/models.sh"

FROM=""; STUB=0; BATCH=6; WS=1024
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) FROM="${2:-}"; shift 2 ;;
    --stub) STUB=1; shift ;;
    --batch) BATCH="${2:-}"; shift 2 ;;
    --workspace) WS="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 --from DIR | --stub [--batch N] [--workspace MB]" ;;
  esac
done
[[ -n "$FROM" || $STUB -eq 1 ]] || die E-SETUP-ARG "--from DIR or --stub is required"
[[ "$BATCH" =~ ^[1-9][0-9]?$ && "$WS" =~ ^[0-9]+$ ]] || die E-SETUP-ARG "--batch 1..99 and --workspace in MB"
TRTEXEC=/usr/src/tensorrt/bin/trtexec
[[ -x $TRTEXEC ]] || die E-SETUP-TRT "trtexec not found" "run scripts/jetson/75-tensorrt.sh"
INFER="$(tool_bin sar-infer)"
HELP="$("$INFER" help 2>&1 || true)"
[[ "$HELP" == *"TensorRT true"* ]] \
  || die E-INF-002 "sar-infer was built without TensorRT" "run scripts/jetson/75-tensorrt.sh, then 20-build.sh and 30-install.sh again"

if [[ $STUB -eq 1 ]]; then
  SRC="$SAR_ROOT/data/models/stub"
  step "Stand-in model (fixed outputs, not trained)"
  "$INFER" stub-model --out "$SRC" | tail -n1
else
  SRC="$(resolve_path "${FROM%/}")"
  [[ -d "$SRC" ]] || die E-SETUP-MODEL "model folder $SRC not found" "pass the folder copied from the ZBook, e.g. --from ~/rubblesar-data/models/<name>"
  step "Checking the model folder $SRC"
  for f in model.onnx model.json; do [[ -f "$SRC/$f" ]] || die E-SETUP-MODEL "$SRC/$f missing" "copy the whole folder (scripts/zbook/85-push-model.sh)"; done
  WANT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["onnx_sha256"])' "$SRC/model.json")"
  GOT="$(sha256sum "$SRC/model.onnx" | cut -d" " -f1)"
  [[ "$WANT" == "$GOT" ]] || die E-INF-001 "model.onnx does not match model.json (sha256 $GOT, expected $WANT)" "copy the folder again"
  ok "model.onnx verified"
fi
NAME="$(model_name "$SRC")"
WORK="$SAR_ROOT/data/models/$NAME-engine"
rm -rf "$WORK"
cp -a "$SRC" "$WORK"
need_sudo

step "Building the TensorRT FP16 engine for $NAME (batch 1..$BATCH; about 5-15 minutes)"
LOG="$LOG_DIR/trtexec-$NAME-$(date -u +%H%M%S).log"
info "full builder output: $LOG"
if ! "$TRTEXEC" --onnx="$WORK/model.onnx" --saveEngine="$WORK/model.engine" --fp16 \
    --minShapes=images:1x3x640x640 --optShapes=images:${BATCH}x3x640x640 --maxShapes=images:${BATCH}x3x640x640 \
    --memPoolSize=workspace:"$WS" >"$LOG" 2>&1; then
  echo "    ---- last 25 lines of $LOG ----"
  tail -n 25 "$LOG" | sed 's/^/    /'
  die E-SETUP-TRT "trtexec could not build the engine" "see the lines above; low memory? close other programs, or try --workspace 512"
fi
grep -E "Throughput|GPU Compute Time: min" "$LOG" | tail -n 2 | sed 's/^/    trtexec: /' || true
TRT_VER="$(grep -oE "TensorRT version: [0-9.]+" "$LOG" | head -n1 | awk '{print $3}' || true)"
[[ -n "$TRT_VER" ]] || TRT_VER="$(dpkg-query -W -f='${Version}' libnvinfer10 2>/dev/null || echo unknown)"
DEVICE="$({ tr -d '\0' </proc/device-tree/model; } 2>/dev/null || echo Jetson)"
python3 - "$WORK" "$TRT_VER" "$BATCH" "$DEVICE" <<'PY'
import datetime, json, sys
d, ver, batch, dev = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
card = json.load(open(f"{d}/model.json"))
json.dump({"engine": "model.engine", "onnx_sha256": card["onnx_sha256"], "tensorrt": ver, "precision": "fp16",
           "max_batch": batch, "built_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
           "device": dev}, open(f"{d}/engine.json", "w"), indent=2)
PY
ok "engine $(du -h "$WORK/model.engine" | cut -f1), TensorRT $TRT_VER"

step "Loading the engine from Go"
"$INFER" info --model "$WORK" | sed 's/^/    /' \
  || die E-INF-005 "the engine was built but Go could not load it" "paste the block above; the trtexec log is $LOG"

if [[ $STUB -eq 1 ]]; then
  step "Running whole frames through TensorRT (the stand-in is a tiny network, so this is not the real speed)"
  "$INFER" bench --model "$WORK" --frames 5 | tail -n 2 | sed 's/^/    /' \
  || die E-INF-005 "the engine loaded but could not run a frame" "paste the block above"
  echo
  echo "    TensorRT works on this Jetson. Next: the real model from the ZBook (--from)."
  finish "after the ZBook finished training: scripts/jetson/80-engine.sh --from ~/rubblesar-data/models/<name>"
  exit 0
fi

if [[ -f "$WORK/golden/golden.json" ]]; then
  step "Agreement with the ZBook reference (golden tiles)"
  "$INFER" parity --model "$WORK" | tail -n 4 | sed 's/^/    /' || warn "the engine disagrees with the ZBook reference (90-phase2-test.sh shows the details)"
fi

step "Installing into $MODEL_ROOT/$NAME"
install_model "$WORK" "$NAME"
rm -rf "$WORK"
finish "scripts/jetson/90-phase2-test.sh"
