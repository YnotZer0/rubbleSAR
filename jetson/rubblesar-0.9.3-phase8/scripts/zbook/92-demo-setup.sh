#!/usr/bin/env bash
# Phase 8 - set up this ZBook to record the demo video: ffmpeg (the rough
# cut), OBS Studio (records the screen; the director starts and stops it over
# OBS's WebSocket server), scrcpy and adb (optional: record an ATAK phone over
# USB), and the director's settings (rubblesar demo init). docs/PHASE8.md has
# the whole workflow, and how to set up OBS on the WinTAK laptop.
#
#   scripts/zbook/92-demo-setup.sh [--no-obs] [--no-scrcpy]
#     --no-obs     leave OBS out (installed another way, e.g. Flatpak)
#     --no-scrcpy  no ATAK phone to record
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"   # tool_bin

OBS=1
SCRCPY=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-obs) OBS=0; shift ;;
    --no-scrcpy) SCRCPY=0; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--no-obs] [--no-scrcpy]" ;;
  esac
done
OPS="$(tool_bin rubblesar)"
need_sudo

step "ffmpeg (the rough cut)"
if ! command -v ffmpeg >/dev/null; then
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg || die E-SETUP-APT "ffmpeg could not be installed" "sudo apt-get update; check internet access"
fi
ok "$(ffmpeg -version | head -n1)"

obs_major() { obs --version 2>/dev/null | sed -n 's/.* \([0-9][0-9]*\)\.[0-9].*/\1/p' | head -n1; }
if [[ $OBS -eq 1 ]]; then
  step "OBS Studio 28 or later (records the screen; WebSocket server built in)"
  if ! command -v obs >/dev/null; then
    info "installing from the OBS project's PPA (ppa:obsproject/obs-studio)"
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y software-properties-common >/dev/null
    if sudo add-apt-repository -y ppa:obsproject/obs-studio && sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y obs-studio; then
      ok "installed"
    else
      warn "the PPA did not work on this Ubuntu release; install OBS from Flathub instead, then run this script with --no-obs:"
      info "  sudo apt install flatpak && flatpak remote-add --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo"
      info "  flatpak install -y flathub com.obsproject.Studio      (start it with: flatpak run com.obsproject.Studio)"
    fi
  fi
  if command -v obs >/dev/null; then
    M="$(obs_major)"
    if [[ -n "$M" && "$M" -lt 28 ]]; then
      warn "OBS $M is older than 28 and has no WebSocket server: remove it (sudo apt remove obs-studio) and run this script again"
    else
      ok "OBS Studio ${M:-(version not read)}"
    fi
  fi
fi

if [[ $SCRCPY -eq 1 ]]; then
  step "scrcpy and adb (optional: an ATAK phone recorded over USB)"
  if sudo DEBIAN_FRONTEND=noninteractive apt-get install -y scrcpy adb >/dev/null; then
    ok "$(scrcpy --version 2>/dev/null | head -n1)"
    info "on the phone: Settings > About > tap Build number 7 times; Developer options > USB debugging; connect it and accept the prompt"
    info "then 'adb devices' must list it; enable the [[recorder]] atak in demo.toml"
  else
    warn "scrcpy or adb could not be installed from apt (the ATAK recorder stays off)"
  fi
fi

step "The director's settings (as $(id -un), not root)"
"$OPS" demo init | sed 's/^/    /'
CONF="$HOME/.config/rubblesar/demo.toml"
[[ -f "$CONF" ]] && chmod 600 "$CONF"

step "What to do in OBS (once)"
info "1. Start OBS (Activities > OBS Studio). In the auto-configuration wizard choose 'Optimise just for recording',"
info "   base and output resolution 1920x1080, 30 fps."
info "2. Tools > WebSocket Server Settings: tick 'Enable WebSocket server', port 4455, tick 'Enable Authentication',"
info "   'Show Connect Info', copy the password into $CONF ([[recorder]] zbook, password = \"...\")."
info "3. Settings > Output > Recording: format 'Hybrid MP4' (OBS 30.2+: chapter marks) or mkv; a folder with space."
info "4. Leave OBS running and run: $OPS demo obs-setup --recorder zbook   (creates the presenter and operator-view scenes)"
info "5. Then: scripts/zbook/90-phase8-test.sh"
finish "OBS steps above, then scripts/zbook/90-phase8-test.sh (docs/PHASE8.md)"
