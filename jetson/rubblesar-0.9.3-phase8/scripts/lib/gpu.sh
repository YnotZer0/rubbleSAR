#!/usr/bin/env bash
# GPU access for the edge-eye service account on the Jetson (phase 3: the model
# runs inside the service). Sourced by scripts/jetson/30-install.sh and
# scripts/jetson/90-phase3-test.sh after common.sh.

EDGE_UNIT_SRC="$SAR_ROOT/deploy/jetson/systemd/edge-eye.service"

# The GPU device files CUDA opens on a Jetson (JetPack 6): memory (nvmap), the
# GPU itself (nvgpu), host1x and the DRM render node. Profiler and debugger
# nodes (group 'debug') are deliberately not included.
GPU_DEVICE_FILES=(/dev/nvmap /dev/nvhost-ctrl-gpu /dev/nvhost-gpu /dev/nvgpu/igpu0/as /dev/nvgpu/igpu0/channel
  /dev/nvgpu/igpu0/ctrl /dev/nvgpu/igpu0/power /dev/nvgpu/igpu0/tsg /dev/nvgpu/igpu0/nvsched /dev/host1x-fence /dev/dri/renderD*)

# gpu_device_groups: the groups (other than root) that own those files; on
# JetPack 6.2 'video' (nvmap, nvgpu) and 'render' (/dev/dri/renderD*).
gpu_device_groups() {
  local f
  for f in "${GPU_DEVICE_FILES[@]}"; do
    [[ -e "$f" ]] && stat -c %G "$f"
  done | sort -u | grep -vxE 'root|debug' || true
}

# gpu_devices_listing: the GPU device files and their owners, for error reports.
gpu_devices_listing() {
  { ls -l /dev/nvmap /dev/nvhost-ctrl-gpu /dev/host1x-fence 2>&1 | head -n 4
    ls -l /dev/nvgpu/igpu0 2>&1 | head -n 6
    ls -l /dev/dri 2>&1 | head -n 4; } || true
}

# gpu_add_service_groups: put the account 'rubblesar' in every group that owns
# a GPU device file (systemd picks up the new groups at the next start).
gpu_add_service_groups() {
  local g
  for g in $(gpu_device_groups); do
    if id -nG rubblesar | tr ' ' '\n' | grep -qx "$g"; then
      ok "rubblesar is in group $g (GPU device files)"
    else
      sudo usermod -aG "$g" rubblesar
      ok "rubblesar added to group $g (owns GPU device files)"
    fi
  done
}

# service_props UNIT: the account and sandbox settings of a unit file, as
# systemd-run -p options (one per line).
service_props() {
  local line
  while IFS= read -r line; do
    case "$line" in
      User=*|Group=*|SupplementaryGroups=*|Environment=*|WorkingDirectory=*|NoNewPrivileges=*|ProtectSystem=*|ProtectHome=*|ReadWritePaths=*|PrivateTmp=*)
        printf '%s\n%s\n' -p "$line" ;;
    esac
  done < "$1"
}

# gpu_test_as_service MODEL_DIR: load the model on the GPU exactly as the
# edge-eye service would (same account, groups and sandbox, via systemd-run).
# Prints sar-infer's output; succeeds when the model loaded.
gpu_test_as_service() {
  local model="$1" out props=()
  if command -v systemd-run >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    mapfile -t props < <(service_props "$EDGE_UNIT_SRC")
    out="$(sudo systemd-run --quiet --wait --pipe --collect "${props[@]}" /opt/rubblesar/bin/sar-infer info --model "$model" </dev/null 2>&1)" || true
  else
    out="$(sudo -u rubblesar /opt/rubblesar/bin/sar-infer info --model "$model" </dev/null 2>&1)" || true
  fi
  printf '%s\n' "$out"
  grep -q '^back end:' <<<"$out"
}

# gpu_model_dir CONFIG: [perception] model_dir from edge-eye.toml (default
# /var/lib/rubblesar/models/current); empty when the detector is the truth oracle.
gpu_model_dir() {
  local cfg="$1" dir det
  det="$(sudo sed -n 's/^[[:space:]]*detector[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" | head -n1)"
  [[ "$det" == "truth-oracle" ]] && return 0
  dir="$(sudo sed -n 's/^[[:space:]]*model_dir[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" | head -n1)"
  echo "${dir:-/var/lib/rubblesar/models/current}"
}

# gpu_check_service CONFIG: the whole check with a report on failure. Returns 1
# when the service account cannot use the GPU (the caller decides to stop).
gpu_check_service() {
  local cfg="$1" model out
  model="$(gpu_model_dir "$cfg")"
  if [[ -z "$model" ]]; then
    info "detector = truth-oracle: the service does not need the GPU"
    return 0
  fi
  if [[ ! -f "$model/model.engine" ]]; then
    warn "no TensorRT engine in $model yet: edge-eye flies without looking until scripts/jetson/80-engine.sh has run"
    return 0
  fi
  if out="$(gpu_test_as_service "$model")"; then
    ok "the model loads on the GPU as the service runs it: $(grep -m1 '^back end:' <<<"$out" | sed 's/^back end: *//')"
    return 0
  fi
  echo "    ---- as the edge-eye service (account rubblesar, service sandbox) ----"
  grep -E '^(ERROR|hint|back end)' <<<"$out" | head -n 4 | sed 's/^/    /' || true
  echo "    ---- as rubblesar outside the service ----"
  sudo -u rubblesar /opt/rubblesar/bin/sar-infer info --model "$model" </dev/null 2>&1 | grep -E '^(ERROR|back end)' | head -n 2 | sed 's/^/    /' || true
  echo "    ---- as $(id -un) ----"
  /opt/rubblesar/bin/sar-infer info --model "$model" </dev/null 2>&1 | grep -E '^(ERROR|back end)' | head -n 2 | sed 's/^/    /' || true
  echo "    ---- id rubblesar; GPU device files ----"
  id rubblesar | sed 's/^/    /'
  gpu_devices_listing | sed 's/^/    /'
  return 1
}
