#!/usr/bin/env bash
# Phase 0 - Jetson step 1 of 4: system packages, Go, time sync, camera group.
# Run as your normal user from the repository root:  scripts/jetson/00-prereqs.sh
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/install-go.sh"

step "Checking this is a Jetson with JetPack 6.2.x"
[[ -f /etc/nv_tegra_release ]] || die E-SETUP-HW "no /etc/nv_tegra_release: this is not a Jetson" "use scripts/zbook/ on the ZBook"
info "$(head -n1 /etc/nv_tegra_release)"
grep -q "R36" /etc/nv_tegra_release || die E-SETUP-HW "Jetson Linux R36 (JetPack 6.x) is required" "flash JetPack 6.2.2"
. /etc/os-release
[[ "$VERSION_ID" == "22.04" ]] || warn "expected Ubuntu 22.04 (JetPack 6), found $PRETTY_NAME"
ok "Jetson Linux R36 on $PRETTY_NAME"

need_sudo

step "Installing system packages (apt)"
sudo apt-get update || die E-SETUP-APT "apt-get update failed" "check the Jetson's internet connection and /etc/apt/sources.list.d/"
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  build-essential pkg-config git curl unzip ca-certificates chrony python3 v4l-utils
ok "packages installed"

step "Installing Go $GO_VERSION (native, /usr/local/go)"
install_go

step "Enabling time synchronisation (chrony)"
sudo systemctl enable --now chrony
ok "chrony $(systemctl is-active chrony); it is pointed at the ZBook by 30-install.sh"

step "Camera access for $(id -un)"
if id -nG "$(id -un)" | grep -qw video; then
  ok "already in the video group"
else
  sudo usermod -aG video "$(id -un)"
  ok "added to the video group (log out and in for it to apply)"
fi

step "Environment check (TODO lines are installed by the next steps)"
(cd "$SAR_ROOT" && GOFLAGS=-mod=vendor make doctor >/dev/null && ./bin/sar-doctor --role jetson --stage prereqs --report-dir reports) || true

finish "scripts/jetson/10-zenoh-c.sh"
