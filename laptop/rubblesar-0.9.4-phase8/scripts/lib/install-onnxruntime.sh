#!/usr/bin/env bash
# Installs the official ONNX Runtime $ONNXRUNTIME_VERSION CPU release into
# /opt/onnxruntime-$ONNXRUNTIME_VERSION and a pkg-config file for the Go build
# (rubblesar-onnxruntime.pc). Sourced by 12-onnxruntime.sh (requires common.sh).

install_onnxruntime() {
  local arch ortarch; arch="$(arch_name)"
  case "$arch" in x86_64) ortarch=x64 ;; aarch64) ortarch=aarch64 ;; esac
  local file="onnxruntime-linux-${ortarch}-${ONNXRUNTIME_VERSION}.tgz"
  local var="ONNXRUNTIME_SHA256_${arch^^}"
  local url="https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/${file}"
  local prefix="/opt/onnxruntime-${ONNXRUNTIME_VERSION}"

  step "Downloading ONNX Runtime $ONNXRUNTIME_VERSION for $arch"
  fetch "$url" "$file" "${!var}"

  step "Installing into $prefix"
  local tmp; tmp="$(mktemp -d)"
  tar -xzf "$DOWNLOAD_DIR/$file" -C "$tmp"
  local src="$tmp/onnxruntime-linux-${ortarch}-${ONNXRUNTIME_VERSION}"
  [[ -f "$src/include/onnxruntime_c_api.h" && -f "$src/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION}" ]] \
    || die E-SETUP-ORT "archive layout unexpected (no include/onnxruntime_c_api.h or lib/libonnxruntime.so.${ONNXRUNTIME_VERSION})"
  sudo rm -rf "$prefix"
  sudo mkdir -p "$prefix" /usr/local/lib/pkgconfig
  sudo cp -a "$src/include" "$src/lib" "$src/LICENSE" "$prefix/"
  sudo tee /usr/local/lib/pkgconfig/rubblesar-onnxruntime.pc >/dev/null <<PC
prefix=$prefix
Name: rubblesar-onnxruntime
Description: ONNX Runtime $ONNXRUNTIME_VERSION (CPU) for RubbleSAR
Version: $ONNXRUNTIME_VERSION
Cflags: -I\${prefix}/include
Libs: -L\${prefix}/lib -lonnxruntime -Wl,-rpath,\${prefix}/lib
PC
  rm -rf "$tmp"
  ok "installed $prefix"

  step "Verifying"
  export PKG_CONFIG_PATH="/usr/local/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
  local v; v="$(pkg-config --modversion rubblesar-onnxruntime 2>/dev/null || true)"
  [[ "$v" == "$ONNXRUNTIME_VERSION" ]] || die E-SETUP-ORT "pkg-config reports rubblesar-onnxruntime '${v:-nothing}', expected $ONNXRUNTIME_VERSION" \
    "ensure /usr/local/lib/pkgconfig is searched: pkg-config --variable pc_path pkg-config"
  ok "pkg-config rubblesar-onnxruntime $v (rebuild with the 20-build script to use it)"
}
