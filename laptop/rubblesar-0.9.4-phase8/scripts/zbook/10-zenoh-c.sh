#!/usr/bin/env bash
# Phase 0 - ZBook step 2 of 5: install the zenoh-c 1.9.0 library (x86_64) into /usr/local.
source "$(dirname "$0")/../lib/common.sh"
source "$SAR_ROOT/scripts/lib/install-zenoh-c.sh"

need_cmd unzip unzip
need_cmd curl curl
need_cmd pkg-config pkg-config
need_sudo
install_zenoh_c
finish "scripts/zbook/15-zenohd.sh"
