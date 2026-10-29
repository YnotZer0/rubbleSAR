#!/usr/bin/env bash
# Phase 1 - ZBook: generate the simulated incident, render its imagery layers,
# verify them, make the preview pages and install the result for the services.
#
#   scripts/zbook/60-scenario.sh [--config FILE] [--osm] [--force] [--no-preview]
#
# --config FILE  generator config (default configs/scenario/antakya-demo.toml)
# --osm          use real building outlines from OpenStreetMap (needs internet)
# --force        replace an existing, different scenario of the same id
# --no-preview   skip the preview pictures (saves about 30 s)
#
# Output: data/scenarios/<id>/ (with preview/index.html) and /var/lib/rubblesar/scenarios/<id>/
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"

CONFIG="$SAR_ROOT/configs/scenario/antakya-demo.toml"
OSM=0; FORCE=(); PREVIEW=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) CONFIG="${2:-}"; shift 2 ;;
    --osm) OSM=1; shift ;;
    --force) FORCE=(--force); shift ;;
    --no-preview) PREVIEW=0; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--config FILE] [--osm] [--force] [--no-preview]" ;;
  esac
done
[[ -f "$CONFIG" ]] || die E-CFG-001 "config $CONFIG not found"
TOOL="$(tool_bin scenario-tool)"
ID="$(scenario_id "$CONFIG")"
need_sudo   # asked now, so the script can run unattended afterwards
OUT="$SAR_ROOT/data/scenarios/$ID"
mkdir -p "$SAR_ROOT/data/scenarios"
info "$("$TOOL" version)"
info "config $CONFIG -> $OUT"

if [[ $OSM -eq 1 ]]; then
  step "Downloading building outlines from OpenStreetMap (Overpass API)"
  OSM_FILE="$SAR_ROOT/data/osm-$ID-buildings.json"
  if [[ -s "$OSM_FILE" ]]; then
    ok "using the earlier download $OSM_FILE (delete it to download again)"
  else
    "$TOOL" fetch-osm --config "$CONFIG" --out "$OSM_FILE"
  fi
  GEN_CFG="$SAR_ROOT/data/$ID-osm.toml"
  sed "s#^osm_buildings = .*#osm_buildings = \"$OSM_FILE\"#" "$CONFIG" > "$GEN_CFG"
  grep -q "^osm_buildings = \"$OSM_FILE\"" "$GEN_CFG" || die E-CFG-003 "could not set osm_buildings in a copy of $CONFIG" "add the line osm_buildings = \"\" under [sites]"
  CONFIG="$GEN_CFG"
  ok "generator config with OpenStreetMap outlines: $CONFIG"
fi

step "Generating the scenario"
"$TOOL" generate --config "$CONFIG" --out "$OUT" "${FORCE[@]}"

step "Rendering imagery layers (about 15-60 s)"
"$TOOL" render --scenario "$OUT"

step "Verifying layers"
"$TOOL" verify --scenario "$OUT"

if [[ $PREVIEW -eq 1 ]]; then
  step "Preview pictures for checking by eye (about 30-60 s)"
  "$TOOL" preview --scenario "$OUT" --out "$OUT/preview"
fi

step "Installing into $SCENARIO_ROOT/$ID"
install_scenario "$OUT" "$ID"
HASH="$(scenario_hash "$SCENARIO_ROOT/$ID")"
ok "scenario $ID, hash $HASH"

step "Sample frames (scout at 40 m, investigator at 12 m)"
FR="$SAR_ROOT/data/frames/$ID"
"$TOOL" frame --scenario "$SCENARIO_ROOT/$ID" --out "$FR/scout-40m" --alt 40 --uav scout-1 | sed -n 1p
FIRST_V="$("$TOOL" info --scenario "$SCENARIO_ROOT/$ID" | awk '$1 ~ /^v[0-9]+$/ && !f {print $1; f=1}')"
"$TOOL" frame --scenario "$SCENARIO_ROOT/$ID" --out "$FR/inv-12m-$FIRST_V" --victim "$FIRST_V" --alt 12 --uav inv-1 | sed -n 1p
ok "frames in $FR"

echo
echo "    Scenario hash: $HASH   (the Jetson's 60-scenario.sh prints the same value)"
if [[ $PREVIEW -eq 1 ]]; then
  echo "    Look at it:    xdg-open $OUT/preview/index.html"
fi
finish "scripts/zbook/70-phase1-test.sh, then on the Jetson scripts/jetson/60-scenario.sh (or push it from here: scripts/zbook/65-push-scenario.sh --jetson user@<jetson-ip>)"
