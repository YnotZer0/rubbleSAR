#!/usr/bin/env bash
# Phase 0 - Jetson step 3 of 4: build natively, run unit, zenoh and loopback tests.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/build.sh"

build_and_test
finish "scripts/jetson/30-install.sh --zbook-ip <ZBook Wi-Fi IP address>"
