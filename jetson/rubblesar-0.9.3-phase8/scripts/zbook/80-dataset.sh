#!/usr/bin/env bash
# Phase 2 - ZBook: build the training set for the body-part model.
#
#   scripts/zbook/80-dataset.sh [--config FILE] [--quick]
#
# --config FILE  dataset config (default configs/dataset/v1.toml)
# --quick        a small smoke-test dataset (2 + 2 scenarios, 30 frames each, about 3 minutes)
#
# Output: data/datasets/<name>/ (about 1-2 GB). An interrupted run continues where it stopped.
# Needs the installed scenario (scripts/zbook/60-scenario.sh): its frames are the held-out test set.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
need_data_dir

CONFIG="$SAR_ROOT/configs/dataset/v1.toml"
QUICK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) CONFIG="${2:-}"; shift 2 ;;
    --quick) QUICK=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--config FILE] [--quick]" ;;
  esac
done
CONFIG="$(resolve_path "$CONFIG")"
[[ -f "$CONFIG" ]] || die E-CFG-001 "config $CONFIG not found"
TOOL="$(tool_bin scenario-tool)"
NAME="$(awk -F'"' '/^\[dataset\]/{s=1;next} /^\[/{s=0} s && /^[[:space:]]*name[[:space:]]*=/{print $2; exit}' "$CONFIG")"
[[ "$NAME" =~ ^[A-Za-z0-9._-]+$ ]] || die E-CFG-003 "cannot read [dataset] name from $CONFIG"
ARGS=()
if [[ $QUICK -eq 1 ]]; then
  NAME="$NAME-quick"
  ARGS=(--limit 2 --frames 30)
fi
OUT="$SAR_ROOT/data/datasets/$NAME"
TEST="$SCENARIO_ROOT/antakya-demo"
info "$("$TOOL" version)"

step "Checking the held-out scenario"
[[ -f "$TEST/scenario.json" ]] || die E-SCN-001 "$TEST is not installed" "run scripts/zbook/60-scenario.sh first"
"$TOOL" verify --scenario "$TEST" | tail -n 1

step "Free disk space"
mkdir -p "$SAR_ROOT/data/datasets"
FREE_GB=$(df -P -BG "$SAR_ROOT/data" | awk 'NR==2 {gsub("G","",$4); print $4}')
[[ "${FREE_GB:-0}" -ge 5 ]] || die E-RND-001 "only ${FREE_GB} GB free under $SAR_ROOT/data" "the dataset needs about 2 GB plus room for training runs; free some space"
ok "${FREE_GB} GB free"

step "Building dataset $NAME (full set: about 15-25 minutes on the ZBook)"
"$TOOL" dataset --config "$CONFIG" --out "$OUT" --test-scenario "$TEST" "${ARGS[@]}"

step "Result"
du -sh "$OUT" | sed 's/^/    /'
echo "    Labelled examples: $OUT/preview.png"
finish "scripts/zbook/81-train.sh$([[ $QUICK -eq 1 ]] && echo " --quick --dataset $OUT")"
