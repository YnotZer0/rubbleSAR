#!/usr/bin/env bash
# Phase 2 - ZBook: copy the current model folder to the Jetson over SSH.
#
#   scripts/zbook/85-push-model.sh --jetson USER@JETSON-IP [--name NAME]
#
# Then, on the Jetson: scripts/jetson/80-engine.sh --from ~/rubblesar-data/models/<name>
# (Or copy /var/lib/rubblesar/models/<name> on a USB stick and pass that folder to --from.)
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/models.sh"

TARGET=""; NAME=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --jetson) TARGET="${2:-}"; shift 2 ;;
    --name) NAME="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 --jetson USER@JETSON-IP [--name NAME]" ;;
  esac
done
[[ "$TARGET" == *@* ]] || die E-SETUP-ARG "--jetson USER@ADDRESS is required" "e.g. --jetson nvidia@192.168.1.60"
if [[ -z "$NAME" ]]; then
  [[ -L "$MODEL_ROOT/current" ]] || die E-SETUP-MODEL "no current model" "run scripts/zbook/82-evaluate.sh first"
  NAME="$(readlink "$MODEL_ROOT/current")"
fi
SRC="$MODEL_ROOT/$NAME"
for f in model.onnx model.json testset.json eval-zbook.json golden/golden.json; do
  [[ -f "$SRC/$f" ]] || die E-SETUP-MODEL "$SRC/$f missing" "run scripts/zbook/82-evaluate.sh first"
done
need_cmd ssh openssh-client
need_cmd scp openssh-client

step "Checking SSH access to $TARGET"
ssh -o ConnectTimeout=10 "$TARGET" true || die E-SETUP-NET "cannot ssh to $TARGET" "check the address and user; on the Jetson: sudo systemctl status ssh"
ok "ssh works"

step "Copying $SRC to $TARGET:rubblesar-data/models/$NAME"
ssh "$TARGET" "mkdir -p rubblesar-data/models && rm -rf rubblesar-data/models/$NAME"
if command -v rsync >/dev/null && ssh "$TARGET" 'command -v rsync >/dev/null'; then
  rsync -a --info=stats1 "$SRC/" "$TARGET:rubblesar-data/models/$NAME/"
else
  scp -q -r "$SRC" "$TARGET:rubblesar-data/models/$NAME"
fi
ok "copied ($(du -sh "$SRC" | cut -f1))"
echo
echo "    Now on the Jetson run:"
echo "      scripts/jetson/80-engine.sh --from ~/rubblesar-data/models/$NAME"
finish "the command above on the Jetson, then scripts/jetson/90-phase2-test.sh"
