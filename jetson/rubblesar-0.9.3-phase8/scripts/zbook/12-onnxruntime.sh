#!/usr/bin/env bash
# Phase 2 - ZBook: install ONNX Runtime (CPU), used to evaluate the trained model in Go.
# Run before scripts/zbook/20-build.sh, which then builds the programs with it.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/install-onnxruntime.sh"

need_cmd curl curl
need_cmd tar tar
need_cmd pkg-config pkg-config
need_sudo
install_onnxruntime
finish "scripts/zbook/20-build.sh"
