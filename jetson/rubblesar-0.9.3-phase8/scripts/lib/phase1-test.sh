#!/usr/bin/env bash
# Phase 1 acceptance steps shared by scripts/{zbook,jetson}/70-phase1-test.sh
# (requires common.sh and scenario.sh). $1 = role (zbook|jetson).

phase1_test() {
  local role="$1" id="${SAR_SCENARIO_ID:-antakya-demo}" quick=()
  [[ "${QUICK:-0}" -eq 1 ]] && quick=(--quick)
  local dir="$SCENARIO_ROOT/$id" check tool reports="$SAR_ROOT/reports" failed=()
  check="$(tool_bin sar-check)"
  tool="$(tool_bin scenario-tool)"
  info "$("$check" version)"
  [[ -f "$dir/scenario.json" ]] || die E-SCN-001 "$dir is not installed" "run scripts/$role/60-scenario.sh first"

  step "Layer files against their checksums"
  "$tool" verify --scenario "$dir" || failed+=("verify")

  step "Phase 1 acceptance suite (1-4 minutes)"
  "$check" phase1 --scenario "$dir" --report-dir "$reports" "${quick[@]}" || failed+=("sar-check phase1")

  step "Scorer demo: detections made from the truth with 0.8 m position noise"
  local dets="$SAR_ROOT/data/truth-detections-$id.json"
  "$tool" truth-detections --scenario "$dir" --out "$dets" --noise-m 0.8 --seed 7 \
    && "$tool" score --scenario "$dir" --detections "$dets" | sed 's/^/    /' || failed+=("scorer demo")

  if [[ ${#failed[@]} -gt 0 ]]; then
    die E-TEST-PHASE1 "${#failed[@]} phase 1 test(s) failed on the $role: ${failed[*]}" "each failure above has its own code and hint; paste the sar-check block"
  fi
  echo
  echo "    PHASE 1 ACCEPTED on the $role."
}
