#!/usr/bin/env bash
# Phase 1 - ZBook acceptance: scenario, imagery layers, virtual cameras and scorer.
#
#   scripts/zbook/70-phase1-test.sh [--quick]     (--quick: every third victim plus all moving ones)
#
# Needs scripts/zbook/60-scenario.sh first. No network or other machine needed.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
source "$SAR_ROOT/scripts/lib/phase1-test.sh"

QUICK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) QUICK=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--quick]" ;;
  esac
done
phase1_test zbook
finish "the same on the Jetson (scripts/jetson/60-scenario.sh, then 70-phase1-test.sh); then share both RESULT lines"
