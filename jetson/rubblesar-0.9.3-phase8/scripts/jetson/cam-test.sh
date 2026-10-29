#!/usr/bin/env bash
# Optional: CSI camera smoke test. Grabs one JPEG from each sensor through the
# NVIDIA Argus pipeline. Cameras are not used until phase 3 (imagery stays simulated).
#
#   scripts/jetson/cam-test.sh [sensor-ids...]     default: 0 1
source "$(dirname "$0")/../lib/common.sh"

need_cmd gst-launch-1.0 "nvidia-l4t-gstreamer"
OUT="$SAR_ROOT/reports/camera"
mkdir -p "$OUT"
SENSORS=("$@")
[[ ${#SENSORS[@]} -gt 0 ]] || SENSORS=(0 1)

step "Argus camera daemon"
systemctl is-active --quiet nvargus-daemon || die E-CAM-001 "nvargus-daemon is not running" "sudo systemctl restart nvargus-daemon"
ok "nvargus-daemon active"
ls -l /dev/video* 2>/dev/null | sed 's/^/    /' || warn "no /dev/video* devices"

FAILED=()
for id in "${SENSORS[@]}"; do
  step "Sensor $id: capturing one frame"
  f="$OUT/sensor${id}-$(date +%H%M%S).jpg"
  if timeout 20 gst-launch-1.0 -q nvarguscamerasrc sensor-id="$id" num-buffers=15 \
      ! 'video/x-raw(memory:NVMM),width=1920,height=1080,framerate=30/1' \
      ! nvvidconv ! 'video/x-raw,format=I420' ! jpegenc ! multifilesink location="$f" max-files=1 >"$OUT/sensor$id.log" 2>&1 \
      && [[ -s "$f" ]]; then
    ok "saved $f ($(stat -c %s "$f") bytes)"
  else
    sed 's/^/    /' "$OUT/sensor$id.log" | tail -n 15
    FAILED+=("sensor $id")
    warn "sensor $id failed (check the ribbon cable orientation and 'sudo /opt/nvidia/jetson-io/jetson-io.py' camera configuration)"
  fi
done
[[ ${#FAILED[@]} -eq 0 ]] || die E-CAM-002 "capture failed for: ${FAILED[*]}" "cameras are optional for phase 0; this does not block the phase 0 acceptance test"
finish "copy the JPEGs from $OUT to check focus and orientation"
