#!/usr/bin/env bash
# Phase 1 - Jetson acceptance: scenario, imagery layers, virtual cameras and scorer.
#
#   scripts/jetson/70-phase1-test.sh [--quick]     (--quick: every third victim plus all moving ones)
#
# Needs scripts/jetson/60-scenario.sh first. No network or other machine needed.
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
phase1_test jetson
finish "share the RESULT lines from both machines; phase 2 follows"
