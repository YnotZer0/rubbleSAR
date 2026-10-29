#!/usr/bin/env bash
# Install the ZBook programs (phase 0+), serve time to the Jetson, run
# casualty-analyst, which stores and acknowledges the drones' detections and
# (phase 4) merges them into triaged casualties, (phase 4) mission-planner,
# which sends the investigators to casualties that need a close look, and
# (phase 5) rescue-dispatch, which sends the rescue teams and feeds the TAK map,
# and (phase 6) sar-web, which serves the offline map and the after-action
# reports ((phase 7) the live operator view, (phase 8) the demo presenter),
# and the rubblesar command (up, down, status, mission, report, demo).
#   scripts/zbook/30-install.sh [--force-config]
source "$(dirname "$0")/../lib/common.sh"

FORCE_CONFIG=0
[[ "${1:-}" == "--force-config" ]] && FORCE_CONFIG=1

BINS="sar-check sar-monitor sar-doctor scenario-tool sar-infer casualty-analyst mission-planner rescue-dispatch sar-web rubblesar"
for b in $BINS; do
  [[ -x "$SAR_ROOT/bin/$b" ]] || die E-SETUP-BIN "bin/$b missing" "run scripts/zbook/20-build.sh first"
done
need_sudo

step "Installing binaries to /opt/rubblesar/bin"
sudo install -d -m 0755 /opt/rubblesar/bin /etc/rubblesar
for b in $BINS; do
  sudo install -m 0755 "$SAR_ROOT/bin/$b" "/opt/rubblesar/bin/$b"
  sudo ln -sf "/opt/rubblesar/bin/$b" "/usr/local/bin/$b"
done
echo "$SAR_VERSION" | sudo tee /opt/rubblesar/VERSION >/dev/null
ok "$(/opt/rubblesar/bin/sar-check version)"

step "Configuration /etc/rubblesar/sar.toml"
CFG=/etc/rubblesar/sar.toml
if [[ -f "$CFG" && $FORCE_CONFIG -eq 0 ]]; then
  if ! cmp -s "$SAR_ROOT/configs/zbook/sar.toml" "$CFG"; then
    sudo install -m 0644 "$SAR_ROOT/configs/zbook/sar.toml" "$CFG.new"
    warn "kept your existing $CFG; release default saved as $CFG.new (use --force-config to replace)"
  else
    ok "unchanged"
  fi
else
  [[ -f "$CFG" ]] && sudo cp "$CFG" "$CFG.bak.$(date +%s)"
  sudo install -m 0644 "$SAR_ROOT/configs/zbook/sar.toml" "$CFG"
  ok "installed"
fi

step "Serving time to the Jetson (chrony)"
sudo install -d /etc/chrony/conf.d
sudo tee /etc/chrony/conf.d/rubblesar.conf >/dev/null <<'EOF'
# RubbleSAR: let the Jetson synchronise to this ZBook, even with no internet.
allow 10.0.0.0/8
allow 172.16.0.0/12
allow 192.168.0.0/16
local stratum 10
EOF
if ! grep -qE '^\s*confdir\s+/etc/chrony/conf.d' /etc/chrony/chrony.conf; then
  echo "confdir /etc/chrony/conf.d" | sudo tee -a /etc/chrony/chrony.conf >/dev/null
fi
sudo systemctl restart chrony
ok "chrony serves time on UDP 123 to private networks"

# start_service NAME: install its unit, (re)start it and wait for 'NAME status'.
start_service() {
  local name=$1
  sudo install -m 0644 "$SAR_ROOT/deploy/zbook/systemd/$name.service" "/etc/systemd/system/$name.service"
  sudo systemctl daemon-reload
  sudo systemctl enable "$name" >/dev/null 2>&1
  sudo systemctl restart "$name"
  local healthy=0
  for _ in $(seq 1 20); do
    if "/opt/rubblesar/bin/$name" status >/dev/null 2>&1; then healthy=1; break; fi
    sleep 1
  done
  if [[ $healthy -eq 1 ]]; then
    ok "$name is running"
    "/opt/rubblesar/bin/$name" status | sed -n '1,25s/^/    /p'   # sed reads it all: head would cut the pipe (SIGPIPE)
  else
    systemctl --no-pager status "$name" | sed 's/^/    /' || true
    echo "    ---- journalctl -u $name (last 30) ----"
    sudo journalctl -u "$name" -n 30 --no-pager | sed 's/^/    /' || true
    die E-SETUP-SVC "$name did not start within 20 s" "is zenohd running? 'systemctl status zenohd'; see the log above"
  fi
}

