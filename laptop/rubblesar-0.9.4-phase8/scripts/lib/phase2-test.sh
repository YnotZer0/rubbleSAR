#!/usr/bin/env bash
# Phase 2 acceptance steps shared by scripts/{zbook,jetson}/90-phase2-test.sh
# (requires common.sh, scenario.sh and models.sh). $1 = role (zbook|jetson).

phase2_test() {
  local role="$1" quick=() failed=() reports="$SAR_ROOT/reports"
  [[ "${QUICK:-0}" -eq 1 ]] && quick=(--quick)
  local check infer model="$MODEL_ROOT/current"
  check="$(tool_bin sar-check)"
  infer="$(tool_bin sar-infer)"
  info "$("$check" version)"
  [[ -e "$model/model.json" ]] || die E-SETUP-MODEL "no model installed at $model" \
    "$([[ $role == zbook ]] && echo 'run scripts/zbook/82-evaluate.sh first' || echo 'run scripts/jetson/80-engine.sh --from <model folder> first')"

  step "Phase 2 acceptance suite ($role, 2-8 minutes)"
  "$check" phase2 --role "$role" --model "$model" --report-dir "$reports" "${quick[@]}" || failed+=("sar-check phase2")

  step "Speed of whole 1080p frames"
  "$infer" bench --model "$model" --frames 10 | tail -n 2 | sed 's/^/    /' || failed+=("bench")

  if [[ ${#failed[@]} -gt 0 ]]; then
    die E-TEST-PHASE2 "${#failed[@]} phase 2 step(s) failed on the $role: ${failed[*]}" "each failure above has its own code and hint; paste the sar-check block"
  fi
  echo
  echo "    PHASE 2 checks passed on the $role (warnings, if any, are listed above)."
}
