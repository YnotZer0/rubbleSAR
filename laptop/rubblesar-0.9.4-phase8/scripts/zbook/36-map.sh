#!/usr/bin/env bash
# Phase 6 - the offline incident map: a street-level map of the incident area
# for WinTAK and ATAK, drawn on this ZBook from OpenStreetMap data (streets
# with their names, the river, buildings, green areas) and the scenario (the
# collapsed buildings, their rubble, the streets it closes), served by sar-web.
# Works without internet once drawn.
#   scripts/zbook/36-map.sh [--no-download] [--force] [--max-zoom 19] [--jobs N]
#     --no-download  draw from the street file only (no buildings, water or green areas)
#     --force        download the buildings again
source "$(dirname "$0")/../lib/common.sh"

NO_DL=0 FORCE=0 MAXZ=19 JOBS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-download) NO_DL=1; shift ;;
    --force) FORCE=1; shift ;;
    --max-zoom) MAXZ="${2:-19}"; shift 2 ;;
    --jobs) JOBS="${2:-}"; shift 2 ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--no-download] [--force] [--max-zoom 19] [--jobs N]" ;;
  esac
done
[[ "$MAXZ" =~ ^(1[5-9]|20)$ ]] || die E-SETUP-ARG "--max-zoom $MAXZ" "use 15 to 20 (19 is street level, 20 doubles the tiles)"

TOOL=/opt/rubblesar/bin/scenario-tool
DSP=/opt/rubblesar/bin/rescue-dispatch
for b in "$TOOL" "$DSP"; do
  [[ -x "$b" ]] || die E-SETUP-BIN "$b missing" "run scripts/zbook/20-build.sh and 30-install.sh first"
done
CFG=/etc/rubblesar/sar.toml
[[ -f "$CFG" ]] || die E-SETUP-CFG "no $CFG" "run scripts/zbook/30-install.sh first"
INCIDENT="$(sed -n 's/^incident_id *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
INCIDENT="${INCIDENT:-antakya-demo}"
GEN_CFG="$SAR_ROOT/configs/scenario/$INCIDENT.toml"
[[ -f "$GEN_CFG" ]] || die E-SETUP-CFG "no scenario config $GEN_CFG for incident $INCIDENT" "the incident centre and radius come from it"
WEB_ROOT="$(sed -n '/^\[web\]/,/^\[/s/^root *= *"\(.*\)"/\1/p' "$CFG" | head -n1)"
WEB_ROOT="${WEB_ROOT:-/var/lib/rubblesar/www}"
WEB_PORT="$(sed -n '/^\[web\]/,/^\[/s/^listen *= *"[^:]*:\([0-9]*\)"/\1/p' "$CFG" | head -n1)"
WEB_PORT="${WEB_PORT:-8090}"
DATA=/var/lib/rubblesar/map
EXTRAS="$DATA/$INCIDENT-osm.json"
LAYERS="$DATA/$INCIDENT-layers.json"
DEST="$WEB_ROOT/map/$INCIDENT"
RENDER="$SAR_ROOT/tools/map/render_map.py"
[[ -f "$RENDER" ]] || die E-SETUP-BIN "$RENDER missing" "run this script from the release folder"
need_sudo

step "Drawing tools (Python 3, Pillow, DejaVu fonts)"
if ! python3 -c 'import PIL' 2>/dev/null || [[ ! -f /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf ]]; then
  sudo apt-get install -y python3-pil fonts-dejavu-core >/dev/null || die E-SETUP-PKG "could not install python3-pil and fonts-dejavu-core" "check internet access and 'sudo apt-get update'"
fi
ok "$(python3 -c 'import PIL, sys; print("Python %s, Pillow %s" % (sys.version.split()[0], PIL.__version__))')"

[[ -f "$(sed -n 's/^roads *= *"\(.*\)"/\1/p' "$CFG" | head -n1)" || -f "/var/lib/rubblesar/roads/$INCIDENT.json" ]] \
  || warn "no street file: the map will have no streets (run scripts/zbook/32-roads.sh first)"

