#!/usr/bin/env bash
# Phase 0 - Jetson tests: environment, zenoh library, path to the ZBook router,
# and the running edge-eye service. Safe to run any time.
source "$(dirname "$0")/../lib/common.sh"

BIN=/opt/rubblesar/bin
[[ -x $BIN/sar-check ]] || BIN="$SAR_ROOT/bin"
REPORTS="$SAR_ROOT/reports"
FAILED=()

run_test() { # name -- command
  local name="$1"; shift 2
  step "$name"
  if "$@"; then ok "$name"; else FAILED+=("$name"); warn "$name FAILED (details above)"; fi
}

run_test "Environment (sar-doctor)" -- "$BIN/sar-doctor" --role jetson --report-dir "$REPORTS"
run_test "zenoh library loopback" -- "$BIN/sar-check" loopback --report-dir "$REPORTS"
run_test "Messages through the ZBook router" -- "$BIN/sar-check" router --config /etc/rubblesar/edge-eye.toml --report-dir "$REPORTS"
run_test "edge-eye service health" -- bash -c 'systemctl is-active --quiet edge-eye && curl -fsS http://127.0.0.1:9101/healthz >/dev/null && /opt/rubblesar/bin/edge-eye status'

if [[ ${#FAILED[@]} -gt 0 ]]; then
  die E-TEST-JETSON "${#FAILED[@]} Jetson test(s) failed: ${FAILED[*]}" "each failure above has its own code and hint; reports are in $REPORTS"
fi
finish "on the ZBook: scripts/zbook/50-acceptance.sh"
