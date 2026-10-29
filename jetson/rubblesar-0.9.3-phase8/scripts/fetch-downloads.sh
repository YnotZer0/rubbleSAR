#!/usr/bin/env bash
# Pre-downloads every archive the setup scripts need, for BOTH machines, into
# ./downloads. Run it on a machine with internet access (e.g. the ZBook) and
# copy the whole repository (including downloads/) to a Jetson that has none.
# Checksums are verified exactly as the setup scripts do.
source "$(dirname "$0")/lib/common.sh"

need_cmd curl curl
need_cmd python3 python3

step "zenoh-c $ZENOH_VERSION (x86_64 and aarch64)"
for a in x86_64 aarch64; do
  var="ZENOH_C_SHA256_${a^^}"
  f="zenoh-c-${ZENOH_VERSION}-${a}-unknown-linux-gnu-standalone.zip"
  fetch "https://github.com/eclipse-zenoh/zenoh-c/releases/download/${ZENOH_VERSION}/$f" "$f" "${!var}"
done

step "zenohd $ZENOH_VERSION (x86_64)"
f="zenoh-${ZENOH_VERSION}-x86_64-unknown-linux-gnu-standalone.zip"
fetch "https://github.com/eclipse-zenoh/zenoh/releases/download/${ZENOH_VERSION}/$f" "$f" "$ZENOHD_SHA256_X86_64"

step "ONNX Runtime $ONNXRUNTIME_VERSION (x86_64 and aarch64)"
for a in x86_64 aarch64; do
  var="ONNXRUNTIME_SHA256_${a^^}"
  o=$([[ $a == x86_64 ]] && echo x64 || echo aarch64)
  f="onnxruntime-linux-${o}-${ONNXRUNTIME_VERSION}.tgz"
  fetch "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/$f" "$f" "${!var}"
done

step "Go $GO_VERSION (amd64 and arm64)"
json="$(curl -fsSL --retry 3 'https://go.dev/dl/?mode=json&include=all')" || die E-SETUP-NET "cannot reach go.dev"
for a in amd64 arm64; do
  f="go${GO_VERSION}.linux-${a}.tar.gz"
  sum="$(python3 -c '
import json, sys
for rel in json.loads(sys.stdin.read()):
    for x in rel.get("files", []):
        if x.get("filename") == sys.argv[1]:
            print(x["sha256"]); sys.exit(0)
sys.exit(1)' "$f" <<<"$json")" || die E-SETUP-GO "no checksum for $f on go.dev"
  fetch "https://go.dev/dl/$f" "$f" "$sum"
  echo "$sum  $f" > "$DOWNLOAD_DIR/$f.sha256"
done

ls -l "$DOWNLOAD_DIR" | sed 's/^/    /'
finish "copy the repository, including downloads/, to the Jetson"
