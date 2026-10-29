#!/usr/bin/env bash
# Phase 0 - ZBook step 3 of 5: install and start the zenoh router (zenohd 1.9.0)
# natively as a systemd service, listening on TCP 7447 for the Jetson.
source "$(dirname "$0")/../lib/common.sh"

need_cmd unzip unzip
need_cmd curl curl
need_sudo

ARCH="$(arch_name)"
FILE="zenoh-${ZENOH_VERSION}-${ARCH}-unknown-linux-gnu-standalone.zip"
VAR="ZENOHD_SHA256_${ARCH^^}"
URL="https://github.com/eclipse-zenoh/zenoh/releases/download/${ZENOH_VERSION}/${FILE}"

step "Downloading zenohd $ZENOH_VERSION"
fetch "$URL" "$FILE" "${!VAR}"

step "Installing zenohd and its plugins"
TMP="$(mktemp -d)"
unzip -q -o "$DOWNLOAD_DIR/$FILE" -d "$TMP"
[[ -x "$TMP/zenohd" ]] || die E-SETUP-ZD "zenohd not found in $FILE"
if systemctl is-active --quiet zenohd 2>/dev/null; then
  sudo systemctl stop zenohd
fi
sudo install -m 0755 "$TMP/zenohd" /usr/local/bin/zenohd
sudo install -d -m 0755 /usr/local/lib/zenoh
sudo install -m 0644 "$TMP"/libzenoh_plugin_*.so /usr/local/lib/zenoh/
rm -rf "$TMP"
ok "$(/usr/local/bin/zenohd --version 2>&1 | tail -n1)"

step "Checking ports 7447 (zenoh) and 8447 (REST admin) are free"
port_owner() { sudo ss -ltnpH "sport = :$1" 2>/dev/null | head -n1; }
if [[ -n "$(port_owner 7447)" ]]; then
  die E-SETUP-PORT "TCP port 7447 is already in use: $(port_owner 7447)" \
    "stop that program (another zenohd or a Docker container?) and run this script again"
fi
ok "7447 free"
if [[ -n "$(port_owner 8447)" ]]; then
  warn "TCP port 8447 is in use ($(port_owner 8447)); zenohd will run but its optional REST admin API will not. Change http_port in /etc/rubblesar/zenohd.json5 to move it."
else
  ok "8447 free"
fi

step "Service account and configuration"
id zenohd >/dev/null 2>&1 || sudo useradd --system --no-create-home --shell /usr/sbin/nologin zenohd
sudo install -d -m 0755 /etc/rubblesar
sudo install -m 0644 "$SAR_ROOT/deploy/zbook/zenohd/zenohd.json5" /etc/rubblesar/zenohd.json5
ok "/etc/rubblesar/zenohd.json5"

step "systemd service zenohd"
sudo install -m 0644 "$SAR_ROOT/deploy/zbook/systemd/zenohd.service" /etc/systemd/system/zenohd.service
sudo systemctl daemon-reload
sudo systemctl enable zenohd >/dev/null 2>&1
sudo systemctl restart zenohd
for _ in $(seq 1 15); do
  timeout 1 bash -c '</dev/tcp/127.0.0.1/7447' 2>/dev/null && break
  sleep 1
done
if ! timeout 1 bash -c '</dev/tcp/127.0.0.1/7447' 2>/dev/null; then
  systemctl --no-pager status zenohd | sed 's/^/    /' || true
  sudo journalctl -u zenohd -n 30 --no-pager | sed 's/^/    /' || true
  die E-SETUP-ZD "zenohd is not listening on 7447" "see the journal lines above; another program may be using port 7447 (sudo ss -ltnp | grep 7447)"
fi
ok "listening on 0.0.0.0:7447"
if curl -fsS "http://127.0.0.1:8447/@/*/router" >/dev/null 2>&1; then
  ok "REST admin API on 127.0.0.1:8447"
else
  warn "REST admin API not answering on 127.0.0.1:8447 (optional, used by sar-doctor)"
fi

step "Firewall"
if command -v ufw >/dev/null && sudo ufw status | grep -q "Status: active"; then
  sudo ufw allow 7447/tcp comment 'RubbleSAR zenoh router' >/dev/null
  sudo ufw allow 123/udp comment 'RubbleSAR time for the Jetson' >/dev/null
  ok "ufw: allowed 7447/tcp (zenoh) and 123/udp (time)"
else
  ok "ufw not active; nothing to open"
fi

step "Addresses the Jetson can use"
for ip in $(hostname -I); do info "$ip"; done
info "pass the Wi-Fi address to scripts/jetson/30-install.sh --zbook-ip <address>"

finish "scripts/zbook/20-build.sh"
