#!/usr/bin/env bash
# Phase 0 - ZBook step 1 of 5: system packages, Go, time sync.
# Run as your normal user from the repository root:  scripts/zbook/00-prereqs.sh
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/install-go.sh"

step "Checking the machine"
[[ -f /etc/nv_tegra_release ]] && die E-SETUP-HW "this is a Jetson" "use scripts/jetson/ here"
[[ "$(arch_name)" == "x86_64" ]] || die E-SETUP-HW "the ZBook scripts expect x86_64, found $(uname -m)"
. /etc/os-release
[[ "$VERSION_ID" == "26.04" ]] || warn "tested on Ubuntu 26.04, found $PRETTY_NAME"
ok "$PRETTY_NAME, $(uname -m)"

need_sudo

step "Installing system packages (apt)"
sudo apt-get update || die E-SETUP-APT "apt-get update failed" "check internet access and /etc/apt/sources.list.d/"
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  build-essential pkg-config git curl unzip ca-certificates chrony python3 python3-venv
ok "packages installed"

step "Go $GO_VERSION"
install_go

step "Time synchronisation (chrony)"
sudo systemctl enable --now chrony
ok "chrony $(systemctl is-active chrony); 30-install.sh lets the Jetson use this machine as its clock"

step "GPU and Docker (needed in later phases; reported only)"
if command -v nvidia-smi >/dev/null; then
  info "$(nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader 2>&1 | head -n1)"
else
  warn "nvidia-smi not found"
fi
if docker info >/dev/null 2>&1; then
  info "Docker $(docker info --format '{{.ServerVersion}}')"
else
  warn "docker not usable by $(id -un) (fine for phase 0; later: sudo usermod -aG docker $(id -un))"
fi

step "Environment check (TODO lines are installed by the next steps)"
(cd "$SAR_ROOT" && GOFLAGS=-mod=vendor make doctor >/dev/null && ./bin/sar-doctor --role zbook --stage prereqs --report-dir reports) || true

finish "scripts/zbook/10-zenoh-c.sh"
