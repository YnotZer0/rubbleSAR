#!/usr/bin/env bash
# Phase 0 acceptance: with edge-eye running on the Jetson, prove that the ZBook
# receives every drone's telemetry at 1 Hz over Wi-Fi, can command each drone
# and can throttle a drone's link. Passing this closes phase 0.
source "$(dirname "$0")/../lib/common.sh"

BIN=/opt/rubblesar/bin
[[ -x $BIN/sar-check ]] || BIN="$SAR_ROOT/bin"
REPORTS="$SAR_ROOT/reports"

step "zenohd sessions (the Jetson should appear as a client)"
sessions_json="$(curl -fsS "http://127.0.0.1:8447/@/*/router" 2>/dev/null || true)"
if ! printf '%s' "$sessions_json" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
for r in data:
    for s in r.get("value", {}).get("sessions", []):
        links = ", ".join(l.get("dst", "?") for l in s.get("links", []))
        print("    %-7s %s" % (s.get("whatami", "?"), links))
'; then
  warn "zenohd admin API on 127.0.0.1:8447 did not answer with session data; skipping the session list (optional)"
fi

step "Live telemetry snapshot (10 s)"
"$BIN/sar-monitor" --once 10s || die E-CHK-002 "no telemetry from the Jetson" \
  "on the Jetson: systemctl status edge-eye; journalctl -u edge-eye -n 50; scripts/jetson/40-test.sh"

step "Phase 0 acceptance test (about 70 s)"
if "$BIN/sar-check" phase0 --report-dir "$REPORTS"; then
  ok "PHASE 0 ACCEPTED"
else
  die E-TEST-ACCEPT "phase 0 acceptance test failed" "paste the COPY FROM HERE block printed by sar-check above"
fi
finish "send the RESULT line above to move on to phase 1"
