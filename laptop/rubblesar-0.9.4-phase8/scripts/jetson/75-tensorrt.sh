#!/usr/bin/env bash
# Phase 2 - Jetson: make sure the TensorRT pieces the build and engine tools need
# are installed (they ship with JetPack 6.2: TensorRT 10.3, CUDA 12.6).
# Run before scripts/jetson/20-build.sh so the programs are built with TensorRT.
source "$(dirname "$0")/../lib/common.sh"

[[ -f /etc/nv_tegra_release ]] || die E-SETUP-HW "this is not a Jetson" "run it on the Jetson"
need_sudo

step "TensorRT headers, trtexec and the CUDA runtime headers"
MISSING=()
[[ -f /usr/include/aarch64-linux-gnu/NvInfer.h ]] || MISSING+=(libnvinfer-dev libnvinfer-headers-dev)
[[ -x /usr/src/tensorrt/bin/trtexec ]] || MISSING+=(libnvinfer-bin)
[[ -f /usr/local/cuda/include/cuda_runtime_api.h ]] || MISSING+=(cuda-cudart-dev-12-6)
if [[ ${#MISSING[@]} -gt 0 ]]; then
  info "installing: ${MISSING[*]}"
  sudo apt-get update -qq || die E-SETUP-APT "apt-get update failed"
  sudo apt-get install -y "${MISSING[@]}" \
    || die E-SETUP-TRT "could not install ${MISSING[*]}" "install the full JetPack: sudo apt install nvidia-jetpack"
fi
[[ -f /usr/include/aarch64-linux-gnu/NvInfer.h ]] || die E-SETUP-TRT "NvInfer.h still missing" "sudo apt install nvidia-jetpack"
[[ -x /usr/src/tensorrt/bin/trtexec ]] || die E-SETUP-TRT "trtexec still missing" "sudo apt install libnvinfer-bin"
[[ -f /usr/local/cuda/include/cuda_runtime_api.h ]] || die E-SETUP-TRT "CUDA headers missing" "sudo apt install cuda-toolkit-12-6"
TRT_VER="$(dpkg-query -W -f='${Version}' libnvinfer10 2>/dev/null || echo unknown)"
ok "TensorRT $TRT_VER, trtexec, CUDA headers present"

step "Power mode (speed matters for phase 2)"
if command -v nvpmodel >/dev/null; then
  info "current: $(sudo nvpmodel -q 2>/dev/null | head -n1 || echo unknown)"
  info "for full speed pick the highest mode (on JetPack 6.2 the Orin Nano has 'MAXN SUPER'):"
  info "  sudo nvpmodel -p --verbose | grep POWER_MODEL     (lists the modes and their numbers)"
  info "  sudo nvpmodel -m <number>; sudo jetson_clocks"
fi
finish "scripts/jetson/20-build.sh, then scripts/jetson/30-install.sh --zbook-ip <ZBook IP>"
