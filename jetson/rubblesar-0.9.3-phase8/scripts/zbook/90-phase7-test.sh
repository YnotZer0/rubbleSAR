#!/usr/bin/env bash
# Phase 7 - ZBook acceptance run of the live operator view: the phase 6 run
# (both machines, the offline map, survey, casualty records, investigations,
# rescue teams, the TAK map, the after-action report), with sar-web's live
# picture (/ops/) compared with casualty-analyst, mission-planner and
# rescue-dispatch throughout, its event stream, the drones' crops and its
# timeline against the after-action report.
#
#   scripts/zbook/90-phase7-test.sh [--quick] [--max-minutes N]
#     --quick          no new survey: the casualties of the last run get teams
#                      again (about 5-15 minutes; needs a phase 4..7 run first)
#     --max-minutes N  stop the drones' run after N minutes (default 45)
#
# Needs: 30-install.sh of this release (sar-web 0.9 with the live view),
# 32-roads.sh, 34-taky.sh, 36-map.sh; edge-eye running on the Jetson
# ('rubblesar up' starts it when 38-jetson-link.sh has been run).
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"   # tool_bin

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
OPS="$(tool_bin rubblesar)"
WEB="$(tool_bin sar-web)"
info "$("$CHECK" version)"

WEB_PORT="$(sed -n '/^\[web\]/,/^\[/s/^listen *= *"[^:]*:\([0-9]*\)"/\1/p' /etc/rubblesar/sar.toml 2>/dev/null | head -n1)"
WEB_PORT="${WEB_PORT:-8090}"

step "sar-web with the live operator view"
"$WEB" status | sed 's/^/    /' || die E-WEB-001 "sar-web is not answering" "sudo systemctl status sar-web; scripts/zbook/30-install.sh installs this release's"
STATUS="$(curl -fsS --max-time 5 "http://127.0.0.1:$WEB_PORT/v1/status" 2>/dev/null || true)"
if ! grep -q '"live"' <<<"$STATUS"; then
  die E-WEB-001 "the running sar-web has no live view (an older release)" "scripts/zbook/20-build.sh, then scripts/zbook/30-install.sh"
fi
ok "live view: $(sed -n 's/.*"bus":"\([^"]*\)".*/\1/p' <<<"$STATUS" | head -n1)"

step "Both machines (rubblesar status)"
if ! "$OPS" status | sed 's/^/    /'; then
  warn "something is down (above): 'rubblesar up' starts everything; the acceptance run checks it again (P6-01)"
fi

if [[ $QUICK -eq 1 ]]; then
  step "Phase 7 acceptance run, quick (5-15 minutes)"
else
  step "Phase 7 acceptance run (30-50 minutes; keep both machines on)"
fi
for ip in $(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -vE '^(127\.|172\.1[7-9]\.)' | head -n2); do
  info "watch it live: http://$ip:$WEB_PORT/ops/ in any browser on the network (no TAK needed)"
done
if ! "$CHECK" phase7 "${ARGS[@]}"; then
  die E-TEST-PHASE7 "the phase 7 acceptance run failed" "each failure above has its own code and hint; paste the sar-check block"
fi
echo
echo "    PHASE 7 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "docs/PHASE8.md: set up the demo recording (scripts/zbook/92-demo-setup.sh)"
