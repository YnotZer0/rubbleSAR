#!/usr/bin/env bash
# Installs the official zenoh-c $ZENOH_VERSION release (headers, libzenohc.so,
# pkg-config file) into /usr/local. Sourced by 10-zenoh-c.sh (requires common.sh).

install_zenoh_c() {
  local arch; arch="$(arch_name)"
  local file="zenoh-c-${ZENOH_VERSION}-${arch}-unknown-linux-gnu-standalone.zip"
  local var="ZENOH_C_SHA256_${arch^^}"
  local sum="${!var}"
  local url="https://github.com/eclipse-zenoh/zenoh-c/releases/download/${ZENOH_VERSION}/${file}"

  step "Downloading zenoh-c $ZENOH_VERSION for $arch"
  fetch "$url" "$file" "$sum"

  step "Unpacking"
  local tmp; tmp="$(mktemp -d)"
  unzip -q -o "$DOWNLOAD_DIR/$file" -d "$tmp"
  [[ -f "$tmp/lib/libzenohc.so" && -f "$tmp/include/zenoh.h" ]] \
    || die E-SETUP-ZC "archive layout unexpected (no lib/libzenohc.so or include/zenoh.h)"
  grep -q "\"$ZENOH_VERSION\"" "$tmp/include/zenoh_configure.h" \
    || die E-SETUP-ZC "archive is not zenoh-c $ZENOH_VERSION"
  ok "archive contains zenoh-c $ZENOH_VERSION"

  step "Installing into /usr/local"
  local manifest=/usr/local/share/rubblesar/zenoh-c.manifest
  sudo mkdir -p /usr/local/share/rubblesar /usr/local/lib/pkgconfig /usr/local/include
  sudo install -m 0755 "$tmp/lib/libzenohc.so" /usr/local/lib/libzenohc.so
  sudo install -m 0644 "$tmp"/include/*.h /usr/local/include/
  sed 's|^prefix=.*|prefix=/usr/local|' "$tmp/lib/pkgconfig/zenohc.pc" | sudo tee /usr/local/lib/pkgconfig/zenohc.pc >/dev/null
  {
    echo /usr/local/lib/libzenohc.so
    for h in "$tmp"/include/*.h; do echo "/usr/local/include/$(basename "$h")"; done
    echo /usr/local/lib/pkgconfig/zenohc.pc
  } | sudo tee "$manifest" >/dev/null
  sudo ldconfig
  rm -rf "$tmp"
  ok "files listed in $manifest"

  step "Verifying"
  export PKG_CONFIG_PATH="/usr/local/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
  local v; v="$(pkg-config --modversion zenohc 2>/dev/null || true)"
  [[ "$v" == "$ZENOH_VERSION" ]] || die E-SETUP-ZC "pkg-config reports zenohc '${v:-nothing}', expected $ZENOH_VERSION" \
    "ensure /usr/local/lib/pkgconfig is searched: pkg-config --variable pc_path pkg-config"
  ldconfig -p | grep -q libzenohc.so || die E-SETUP-ZC "libzenohc.so not in the linker cache" "sudo ldconfig"
  ok "pkg-config zenohc $v, libzenohc.so in linker cache"
}
