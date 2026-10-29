#!/usr/bin/env bash
# Phase 1 - Jetson: put the simulated scenario in place for edge-eye.
#
#   scripts/jetson/60-scenario.sh                 generate and render it here (about 1-2 min)
#   scripts/jetson/60-scenario.sh --from DIR      install a folder copied from the ZBook
#                                                 (scripts/zbook/65-push-scenario.sh or a USB stick)
#   [--config FILE]   generator config for rendering here (default configs/scenario/antakya-demo.toml)
#   [--force]         replace an existing, different local scenario
#
# Result: /var/lib/rubblesar/scenarios/<id>/
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

CONFIG="$SAR_ROOT/configs/scenario/antakya-demo.toml"
FROM=""; FORCE=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) FROM="${2:-}"; shift 2 ;;
    --config) CONFIG="${2:-}"; shift 2 ;;
    --force) FORCE=(--force); shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--from DIR] [--config FILE] [--force]" ;;
  esac
done
TOOL="$(tool_bin scenario-tool)"
info "$("$TOOL" version)"
need_sudo   # asked now, so the script can run unattended afterwards

if [[ -n "$FROM" ]]; then
  FROM="${FROM%/}"
  step "Checking the copied scenario in $FROM"
  "$TOOL" verify --scenario "$FROM"
  ID="$(basename "$FROM")"
  SRC="$FROM"
else
  [[ -f "$CONFIG" ]] || die E-CFG-001 "config $CONFIG not found"
  ID="$(scenario_id "$CONFIG")"
  SRC="$SAR_ROOT/data/scenarios/$ID"
  mkdir -p "$SAR_ROOT/data/scenarios"
  step "Generating the scenario"
  "$TOOL" generate --config "$CONFIG" --out "$SRC" "${FORCE[@]}"
  step "Rendering imagery layers on the Jetson (about 1-2 min; MAXN power mode is faster)"
  "$TOOL" render --scenario "$SRC"
  step "Verifying layers"
  "$TOOL" verify --scenario "$SRC"
fi

step "Installing into $SCENARIO_ROOT/$ID"
install_scenario "$SRC" "$ID"
HASH="$(scenario_hash "$SCENARIO_ROOT/$ID")"
ok "scenario $ID, hash $HASH"

CFG=/etc/rubblesar/edge-eye.toml
if [[ -f $CFG ]] && grep -q '^scenario_dir' "$CFG" && ! grep -q "^scenario_dir = \"$SCENARIO_ROOT/$ID\"" "$CFG"; then
  warn "$CFG sets a different scenario_dir: $(grep '^scenario_dir' "$CFG")"
fi

echo
echo "    Scenario hash: $HASH   (compare with the value printed by scripts/zbook/60-scenario.sh;"
echo "    if they differ, copy the ZBook's folder with scripts/zbook/65-push-scenario.sh)"
finish "scripts/jetson/70-phase1-test.sh"
