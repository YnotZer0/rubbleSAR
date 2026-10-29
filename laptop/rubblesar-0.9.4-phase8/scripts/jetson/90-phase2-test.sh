#!/usr/bin/env bash
# Phase 2 - Jetson acceptance: the body-part model (TensorRT): engine, agreement with the ZBook, accuracy, speed.
#
#   scripts/jetson/90-phase2-test.sh [--quick]      (--quick: every third held-out frame)
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/scenario.sh"
source "$SAR_ROOT/scripts/lib/models.sh"
source "$SAR_ROOT/scripts/lib/phase2-test.sh"

QUICK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) QUICK=1; shift ;;
    *) die E-SETUP-ARG "unknown option $1" "usage: $0 [--quick]" ;;
  esac
done
phase2_test jetson
finish "share the RESULT lines from both machines"
