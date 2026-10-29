#!/usr/bin/env bash
# Phase 5 - the streets for rescue routes: downloaded once from OpenStreetMap
# (Overpass API) around the incident and installed for rescue-dispatch, which
# closes the streets under each rubble spill area itself.
#   scripts/zbook/32-roads.sh [--force] [--synthetic] [--from FILE]
#     --force       download again even if the street file exists
#     --synthetic   no internet: a street grid every 60 m instead of the real streets
#     --from FILE   use an Overpass JSON file downloaded elsewhere (scenario-tool fetch-roads)
source "$(dirname "$0")/../lib/common.sh"

FORCE=0 SYNTH=0 FROM=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --force) FORCE=1; shift ;;
    --synthetic) SYNTH=1; shift ;;
    --from) FROM="$(resolve_path "${2:-}")"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--force] [--synthetic] [--from FILE]" ;;
  esac
done

TOOL=/opt/rubblesar/bin/scenario-tool
[[ -x "$TOOL" ]] || TOOL="$SAR_ROOT/bin/scenario-tool"
[[ -x "$TOOL" ]] || die E-SETUP-BIN "scenario-tool missing" "run scripts/zbook/20-build.sh first"
DSP=/opt/rubblesar/bin/rescue-dispatch
[[ -x "$DSP" ]] || DSP="$SAR_ROOT/bin/rescue-dispatch"
CFG=/etc/rubblesar/sar.toml
[[ -f "$CFG" ]] || CFG="$SAR_ROOT/configs/zbook/sar.toml"
INCIDENT="$(sed -n 's/^incident_id *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
INCIDENT="${INCIDENT:-antakya-demo}"
GEN_CFG="$SAR_ROOT/configs/scenario/$INCIDENT.toml"
[[ -f "$GEN_CFG" ]] || die E-SETUP-CFG "no scenario config $GEN_CFG for incident $INCIDENT" "the incident centre and radius come from it"
DEST_DIR=/var/lib/rubblesar/roads
DEST="$DEST_DIR/$INCIDENT.json"
need_sudo

step "Street file $DEST"
KEEP_OLD=0
if [[ -f "$DEST" && $FORCE -eq 0 && $SYNTH -eq 0 && -z "$FROM" ]]; then
  if grep -q '"Overpass API' "$DEST" && ! grep -q '"waterway"' "$DEST"; then
    # Downloaded by 0.6.0: no rivers, so walks off the streets could cross the Asi.
    info "the installed street file has no rivers (downloaded by 0.6.0): downloading it again with them"
    FORCE=1
    KEEP_OLD=1
  else
    ok "already installed (use --force to download again)"
  fi
fi
if [[ ! -f "$DEST" || $FORCE -eq 1 || $SYNTH -eq 1 || -n "$FROM" ]]; then
  TMP="$(mktemp -d)"
  OUT="$TMP/$INCIDENT.json"
  if [[ -n "$FROM" ]]; then
    [[ -f "$FROM" ]] || die E-SETUP-ARG "no file $FROM"
    cp "$FROM" "$OUT"
    ok "using $FROM"
  elif [[ $SYNTH -eq 1 ]]; then
    "$TOOL" fetch-roads --config "$GEN_CFG" --out "$OUT" --synthetic
    warn "synthetic street grid: routes avoid the rubble but do not follow Antakya's real streets"
  else
    if ! "$TOOL" fetch-roads --config "$GEN_CFG" --out "$OUT"; then
      if [[ $KEEP_OLD -eq 1 ]]; then
        warn "could not download the streets again: kept the old file (routes may walk across the river); run $0 --force when online"
        OUT=""
      else
        die E-SETUP-ROADS "could not download the streets from OpenStreetMap" \
          "check internet access and try again later (the Overpass API is sometimes busy), or run $0 --synthetic for a street grid"
      fi
    fi
  fi
  if [[ -n "$OUT" ]]; then
    sudo install -d -m 0755 "$DEST_DIR"
    sudo install -m 0644 "$OUT" "$DEST"
    ok "installed"
  fi
  rm -rf "$TMP"
fi

step "Streets and the rubble that closes them"
if [[ -x "$DSP" ]]; then
  # The street file is read with the installed config, as the service does.
  "$DSP" roads --config "$CFG" | sed 's/^/    /' \
    || die E-SETUP-ROADS "rescue-dispatch cannot use $DEST" "paste the block; run $0 --force to download again"
else
  warn "rescue-dispatch not built yet (scripts/zbook/20-build.sh): not checked"
fi

if systemctl is-active --quiet rescue-dispatch 2>/dev/null; then
  step "rescue-dispatch"
  sudo systemctl restart rescue-dispatch
  ok "restarted with the new street file"
fi

finish "scripts/zbook/34-taky.sh (the TAK server), then 90-phase5-test.sh"
