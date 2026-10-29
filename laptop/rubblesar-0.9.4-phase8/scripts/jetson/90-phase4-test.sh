#!/usr/bin/env bash
# Phase 4 - Jetson: test the onboard pipeline (as in phase 3) and the movement
# detector used while hovering over a casualty, then show what edge-eye is doing.
#
#   scripts/jetson/90-phase4-test.sh [--frames N]     (default 20 frames at each altitude)
#
# Checks (edge-eye selftest): ST-01 scenario and model load, ST-02 pipeline
# speed, ST-03 reports land near the true body parts, ST-04 store-and-forward
# through a slow link with an outage, ST-05 the queue survives a restart,
# ST-06 a 30 s hover over a moving victim and over a still one: the model's
# outlines tell them apart with the [perception] movement_* settings.
# The full acceptance run is on the ZBook: scripts/zbook/90-phase4-test.sh.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
source "$SAR_ROOT/scripts/lib/gpu.sh"

FRAMES=20
while [[ $# -gt 0 ]]; do
  case "$1" in
    --frames) FRAMES="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--frames N]" ;;
  esac
done
[[ "$FRAMES" =~ ^[0-9]+$ ]] || die E-SETUP-ARG "--frames takes a number"
EYE="$(tool_bin edge-eye)"
CFG=/etc/rubblesar/edge-eye.toml
[[ -f "$CFG" ]] || CFG="$SAR_ROOT/configs/jetson/edge-eye.toml"
info "$("$EYE" version)"

step "Phase 4 configuration ($CFG)"
"$EYE" check-config --config "$CFG" | sed 's/^/    /'
if ! grep -q '^behaviour *= *"standby"' "$CFG"; then
  warn "no drone in $CFG waits for tasks (behaviour = \"standby\"): install the phase 4 config with scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config"
fi
if ! grep -q '^movement_change' "$CFG"; then
  warn "$CFG has no movement_* settings (phase 3 config); the defaults apply. Install the phase 4 config with --force-config"
fi

need_sudo
step "GPU as the edge-eye service uses it (account rubblesar)"
if ! gpu_check_service "$CFG"; then
  die E-SETUP-GPUACC "the edge-eye service account 'rubblesar' cannot use the GPU" "run scripts/jetson/30-install.sh --zbook-ip <ZBook IP> (it adds the account to the GPU groups); paste this block if it still fails"
fi
step "Perception and hover self-test (about 4-5 minutes)"
if ! "$EYE" selftest --config "$CFG" --frames "$FRAMES" --report-dir "$SAR_ROOT/reports"; then
  die E-TEST-PHASE4 "the perception self-test failed on the Jetson" "each failure above has its own code and hint; paste the block"
fi

step "edge-eye service"
if "$EYE" status >/dev/null 2>&1; then
  "$EYE" status | sed 's/^/    /'
else
  warn "edge-eye is not running (sudo systemctl status edge-eye)"
fi
echo
echo "    The Jetson side works. Now run the acceptance run on the ZBook: scripts/zbook/90-phase4-test.sh"
finish "scripts/zbook/90-phase4-test.sh on the ZBook"
