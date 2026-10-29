#!/usr/bin/env bash
# Phase 6 - ZBook acceptance run of the whole application: both machines up
# ('rubblesar status'), the offline incident map drawn and served, then the
# phase 5 run (survey, casualty records, investigations, rescue teams, the TAK
# map) recorded for its after-action report, which is checked and served.
#
#   scripts/zbook/90-phase6-test.sh [--quick] [--max-minutes N]
#     --quick          no new survey: the casualties of the last run get teams
#                      again (about 5-15 minutes; needs a phase 4/5/6 run first)
#     --max-minutes N  stop the drones' run after N minutes (default 45)
#
# Needs: 30-install.sh (sar-web, rubblesar), 32-roads.sh, 34-taky.sh,
# 36-map.sh on this ZBook; edge-eye running on the Jetson ('rubblesar up'
# starts it when 38-jetson-link.sh has been run).
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
OPS="$(tool_bin rubblesar)"
DSP="$(tool_bin rescue-dispatch)"
info "$("$CHECK" version)"

step "Both machines (rubblesar status)"
if ! "$OPS" status | sed 's/^/    /'; then
  warn "something is down (above): 'rubblesar up' starts everything; the acceptance run checks it again (P6-01)"
fi

step "TAK server (taky in Docker)"
if ! systemctl is-active --quiet rubblesar-taky 2>/dev/null; then
  die E-SETUP-TAKY "the TAK server rubblesar-taky is not running" "rubblesar up, or install it with scripts/zbook/34-taky.sh"
fi
"$CHECK" tak --server 127.0.0.1:8087 --report-dir "$SAR_ROOT/reports" | sed 's/^/    /' \
  || die E-SETUP-TAKY "the TAK server does not pass messages between clients" "paste the block above; 'docker logs rubblesar-taky'"

if [[ $QUICK -eq 1 ]]; then
  step "Phase 6 acceptance run, quick (5-15 minutes)"
else
  step "Phase 6 acceptance run (30-50 minutes; keep both machines on)"
fi
info "watch it on WinTAK (with the offline map) or ATAK; the after-action report is written at the end"
if ! "$CHECK" phase6 "${ARGS[@]}"; then
  die E-TEST-PHASE6 "the phase 6 acceptance run failed" "each failure above has its own code and hint; paste the sar-check block"
fi

step "Teams and assignments at the end of the run"
"$DSP" status | sed 's/^/    /'
step "The after-action report"
"$OPS" report --list | sed -n '1s/^/    latest run: /p'
WEB_PORT="$(sed -n '/^\[web\]/,/^\[/s/^listen *= *"[^:]*:\([0-9]*\)"/\1/p' /etc/rubblesar/sar.toml 2>/dev/null | head -n1)"
for ip in $(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -vE '^(127\.|172\.1[7-9]\.)' | head -n2); do
  info "open http://$ip:${WEB_PORT:-8090}/ in a browser on the network: the map downloads and every report"
done
echo
echo "    PHASE 6 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "share the sar-check block; try 'rubblesar mission start' for a demo run with its own report"