if id rubblesar >/dev/null 2>&1; then
  ok "service account 'rubblesar' exists"
else
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin rubblesar
  ok "service account 'rubblesar' created"
fi
sudo install -d -m 0755 /var/lib/rubblesar
sudo install -d -m 0755 -o rubblesar -g rubblesar /var/lib/rubblesar/detections /var/lib/rubblesar/planner /var/lib/rubblesar/dispatch
sudo install -d -m 0755 /var/lib/rubblesar/roads /var/lib/rubblesar/map /var/lib/rubblesar/www /var/lib/rubblesar/www/map
# Reports are written by you (rubblesar mission, sar-check) and served by sar-web.
ME="${SUDO_USER:-$USER}"
sudo install -d -m 0755 -o "$ME" -g "$(id -gn "$ME")" /var/lib/rubblesar/www/reports
# An archive/ folder made by an earlier install as root: give it to the service account.
for d in /var/lib/rubblesar/detections/*/archive; do
  if [[ -d "$d" ]]; then sudo chown rubblesar:rubblesar "$d"; fi
done

step "casualty-analyst service (stores the detections, merges them into triaged casualties)"
/opt/rubblesar/bin/casualty-analyst check-config --config "$CFG" | sed 's/^/    /'
# Reports stored before phase 4 (no casualties-state.json yet) are set aside,
# so the casualty records start from this release's runs. Nothing is deleted.
STORE="$(sed -n 's/^store_dir *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
INCIDENT="$(sed -n 's/^incident_id *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
RUN_DIR="${STORE:-/var/lib/rubblesar/detections}/${INCIDENT:-antakya-demo}"
if sudo test -f "$RUN_DIR/detections.jsonl" && ! sudo test -f "$RUN_DIR/casualties-state.json"; then
  sudo systemctl stop casualty-analyst 2>/dev/null || true
  ARCH="$RUN_DIR/archive/$(date -u +%Y%m%dT%H%M%SZ)-before-phase4"
  # Both levels owned by the service account: it archives into archive/ itself later.
  sudo install -d -m 0755 -o rubblesar -g rubblesar "$RUN_DIR/archive" "$ARCH"
  sudo find "$RUN_DIR" -mindepth 1 -maxdepth 1 ! -name archive -exec mv -t "$ARCH" {} +
  ok "reports stored before phase 4 moved to $ARCH (kept; casualty records start afresh)"
fi
start_service casualty-analyst

step "mission-planner service (sends the investigators to casualties that need a close look)"
/opt/rubblesar/bin/mission-planner check-config --config "$CFG" | sed 's/^/    /'
start_service mission-planner

step "rescue-dispatch service (sends the rescue teams, feeds the TAK map)"
/opt/rubblesar/bin/rescue-dispatch check-config --config "$CFG" | sed 's/^/    /'
start_service rescue-dispatch
ROADS="/var/lib/rubblesar/roads/${INCIDENT:-antakya-demo}.json"
[[ -f "$ROADS" ]] || warn "no street file yet ($ROADS): run scripts/zbook/32-roads.sh (routes are straight lines until then)"
systemctl is-active --quiet rubblesar-taky 2>/dev/null || warn "the TAK server is not installed yet: run scripts/zbook/34-taky.sh"

step "sar-web service (the offline map, the live operator view, the after-action reports, port 8090)"
/opt/rubblesar/bin/sar-web check-config --config "$CFG" | sed 's/^/    /'
start_service sar-web
WEB_PORT="$(sed -n '/^\[web\]/,/^\[/s/^listen *= *"[^:]*:\([0-9]*\)"/\1/p' "$CFG" | head -n1)"
WEB_PORT="${WEB_PORT:-8090}"
if command -v ufw >/dev/null && sudo ufw status 2>/dev/null | grep -q "Status: active"; then
  sudo ufw allow "$WEB_PORT/tcp" comment "RubbleSAR sar-web" >/dev/null
  ok "firewall: $WEB_PORT/tcp open for the devices"
fi
[[ -f "/var/lib/rubblesar/www/map/${INCIDENT:-antakya-demo}/${INCIDENT:-antakya-demo}.mbtiles" ]] || warn "no offline map yet: run scripts/zbook/36-map.sh"

for ip in $(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -vE '^(127\.|172\.1[7-9]\.)' | head -n1); do
  info "the live operator view: http://$ip:$WEB_PORT/ops/ (any browser on the network)"
done
command -v obs >/dev/null || info "to record the demo video: scripts/zbook/92-demo-setup.sh (docs/PHASE8.md)"

finish "docs/PHASE7.md (90-phase7-test.sh), then docs/PHASE8.md for the demo video"
