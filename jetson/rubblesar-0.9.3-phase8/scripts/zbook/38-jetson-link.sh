#!/usr/bin/env bash
# Phase 6 - let this ZBook start and stop edge-eye on the Jetson, so that
# 'rubblesar up' and 'rubblesar down' handle both machines. Once:
#   - an SSH key for you on this ZBook, copied to the Jetson (asks the Jetson
#     password once);
#   - on the Jetson, a sudo rule allowing exactly 'systemctl start|stop|restart
#     edge-eye' without a password (asks your sudo password there once);
#   - [ops] jetson = "USER@ADDRESS" in /etc/rubblesar/sar.toml.
#   scripts/zbook/38-jetson-link.sh --jetson USER@JETSON-IP
source "$(dirname "$0")/../lib/common.sh"

TARGET=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --jetson) TARGET="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 --jetson USER@JETSON-IP" ;;
  esac
done
[[ "$TARGET" =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9.:-]+$ ]] || die E-SETUP-ARG "--jetson USER@ADDRESS is required" "e.g. --jetson nvidia@192.168.1.60 (the account you log in to the Jetson with)"
JUSER="${TARGET%@*}"
CFG=/etc/rubblesar/sar.toml
[[ -f "$CFG" ]] || die E-SETUP-CFG "no $CFG" "run scripts/zbook/30-install.sh first"
SVC="$(sed -n '/^\[ops\]/,/^\[/s/^jetson_service *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
SVC="${SVC:-edge-eye}"
need_cmd ssh openssh-client
need_cmd ssh-copy-id openssh-client
need_sudo

step "Your SSH key on this ZBook"
KEY="$HOME/.ssh/id_ed25519"
if [[ -f "$KEY" ]]; then
  ok "using $KEY"
else
  mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
  ssh-keygen -q -t ed25519 -N "" -f "$KEY" -C "rubblesar@$(hostname)"
  ok "created $KEY"
fi

step "Copying it to $TARGET (the Jetson's password, once)"
if ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new "$TARGET" true 2>/dev/null; then
  ok "the key already works"
else
  ssh-copy-id -o StrictHostKeyChecking=accept-new -i "$KEY.pub" "$TARGET" || die E-SETUP-NET "could not copy the key to $TARGET" "check the address and user; on the Jetson: sudo systemctl status ssh"
  ssh -o BatchMode=yes -o ConnectTimeout=8 "$TARGET" true || die E-SETUP-NET "the key still does not work for $TARGET" "paste the block above"
  ok "the key works"
fi

step "sudo rule on the Jetson for '$SVC' (your sudo password there, once)"
RULE="$JUSER ALL=(root) NOPASSWD: /usr/bin/systemctl start $SVC, /usr/bin/systemctl stop $SVC, /usr/bin/systemctl restart $SVC, /bin/systemctl start $SVC, /bin/systemctl stop $SVC, /bin/systemctl restart $SVC"
if ssh -o BatchMode=yes "$TARGET" "sudo -n -l /usr/bin/systemctl restart $SVC" >/dev/null 2>&1; then
  ok "already allowed"
else
  ssh -t "$TARGET" "echo '$RULE' | sudo tee /etc/sudoers.d/rubblesar-ops >/dev/null && sudo chmod 0440 /etc/sudoers.d/rubblesar-ops && sudo visudo -cf /etc/sudoers.d/rubblesar-ops >/dev/null" \
    || die E-SETUP-SUDO "could not install the sudo rule on $TARGET" "is $JUSER allowed to use sudo on the Jetson?"
  ssh -o BatchMode=yes "$TARGET" "sudo -n -l /usr/bin/systemctl restart $SVC" >/dev/null || die E-SETUP-SUDO "the sudo rule does not work on $TARGET" "paste the block above"
  ok "installed /etc/sudoers.d/rubblesar-ops on the Jetson (only: systemctl start|stop|restart $SVC)"
fi
STATE="$(ssh -o BatchMode=yes "$TARGET" "systemctl is-active $SVC" 2>/dev/null || true)"
info "$SVC on the Jetson: ${STATE:-unknown}"

step "[ops] jetson in $CFG"
sudo cp "$CFG" "$CFG.bak.$(date +%s)"
if grep -q '^\[ops\]' "$CFG"; then
  if sed -n '/^\[ops\]/,/^\[/p' "$CFG" | grep -q '^jetson *='; then
    sudo sed -i "/^\[ops\]/,/^\[/s|^jetson *=.*|jetson = \"$TARGET\"|" "$CFG"
  else
    sudo sed -i "/^\[ops\]/a jetson = \"$TARGET\"" "$CFG"
  fi
else
  printf '\n[ops]\njetson = "%s"\njetson_service = "%s"\nmax_mission = "90m"\n' "$TARGET" "$SVC" | sudo tee -a "$CFG" >/dev/null
fi
/opt/rubblesar/bin/rubblesar version >/dev/null 2>&1 || die E-SETUP-BIN "rubblesar is not installed" "run scripts/zbook/30-install.sh"
ok "jetson = \"$TARGET\""

step "rubblesar status"
/opt/rubblesar/bin/rubblesar status | sed 's/^/    /' || warn "something is down (see above): 'rubblesar up' starts everything"

finish "rubblesar up, then scripts/zbook/90-phase6-test.sh"