step "Buildings, water and green areas ($EXTRAS)"
sudo install -d -m 0755 "$DATA"
USE_EXTRAS=1
if [[ $NO_DL -eq 1 ]]; then
  if [[ -f "$EXTRAS" ]]; then ok "using the file downloaded before"; else warn "--no-download: the map shows the streets only"; USE_EXTRAS=0; fi
elif [[ -f "$EXTRAS" && $FORCE -eq 0 ]]; then
  ok "already downloaded (use --force to download again)"
else
  TMP="$(mktemp -d)"
  if "$TOOL" fetch-map --config "$GEN_CFG" --out "$TMP/osm.json"; then
    sudo install -m 0644 "$TMP/osm.json" "$EXTRAS"
    ok "installed"
  elif [[ -f "$EXTRAS" ]]; then
    warn "could not download them again: using the file downloaded before"
  else
    warn "could not download them (no internet? the Overpass API busy?): the map shows the streets only; run $0 again later"
    USE_EXTRAS=0
  fi
  rm -rf "$TMP"
fi

step "Map layers (streets as rescue-dispatch routes over them, rubble, collapses)"
TMPL="$(mktemp -d)"
ARGS=(map-layers --config "$CFG" --out "$TMPL/layers.json")
[[ $USE_EXTRAS -eq 1 ]] && ARGS+=(--extras "$EXTRAS")
"$DSP" "${ARGS[@]}" | sed 's/^/    /'
sudo install -m 0644 "$TMPL/layers.json" "$LAYERS"

step "Drawing the tiles, zoom 13 to $MAXZ (about 2 minutes)"
OUT="$TMPL/tiles"
RARGS=(--layers "$LAYERS" --out "$OUT" --mbtiles "$OUT/$INCIDENT.mbtiles" --max-zoom "$MAXZ" --legend "$OUT/legend.png" --overview "$OUT/overview.png")
[[ -n "$JOBS" ]] && RARGS+=(--jobs "$JOBS")
if ! python3 -I "$RENDER" "${RARGS[@]}" 2>&1 | sed 's/^/    /'; then
  die E-MAP-003 "drawing the tiles failed" "paste the block above; python3-pil must be installed (sudo apt install python3-pil)"
fi

step "Installing to $DEST"
sudo install -d -m 0755 "$WEB_ROOT/map"
sudo rm -rf "$DEST.new" "$DEST.old"
sudo cp -r "$OUT" "$DEST.new"
sudo chmod -R a+rX "$DEST.new"
[[ -d "$DEST" ]] && sudo mv "$DEST" "$DEST.old"
sudo mv "$DEST.new" "$DEST"
sudo rm -rf "$DEST.old" "$TMPL"
ok "$(find "$DEST" -name '*.png' | wc -l) tiles, $(du -sh "$DEST" | cut -f1)"

step "sar-web"
if systemctl is-active --quiet sar-web; then
  /opt/rubblesar/bin/sar-web status | sed 's/^/    /'
else
  warn "sar-web is not running: scripts/zbook/30-install.sh installs it"
fi
IPS="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -vE '^(127\.|172\.1[7-9]\.)' | head -n3 | tr '\n' ' ')"
cat <<EOF

    On the WinTAK laptop (same network), open in a browser:
EOF
for ip in $IPS; do echo "      http://$ip:$WEB_PORT/"; done
cat <<EOF
    and either
      - download rubblesar-map.xml there, quit WinTAK, copy the file into C:\\ProgramData\\WinTAK\\Imagery,
        start WinTAK, then Maps & Favorites > MOBILE > Online > "RubbleSAR $INCIDENT" (streams from this ZBook), or
      - download $INCIDENT.mbtiles, quit WinTAK, copy it into C:\\ProgramData\\WinTAK\\Imagery, start WinTAK,
        then MOBILE > Local (works away from the ZBook).
    Map data (c) OpenStreetMap contributors (ODbL).
EOF

finish "scripts/zbook/38-jetson-link.sh (rubblesar up/down for the Jetson), then 90-phase6-test.sh"
