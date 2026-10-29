#!/usr/bin/env bash
# Phase 8 - ZBook acceptance of the demo director: its settings and
# storyboard, a recorded run with the storyboard's beats, a drone's frame
# rendered again with the model on it, the narration filled in from the run,
# sar-web's replay and the presenter page, a short test recording on every
# recorder, the rough-cut tools; with --rehearse, the whole narrated take
# played on the presenter page (not recorded). No drones needed.
#
#   scripts/zbook/90-phase8-test.sh [--run RUN] [--rehearse]
#     --run RUN    the recorded run (default the latest; 'rubblesar report --list')
#     --rehearse   also play the whole storyboard (about 5 minutes; watch OBS's preview)
#
# Needs: 92-demo-setup.sh, OBS running with its WebSocket server on and the
# password in ~/.config/rubblesar/demo.toml, 'rubblesar demo obs-setup' done,
# and a recorded run ('rubblesar demo live', or the phase 6/7 run).
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"   # tool_bin

ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --run) ARGS+=(--run "${2:-}"); shift 2 ;;
    --rehearse) ARGS+=(--rehearse); shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--run RUN] [--rehearse]" ;;
  esac
done
CHECK="$(tool_bin sar-check)"
OPS="$(tool_bin rubblesar)"
info "$("$CHECK" version)"

[[ -f "$HOME/.config/rubblesar/demo.toml" ]] || die E-DIR-002 "no demo settings yet" "scripts/zbook/92-demo-setup.sh (or rubblesar demo init)"

step "What the takes need (rubblesar demo check)"
"$OPS" demo check | sed 's/^/    /' || warn "a FAIL above: the acceptance below shows it with its code and hint"

step "Phase 8 acceptance (a few minutes)"
if ! "$CHECK" phase8 "${ARGS[@]}"; then
  die E-TEST-PHASE8 "the phase 8 acceptance failed" "each failure above has its own code and hint; paste the sar-check block"
fi
echo
echo "    PHASE 8 checks finished. Paste the COPY FROM HERE ... COPY TO HERE block above."
finish "record the video: docs/PHASE8.md, 'Recording the video' (rubblesar demo live, media, script, replay, cut)"
