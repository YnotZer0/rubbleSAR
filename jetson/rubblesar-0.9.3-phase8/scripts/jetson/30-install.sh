#!/usr/bin/env bash
# Phase 0 - Jetson step 4 of 4: install edge-eye as a systemd service pointed at the ZBook.
#
#   scripts/jetson/30-install.sh --zbook-ip 192.168.1.50 [--no-start] [--force-config]
#
# --force-config  overwrite an existing /etc/rubblesar/edge-eye.toml (a backup is kept)
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/gpu.sh"

ZBOOK_IP=""
START=1
FORCE_CONFIG=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --zbook-ip) ZBOOK_IP="${2:-}"; shift 2 ;;
    --no-start) START=0; shift ;;
    --force-config) FORCE_CONFIG=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 --zbook-ip <address> [--no-start] [--force-config]" ;;
  esac
done
[[ -n "$ZBOOK_IP" ]] || die E-SETUP-ARG "--zbook-ip is required" "find it on the ZBook with: hostname -I"
[[ "$ZBOOK_IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die E-SETUP-ARG "'$ZBOOK_IP' is not an IPv4 address"

for b in edge-eye sar-check sar-doctor scenario-tool sar-infer; do
  [[ -x "$SAR_ROOT/bin/$b" ]] || die E-SETUP-BIN "bin/$b missing" "run scripts/jetson/20-build.sh first"
done
need_sudo

step "Service account 'rubblesar'"
if id rubblesar >/dev/null 2>&1; then
  ok "exists"
else
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups video rubblesar
  ok "created (member of video for the cameras)"
fi

step "Installing binaries to /opt/rubblesar/bin"
sudo install -d -m 0755 /opt/rubblesar/bin /etc/rubblesar /var/lib/rubblesar
sudo chown rubblesar:rubblesar /var/lib/rubblesar
sudo install -d -o rubblesar -g rubblesar -m 0755 /var/lib/rubblesar/edge /var/lib/rubblesar/edge/queue   # report queue (phase 3+)
for b in edge-eye sar-check sar-doctor scenario-tool sar-infer; do
  sudo install -m 0755 "$SAR_ROOT/bin/$b" "/opt/rubblesar/bin/$b"
  sudo ln -sf "/opt/rubblesar/bin/$b" "/usr/local/bin/$b"
done
echo "$SAR_VERSION" | sudo tee /opt/rubblesar/VERSION >/dev/null
ok "$(/opt/rubblesar/bin/edge-eye version)"

step "Configuration in /etc/rubblesar"
CFG=/etc/rubblesar/edge-eye.toml
if [[ -f "$CFG" && $FORCE_CONFIG -eq 0 ]]; then
  if ! sudo cmp -s "$SAR_ROOT/configs/jetson/edge-eye.toml" "$CFG"; then
    sudo install -m 0644 "$SAR_ROOT/configs/jetson/edge-eye.toml" "$CFG.new"
    warn "kept your existing $CFG; the release default is in $CFG.new (use --force-config to replace)"
    if ! sudo grep -q '^\[perception\]' "$CFG"; then
      warn "your $CFG has no [perception] section: the drones will fly but not look for anyone. Re-run with --force-config (your file is backed up; the ZBook address lives in edge-eye.env)"
    elif ! sudo grep -q '^behaviour *= *"standby"' "$CFG"; then
      warn "your $CFG is a phase 3 config (no investigator waits for tasks). For phase 4 re-run with --force-config (your file is backed up; the ZBook address lives in edge-eye.env)"
    fi
  else
    ok "$CFG unchanged"
  fi
else
  [[ -f "$CFG" ]] && sudo cp "$CFG" "$CFG.bak.$(date +%s)"
  sudo install -m 0644 "$SAR_ROOT/configs/jetson/edge-eye.toml" "$CFG"
  ok "installed $CFG"
fi
sudo tee /etc/rubblesar/edge-eye.env >/dev/null <<EOF
# Written by scripts/jetson/30-install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# The zenoh router on the ZBook (overrides [zenoh] connect in edge-eye.toml)
SAR_ZENOH_CONNECT=tcp/${ZBOOK_IP}:7447
# zenoh's own log level (error|warn|info|debug) and no colour codes in the journal
RUST_LOG=error
NO_COLOR=1
EOF
ok "SAR_ZENOH_CONNECT=tcp/${ZBOOK_IP}:7447 in /etc/rubblesar/edge-eye.env"
/opt/rubblesar/bin/edge-eye check-config --config "$CFG" | sed 's/^/    /'

step "GPU access for the service account (phase 3: the model runs inside edge-eye)"
gpu_add_service_groups
if ! gpu_check_service "$CFG"; then
  die E-SETUP-GPUACC "the edge-eye service account 'rubblesar' cannot use the GPU, so the drones would fly without looking" \
    "paste this block: the three tries above show whether it is the account's groups or the service sandbox"
fi

step "Time synchronisation from the ZBook (chrony)"
if [[ -d /etc/chrony/sources.d ]]; then
  echo "server ${ZBOOK_IP} iburst prefer" | sudo tee /etc/chrony/sources.d/rubblesar.sources >/dev/null
  sudo chronyc reload sources >/dev/null 2>&1 || sudo systemctl restart chrony
else
  if ! grep -q "rubblesar" /etc/chrony/chrony.conf; then
    printf '\n# rubblesar: use the ZBook as time source\nserver %s iburst prefer\n' "$ZBOOK_IP" | sudo tee -a /etc/chrony/chrony.conf >/dev/null
  fi
  sudo systemctl restart chrony
fi
ok "chrony will follow the ZBook ($ZBOOK_IP) once it serves time (scripts/zbook/30-install.sh)"

step "Reaching the ZBook's zenoh router"
if timeout 3 bash -c "</dev/tcp/${ZBOOK_IP}/7447" 2>/dev/null; then
  ok "TCP ${ZBOOK_IP}:7447 reachable"
else
  warn "cannot reach ${ZBOOK_IP}:7447 yet; edge-eye will keep retrying (check zenohd and the ZBook firewall)"
fi

step "systemd service edge-eye"
sudo install -m 0644 "$SAR_ROOT/deploy/jetson/systemd/edge-eye.service" /etc/systemd/system/edge-eye.service
sudo systemctl daemon-reload
if [[ $START -eq 1 ]]; then
  sudo systemctl enable edge-eye >/dev/null 2>&1
  SINCE="$(date '+%Y-%m-%d %H:%M:%S')"
  sudo systemctl restart edge-eye
  info "waiting for edge-eye to report healthy (connected to the ZBook)..."
  healthy=0
  for _ in $(seq 1 30); do
    if curl -fsS http://127.0.0.1:9101/healthz >/dev/null 2>&1; then healthy=1; break; fi
    sleep 1
  done
  if [[ $healthy -eq 1 ]]; then
    ok "edge-eye is running and connected"
    /opt/rubblesar/bin/edge-eye status | sed 's/^/    /'
  else
    systemctl --no-pager status edge-eye 2>&1 | head -n 6 | sed 's/^/    /' || true
    curl -sS http://127.0.0.1:9101/healthz 2>&1 | sed 's/^/    healthz: /' || true
    # This start's log without the CPU register dump of a crash.
    JLOG="$(sudo journalctl -u edge-eye --since "$SINCE" --no-pager -o cat 2>/dev/null | grep -vE '^(r[0-9]+|lr|sp|pc|fault|pstate)[[:space:]]+0x' || true)"
    CRASH='SIGSEGV|SIGABRT|SIGBUS|SIGILL|fatal error|panic:|signal arrived during cgo'
    if grep -qE "$CRASH" <<<"$JLOG"; then
      echo "    ---- journalctl -u edge-eye: the start of the crash ----"
      grep -m1 -B6 -A24 -E "$CRASH" <<<"$JLOG" | sed 's/^/    /' || true
      die E-SETUP-SVC "edge-eye crashed while starting (this is not a network problem)" "paste this block"
    fi
    echo "    ---- journalctl -u edge-eye since the restart ----"
    head -n 25 <<<"$JLOG" | sed 's/^/    /' || true
    if grep -q 'E-BUS-010' <<<"$JLOG"; then
      die E-SETUP-SVC "edge-eye cannot reach the zenoh router at ${ZBOOK_IP}:7447" "is zenohd running on the ZBook (systemctl status zenohd)? is ${ZBOOK_IP} its address (hostname -I)? does its firewall allow TCP 7447?"
    fi
    die E-SETUP-SVC "edge-eye did not become healthy within 30 s" "see the log above; paste this block"
  fi
else
  ok "installed but not started (--no-start); start with: sudo systemctl enable --now edge-eye"
fi

finish "scripts/jetson/90-phase4-test.sh (phase 4), or scripts/jetson/40-test.sh (phase 0 checks)"
