#!/usr/bin/env bash
# Phase 5 - ZBook acceptance run: the phase 4 run (survey, casualty records,
# investigations) with rescue-dispatch sending the rescue teams as casualties
# are triaged, checked on the TAK server: sar-check joins taky as the TAK
# team CHECK-1, follows its routes and says "done", and compares the map,
# the routes, the dispatch order and the chat with what was published.
#
#   scripts/zbook/90-phase5-test.sh [--quick] [--max-minutes N]
#     --quick          no new survey: the casualties of the last run get teams
#                      again (about 5-15 minutes; needs a phase 4 or 5 run first)
#     --max-minutes N  stop the drones' run after N minutes (default 45)
#
# Needs: edge-eye running on the Jetson with the phase 4 config (not for
# --quick), and here casualty-analyst, mission-planner, rescue-dispatch
# (30-install.sh), the street file (32-roads.sh) and taky (34-taky.sh).
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

ARGS=()
QUICK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) ARGS+=(--quick); QUICK=1; shift ;;
    --max-minutes) ARGS+=(--max-minutes "${2:-}"); shift 2 ;;
    --allow-oracle) ARGS+=(--allow-oracle); shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--quick] [--max-minutes N]" ;;
  esac
done
CHECK="$(tool_bin sar-check)"
ANALYST="$(tool_bin casualty-analyst)"
PLANNER="$(tool_bin mission-planner)"
DSP="$(tool_bin rescue-dispatch)"
info "$("$CHECK" version)"

step "TAK server (taky in Docker)"
if ! systemctl is-active --quiet rubblesar-taky 2>/dev/null; then
  die E-SETUP-TAKY "the TAK server rubblesar-taky is not running" "install it with scripts/zbook/34-taky.sh; 'sudo systemctl status rubblesar-taky'"
fi
"$CHECK" tak --server 127.0.0.1:8087 --report-dir "$SAR_ROOT/reports" | sed 's/^/    /' \
  || die E-SETUP-TAKY "the TAK server does not pass messages between clients" "paste the block above; 'docker logs rubblesar-taky'"

step "casualty-analyst and mission-planner"
"$ANALYST" status >/dev/null 2>&1 || die E-SETUP-SVC "casualty-analyst is not running" "sudo systemctl start casualty-analyst (scripts/zbook/30-install.sh)"
"$PLANNER" status >/dev/null 2>&1 || die E-SETUP-SVC "mission-planner is not running" "sudo systemctl start mission-planner (scripts/zbook/30-install.sh)"
ok "both running"

step "rescue-dispatch (teams, routes, TAK map)"
if ! "$DSP" status >/dev/null 2>&1; then
  die E-SETUP-SVC "rescue-dispatch is not running" "sudo systemctl start rescue-dispatch (installed by scripts/zbook/30-install.sh); 'journalctl -u rescue-dispatch -n 30'"
fi
"$DSP" status | sed -n '1,14s/^/    /p'

if [[ $QUICK -eq 1 ]]; then
  step "Phase 5 acceptance run, quick (5-15 minutes)"
else
  step "Phase 5 acceptance run (30-45 minutes; keep both machines on)"
fi
info "watch it on ATAK or WinTAK: casualty markers, drones, teams walking their routes, chat in All Chat Rooms"
if ! "$CHECK" phase5 "${ARGS[@]}"; then
  die E-TEST-PHASE5 "the phase 5 acceptance run failed" "each failure above has its own code and hint; paste the sar-check block"
fi

step "Teams and assignments at the end of the run"
"$DSP" status | sed 's/^/    /'
echo
echo "    PHASE 5 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "share the sar-check block from this run"
