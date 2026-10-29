#!/usr/bin/env bash
# Collects everything needed to diagnose a problem into one text file and prints
# a compact version between COPY markers. Safe to run at any time, on either machine.
#   scripts/collect-diagnostics.sh
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUTDIR="$ROOT/reports"
mkdir -p "$OUTDIR"
OUT="$OUTDIR/diagnostics-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).txt"
ROLE=zbook; [[ -f /etc/nv_tegra_release ]] && ROLE=jetson

section() { printf '\n##### %s\n' "$*"; }
run() { printf '$ %s\n' "$*"; timeout 20 "$@" 2>&1 | head -n 200; }

{
  section "versions"
  echo "rubblesar: $(cat "$ROOT/VERSION" 2>/dev/null) installed: $(cat /opt/rubblesar/VERSION 2>/dev/null || echo none)"
  run uname -a
  grep PRETTY_NAME /etc/os-release
  [[ -f /etc/nv_tegra_release ]] && head -n1 /etc/nv_tegra_release
  run go version
  run pkg-config --modversion zenohc
  for b in /opt/rubblesar/bin/*; do [[ -x $b ]] && run "$b" version; done
  section "sar-doctor"
  BIN=/opt/rubblesar/bin/sar-doctor; [[ -x $BIN ]] || BIN="$ROOT/bin/sar-doctor"
  [[ -x $BIN ]] && run "$BIN" --role "$ROLE" --report-dir "$OUTDIR"
  section "network"
  run hostname -I
  run ip -brief address
  run ip route
  section "time"
  run timedatectl
  run chronyc tracking
  run chronyc sources
  if [[ $ROLE == jetson ]]; then
    section "edge-eye"
    run systemctl --no-pager status edge-eye
    run journalctl -u edge-eye -n 80 --no-pager
    run curl -sS http://127.0.0.1:9101/v1/status
    section "config"
    run cat /etc/rubblesar/edge-eye.env
    run /opt/rubblesar/bin/edge-eye check-config
  else
    section "zenohd"
    run systemctl --no-pager status zenohd
    run journalctl -u zenohd -n 60 --no-pager
    run curl -sS "http://127.0.0.1:8447/@/*/router"
    section "services"
    for svc in casualty-analyst mission-planner rescue-dispatch rubblesar-taky sar-web; do
      run systemctl --no-pager status "$svc"
      run journalctl -u "$svc" -n 40 --no-pager
    done
    [[ -x /opt/rubblesar/bin/rescue-dispatch ]] && run /opt/rubblesar/bin/rescue-dispatch status
    [[ -x /opt/rubblesar/bin/rubblesar ]] && run /opt/rubblesar/bin/rubblesar status
    [[ -x /opt/rubblesar/bin/sar-web ]] && run /opt/rubblesar/bin/sar-web status
    command -v docker >/dev/null && run sudo -n docker logs --tail 40 rubblesar-taky
    section "config"
    run cat /etc/rubblesar/sar.toml
  fi
  section "latest reports"
  ls -t "$OUTDIR"/*.json 2>/dev/null | head -n 5
  section "latest setup logs (tails)"
  for f in $(ls -t "$OUTDIR"/setup/*.log 2>/dev/null | head -n 3); do echo "--- $f"; tail -n 30 "$f"; done
} >"$OUT" 2>&1

echo
echo "=============== COPY FROM HERE: RubbleSAR diagnostics ==============="
grep -E '^(#####|rubblesar:|PRETTY|# R36|\$ (go|pkg-config)|go version|[0-9]\.[0-9]+\.[0-9]+$|  \[(FAIL|WARN)\]|         (code|hint|- )|Active:|.*(ERROR|WARN|E-[A-Z]+-[0-9]+).*)' "$OUT" | head -n 150
echo "================ COPY TO HERE ======================================="
echo "Full file (attach if asked): $OUT"
