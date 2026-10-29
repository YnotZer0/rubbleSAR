#!/usr/bin/env bash
# Phase 3 - ZBook acceptance run: the Jetson's drones fly their survey missions
# with the perception pipeline while sar-check measures what arrives.
#
#   scripts/zbook/90-phase3-test.sh [--quick] [--max-minutes N]
#
# The drones restart their missions; after 90 s every drone's link goes silent
# for 60 s (simulated outage), after 180 s the budget is cut to 32 kbit/s for
# 90 s. The run ends when the missions are done and the queues are empty
# (about 10-15 minutes; --quick: 30 s outage and 60 s budget cut).
# Needs: edge-eye running on the Jetson with the phase 3 config and the
# model engine, casualty-analyst running here (scripts/zbook/30-install.sh).
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) ARGS+=(--quick); shift ;;
    --max-minutes) ARGS+=(--max-minutes "${2:-}"); shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--quick] [--max-minutes N]" ;;
  esac
done
CHECK="$(tool_bin sar-check)"
ANALYST="$(tool_bin casualty-analyst)"
info "$("$CHECK" version)"

step "casualty-analyst (stores and acknowledges the detections)"
if ! "$ANALYST" status >/dev/null 2>&1; then
  die E-SETUP-SVC "casualty-analyst is not running" "sudo systemctl start casualty-analyst (installed by scripts/zbook/30-install.sh); 'journalctl -u casualty-analyst -n 30'"
fi
"$ANALYST" status | sed 's/^/    /'

step "Phase 3 acceptance run (10-15 minutes; keep both machines on)"
if ! "$CHECK" phase3 "${ARGS[@]}"; then
  die E-TEST-PHASE3 "the phase 3 acceptance run failed" "each failure above has its own code and hint; paste the sar-check block"
fi

step "Stored detections"
"$ANALYST" status | sed 's/^/    /'
STORE="$(sed -n 's/^store_dir *= *"\(.*\)"/\1/p' /etc/rubblesar/sar.toml 2>/dev/null | head -n1)"
info "reports and crops: ${STORE:-/var/lib/rubblesar/detections}/<incident>/ (detections.jsonl, crops/)"
echo
echo "    PHASE 3 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "share the sar-check block from this run and the Jetson's 90-phase3-test.sh block"
