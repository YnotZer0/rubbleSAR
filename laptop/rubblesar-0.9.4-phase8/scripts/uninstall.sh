#!/usr/bin/env bash
# Removes RubbleSAR services and binaries from this machine.
#   scripts/uninstall.sh            keep /etc/rubblesar (configs) and zenoh-c
#   scripts/uninstall.sh --purge    also remove configs, scenarios, models, zenoh-c, zenohd,
#                                   ONNX Runtime, the training images and service users
source "$(dirname "$0")/lib/common.sh"
PURGE=0; [[ "${1:-}" == "--purge" ]] && PURGE=1
need_sudo

step "Stopping services"
for s in edge-eye sar-web rescue-dispatch mission-planner casualty-analyst rubblesar-taky zenohd; do
  if [[ -f /etc/systemd/system/$s.service ]]; then
    sudo systemctl disable --now "$s" >/dev/null 2>&1 || true
    sudo rm -f "/etc/systemd/system/$s.service"
    ok "removed $s.service"
  fi
done
sudo systemctl daemon-reload

step "Removing binaries"
for b in edge-eye sar-check sar-monitor sar-doctor scenario-tool sar-infer casualty-analyst mission-planner rescue-dispatch sar-web rubblesar; do sudo rm -f "/usr/local/bin/$b"; done
sudo rm -rf /opt/rubblesar
ok "/opt/rubblesar removed"

if [[ $PURGE -eq 1 ]]; then
  step "Purging configuration, zenoh-c, zenohd and accounts"
  sudo rm -rf /etc/rubblesar /var/lib/rubblesar /etc/chrony/conf.d/rubblesar.conf /etc/chrony/sources.d/rubblesar.sources
  if [[ -f /usr/local/share/rubblesar/zenoh-c.manifest ]]; then
    xargs -a /usr/local/share/rubblesar/zenoh-c.manifest sudo rm -f
    sudo rm -rf /usr/local/share/rubblesar
    sudo ldconfig
    ok "zenoh-c removed"
  fi
  sudo rm -f /usr/local/bin/zenohd; sudo rm -rf /usr/local/lib/zenoh
  sudo rm -rf /opt/onnxruntime-* /usr/local/lib/pkgconfig/rubblesar-onnxruntime.pc
  ok "ONNX Runtime removed"
  if command -v docker >/dev/null 2>&1; then
    imgs="$(sudo docker images -q rubblesar-train 2>/dev/null || true)"
    [[ -n "$imgs" ]] && sudo docker rmi -f $imgs >/dev/null 2>&1 && ok "rubblesar-train images removed" || true
    imgs="$(sudo docker images -q rubblesar-taky 2>/dev/null || true)"
    [[ -n "$imgs" ]] && sudo docker rmi -f $imgs >/dev/null 2>&1 && ok "rubblesar-taky image removed" || true
  fi
  for u in rubblesar zenohd; do id "$u" >/dev/null 2>&1 && sudo userdel "$u" || true; done
  sudo systemctl restart chrony 2>/dev/null || true
  ok "purged (Go in /usr/local/go and the repository's data/ folder were left in place)"
fi
finish
