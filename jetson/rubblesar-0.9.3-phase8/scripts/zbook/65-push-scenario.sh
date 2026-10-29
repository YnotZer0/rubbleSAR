#!/usr/bin/env bash
# Phase 1 - ZBook (optional): copy the installed scenario to the Jetson over SSH,
# so both machines use byte-identical layers. The alternative is to let the
# Jetson render its own copy (scripts/jetson/60-scenario.sh), which gives the
# same scenario.
#
#   scripts/zbook/65-push-scenario.sh --jetson USER@JETSON-IP [--id antakya-demo]
#
# Needs: ssh access to the Jetson (password or key) and sudo there. About 60 MB.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

TARGET=""; ID="antakya-demo"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --jetson) TARGET="${2:-}"; shift 2 ;;
    --id) ID="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 --jetson USER@JETSON-IP [--id ID]" ;;
  esac
done
[[ "$TARGET" == *@* ]] || die E-SETUP-ARG "--jetson USER@ADDRESS is required" "e.g. --jetson nvidia@192.168.1.60"
SRC="$SCENARIO_ROOT/$ID"
[[ -f "$SRC/scenario.json" ]] || die E-SCN-001 "$SRC is not installed" "run scripts/zbook/60-scenario.sh first"
need_cmd ssh openssh-client
need_cmd scp openssh-client

step "Checking SSH access to $TARGET"
ssh -o ConnectTimeout=10 "$TARGET" true || die E-SETUP-NET "cannot ssh to $TARGET" "check the address and user; on the Jetson: sudo systemctl status ssh"
ok "ssh works"
REMOTE_RSYNC=0
ssh "$TARGET" 'command -v rsync >/dev/null' && command -v rsync >/dev/null && REMOTE_RSYNC=1

step "Copying $SRC to $TARGET:rubblesar-data/scenarios/$ID"
ssh "$TARGET" "mkdir -p rubblesar-data/scenarios"
if [[ $REMOTE_RSYNC -eq 1 ]]; then
  rsync -a --delete --info=stats1 "$SRC/" "$TARGET:rubblesar-data/scenarios/$ID/"
else
  ssh "$TARGET" "rm -rf rubblesar-data/scenarios/$ID"
  scp -q -r "$SRC" "$TARGET:rubblesar-data/scenarios/$ID"
fi
ok "copied ($(du -sh "$SRC" | cut -f1))"

echo
echo "    Now on the Jetson run:"
echo "      scripts/jetson/60-scenario.sh --from ~/rubblesar-data/scenarios/$ID"
finish "the command above on the Jetson, then scripts/jetson/70-phase1-test.sh"
