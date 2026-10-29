#!/usr/bin/env bash
# Phase 5 - the TAK server: taky in Docker on this ZBook, plain TCP 8087, as
# the systemd service rubblesar-taky. ATAK (Android), WinTAK (Windows) and
# rescue-dispatch connect to it. Ends with a routing test (sar-check tak).
#   scripts/zbook/34-taky.sh [--rebuild]
#     --rebuild   build the image again (needs internet: Docker Hub and PyPI)
source "$(dirname "$0")/../lib/common.sh"

REBUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --rebuild) REBUILD=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--rebuild]" ;;
  esac
done

IMAGE="rubblesar-taky:${TAKY_VERSION}"
need_cmd docker docker.io
need_sudo

step "Docker"
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  sudo docker info >/dev/null 2>&1 || die E-SETUP-DOCKER "docker is not running" "sudo systemctl enable --now docker"
  DOCKER=(sudo docker)
fi
ok "$("${DOCKER[@]}" version --format 'docker {{.Server.Version}}')"
DOCKER_BIN="$(command -v docker)"

step "Port 8087 (TAK, plain TCP)"
owner="$(sudo ss -ltnpH 'sport = :8087' 2>/dev/null | head -n1 || true)"
if [[ -n "$owner" ]] && ! "${DOCKER[@]}" ps --format '{{.Names}}' | grep -qx rubblesar-taky; then
  die E-SETUP-PORT "TCP port 8087 is already in use: $owner" "stop that program (another TAK server?) and run this script again"
fi
ok "free (or already ours)"

step "Image $IMAGE (taky $TAKY_VERSION on $TAKY_BASE_IMAGE)"
if [[ $REBUILD -eq 0 ]] && "${DOCKER[@]}" image inspect "$IMAGE" >/dev/null 2>&1; then
  ok "already built (use --rebuild to build again)"
else
  "${DOCKER[@]}" build --pull -t "$IMAGE" --build-arg "TAKY_VERSION=$TAKY_VERSION" --build-arg "TAKY_BASE_IMAGE=$TAKY_BASE_IMAGE" "$SAR_ROOT/deploy/zbook/taky" \
    || die E-SETUP-TAKY "building the taky image failed" "needs internet (Docker Hub, PyPI); check 'docker system df' for disk space"
  ok "built"
fi
"${DOCKER[@]}" run --rm "$IMAGE" python -c "import importlib.metadata as m; print('taky', m.version('taky'))" | sed 's/^/    /' \
  || die E-SETUP-TAKY "the taky image does not start" "run $0 --rebuild; 'docker image ls rubblesar-taky'"

step "systemd service rubblesar-taky"
sed -e "s#@DOCKER@#$DOCKER_BIN#g" -e "s#@IMAGE@#$IMAGE#g" "$SAR_ROOT/deploy/zbook/systemd/rubblesar-taky.service" \
  | sudo tee /etc/systemd/system/rubblesar-taky.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable rubblesar-taky >/dev/null 2>&1
sudo systemctl restart rubblesar-taky
up=0
for _ in $(seq 1 45); do
  if timeout 1 bash -c '</dev/tcp/127.0.0.1/8087' 2>/dev/null; then up=1; break; fi
  sleep 1
done
if [[ $up -ne 1 ]]; then
  systemctl --no-pager status rubblesar-taky | sed 's/^/    /' || true
  echo "    ---- docker logs rubblesar-taky (last 30) ----"
  "${DOCKER[@]}" logs --tail 30 rubblesar-taky 2>&1 | sed 's/^/    /' || true
  die E-SETUP-TAKY "taky did not open TCP 8087 within 45 s" "see the log above; 'docker logs rubblesar-taky'"
fi
ok "taky answers on TCP 8087"

step "Firewall"
if command -v ufw >/dev/null 2>&1 && sudo ufw status | grep -q "Status: active"; then
  sudo ufw allow 8087/tcp comment 'RubbleSAR TAK server (taky)' >/dev/null
  ok "ufw allows 8087/tcp"
else
  ok "ufw not active (nothing to open)"
fi

step "Routing test (three test clients through taky)"
CHECK=/opt/rubblesar/bin/sar-check
[[ -x "$CHECK" ]] || CHECK="$SAR_ROOT/bin/sar-check"
"$CHECK" tak --server 127.0.0.1:8087 --report-dir "$SAR_ROOT/reports" \
  || die E-SETUP-TAKY "taky runs but does not pass messages between clients" "paste the block above and 'docker logs rubblesar-taky'"

if systemctl is-active --quiet rescue-dispatch 2>/dev/null; then
  step "rescue-dispatch"
  sudo systemctl restart rescue-dispatch
  sleep 3
  ok "restarted (it connects to taky on its own)"
fi

step "Connecting ATAK and WinTAK"
IPS="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^(10|172|192)\.' | tr '\n' ' ')"
info "TAK server address for the devices: ${IPS:-<the ZBook IP address>} port 8087, protocol TCP, no SSL"
info "ATAK:   Settings > Network Preferences > TAK Servers > Add (use SSL: off)"
info "WinTAK: Settings > Network Preferences > Manage Server Connections > Add (TCP)"
info "details: docs/PHASE5.md"

finish "scripts/zbook/90-phase5-test.sh (docs/PHASE5.md)"
