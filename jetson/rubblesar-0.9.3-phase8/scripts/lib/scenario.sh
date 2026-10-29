#!/usr/bin/env bash
# Scenario helpers shared by scripts/{zbook,jetson}/60-scenario.sh and 70-phase1-test.sh
# (requires common.sh).

SCENARIO_ROOT=/var/lib/rubblesar/scenarios

# tool_bin NAME: the installed binary, else the one in ./bin.
tool_bin() {
  if [[ -x /opt/rubblesar/bin/$1 ]]; then echo "/opt/rubblesar/bin/$1"
  elif [[ -x "$SAR_ROOT/bin/$1" ]]; then echo "$SAR_ROOT/bin/$1"
  else die E-SETUP-BIN "$1 not found" "run scripts/$SCRIPT_DIR_NAME/20-build.sh and 30-install.sh first"
  fi
}

# scenario_id CONFIG: the [scenario] id of a generator config.
scenario_id() {
  local id
  id="$(awk -F'"' '/^\[scenario\]/{s=1;next} /^\[/{s=0} s && /^[[:space:]]*id[[:space:]]*=/{print $2; exit}' "$1")"
  [[ "$id" =~ ^[a-z0-9][a-z0-9-]{0,31}$ ]] || die E-CFG-003 "cannot read [scenario] id from $1 (got '$id')" "the id must use a-z, 0-9 and '-'"
  echo "$id"
}

# install_scenario SRC_DIR ID: copy a rendered scenario into /var/lib (read-only for services).
install_scenario() {
  local src="$1" id="$2" dst="$SCENARIO_ROOT/$2"
  [[ -f "$src/scenario.json" ]] || die E-SCN-001 "$src has no scenario.json" "generate it first, or copy the whole folder from the ZBook"
  # Create only what is missing: /var/lib/rubblesar belongs to edge-eye's account on the Jetson.
  [[ -d /var/lib/rubblesar ]] || sudo install -d -m 0755 /var/lib/rubblesar
  [[ -d "$SCENARIO_ROOT" ]] || sudo install -d -m 0755 "$SCENARIO_ROOT"
  sudo rm -rf "$dst.new"
  if command -v rsync >/dev/null 2>&1; then
    sudo rsync -a --exclude preview "$src/" "$dst.new/"
  else
    sudo cp -a "$src" "$dst.new"
    sudo rm -rf "$dst.new/preview"
  fi
  sudo chown -R root:root "$dst.new"
  sudo chmod -R u=rwX,go=rX "$dst.new"
  sudo rm -rf "$dst"
  sudo mv "$dst.new" "$dst"
  ok "installed $dst ($(du -sh "$dst" | cut -f1))"
}

# scenario_hash DIR: first 12 hex digits of the scenario hash, as tools print it.
scenario_hash() {
  "$(tool_bin scenario-tool)" verify --scenario "$1" | awk '/^RESULT: PASS/{gsub(/\)/,"",$NF); print $NF}'
}
