#!/usr/bin/env bash
# Phase 4 - ZBook acceptance run: the scout surveys the sites again, casualty-
# analyst merges the reports into triaged casualty records, mission-planner
# sends the investigators to every casualty that needs a close look, and
# sar-check compares the result with the scenario's ground truth.
#
#   scripts/zbook/90-phase4-test.sh [--max-minutes N]     (default 45)
#
# The run first archives casualty-analyst's previous run (reports, crops and
# casualties move to <store_dir>/<incident>/archive/<time>-before-phase4/) and
# forgets mission-planner's tasks, so it is scored on its own. It ends when
# the survey is done, no task is open and the queues are empty (about 25-35
# minutes). Needs: edge-eye running on the Jetson with the phase 4 config,
# casualty-analyst and mission-planner running here (scripts/zbook/30-install.sh).
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max-minutes) ARGS+=(--max-minutes "${2:-}"); shift 2 ;;
    --allow-oracle) ARGS+=(--allow-oracle); shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--max-minutes N]" ;;
  esac
done
CHECK="$(tool_bin sar-check)"
ANALYST="$(tool_bin casualty-analyst)"
PLANNER="$(tool_bin mission-planner)"
info "$("$CHECK" version)"

step "casualty-analyst (stores the detections, keeps the casualty records)"
if ! "$ANALYST" status >/dev/null 2>&1; then
  die E-SETUP-SVC "casualty-analyst is not running" "sudo systemctl start casualty-analyst (installed by scripts/zbook/30-install.sh); 'journalctl -u casualty-analyst -n 30'"
fi
"$ANALYST" status | sed -n '1,12s/^/    /p'

step "mission-planner (sends the investigators)"
if ! "$PLANNER" status >/dev/null 2>&1; then
  die E-SETUP-SVC "mission-planner is not running" "sudo systemctl start mission-planner (installed by scripts/zbook/30-install.sh); 'journalctl -u mission-planner -n 30'"
fi
"$PLANNER" status | sed -n '1,8s/^/    /p'

step "Phase 4 acceptance run (25-35 minutes; keep both machines on)"
if ! "$CHECK" phase4 "${ARGS[@]}"; then
  die E-TEST-PHASE4 "the phase 4 acceptance run failed" "each failure above has its own code and hint; paste the sar-check block"
fi

step "Casualties and investigations at the end of the run"
"$ANALYST" status | sed 's/^/    /'
"$PLANNER" status | sed 's/^/    /'
STORE="$(sed -n 's/^store_dir *= *"\(.*\)"/\1/p' /etc/rubblesar/sar.toml 2>/dev/null | head -n1)"
info "this run: ${STORE:-/var/lib/rubblesar/detections}/<incident>/ (detections.jsonl, crops/, casualties-state.json); earlier runs in archive/"
echo
echo "    PHASE 4 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "share the sar-check block from this run and the Jetson's 90-phase4-test.sh block"
