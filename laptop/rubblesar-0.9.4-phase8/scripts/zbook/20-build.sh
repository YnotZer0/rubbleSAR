#!/usr/bin/env bash
# Phase 0 - ZBook step 4 of 5: build, run unit, zenoh and loopback tests.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/build.sh"

build_and_test
finish "scripts/zbook/30-install.sh"
