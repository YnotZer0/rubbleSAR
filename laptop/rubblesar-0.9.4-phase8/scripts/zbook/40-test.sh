#!/usr/bin/env bash
# Phase 0 - ZBook tests that need no Jetson: environment, zenoh library,
# routing through zenohd, and the full phase 0 suite against simulated drones.
source "$(dirname "$0")/../lib/common.sh"

BIN=/opt/rubblesar/bin
[[ -x $BIN/sar-check ]] || BIN="$SAR_ROOT/bin"
REPORTS="$SAR_ROOT/reports"
FAILED=()

run_test() {
  local name="$1"; shift 2
  step "$name"
  if "$@"; then ok "$name"; else FAILED+=("$name"); warn "$name FAILED (details above)"; fi
}

run_test "Environment (sar-doctor)" -- "$BIN/sar-doctor" --role zbook --report-dir "$REPORTS"
run_test "zenoh library loopback" -- "$BIN/sar-check" loopback --report-dir "$REPORTS"
run_test "Messages through zenohd" -- "$BIN/sar-check" router --report-dir "$REPORTS"
run_test "Phase 0 suite with simulated drones (about 60 s)" -- "$BIN/sar-check" phase0 --self-test --report-dir "$REPORTS"

if [[ ${#FAILED[@]} -gt 0 ]]; then
  die E-TEST-ZBOOK "${#FAILED[@]} ZBook test(s) failed: ${FAILED[*]}" "each failure above has its own code and hint; reports are in $REPORTS"
fi
finish "set up the Jetson (scripts/jetson/00..40), then run scripts/zbook/50-acceptance.sh"
