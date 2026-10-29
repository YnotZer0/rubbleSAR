#!/usr/bin/env bash
# Installs Go $GO_VERSION into /usr/local/go unless that exact version is present.
# Sourced by 00-prereqs.sh on both machines (requires common.sh).

install_go() {
  ensure_go_path
  local have=""
  command -v go >/dev/null 2>&1 && have="$(go env GOVERSION 2>/dev/null || true)"
  if [[ "$have" == "go$GO_VERSION" ]]; then
    ok "Go $GO_VERSION already installed ($(command -v go))"
    return 0
  fi
  if [[ -n "$have" && "${SAR_KEEP_GO:-0}" == "1" ]]; then
    warn "keeping existing $have because SAR_KEEP_GO=1"
    return 0
  fi
  local file="go${GO_VERSION}.linux-$(go_arch).tar.gz" sum="${SAR_GO_SHA256:-}"
  if [[ -z "$sum" ]]; then
    info "looking up the official checksum for $file on go.dev"
    sum="$(curl -fsSL --retry 3 'https://go.dev/dl/?mode=json&include=all' | python3 -c '
import json, sys
want = sys.argv[1]
for rel in json.load(sys.stdin):
    for f in rel.get("files", []):
        if f.get("filename") == want:
            print(f["sha256"]); sys.exit(0)
sys.exit(1)' "$file")" || true
    if [[ -z "$sum" && -f "$DOWNLOAD_DIR/$file.sha256" ]]; then
      sum="$(awk '{print $1}' "$DOWNLOAD_DIR/$file.sha256")"
    fi
    [[ -n "$sum" ]] || die E-SETUP-GO "could not find the checksum for $file on go.dev" \
      "check internet access, or set SAR_GO_SHA256=<sha256 from https://go.dev/dl/> and run again"
  fi
  fetch "https://go.dev/dl/$file" "$file" "$sum"
  [[ -n "$have" ]] && info "replacing $have in /usr/local/go"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$DOWNLOAD_DIR/$file"
  echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/rubblesar-go.sh >/dev/null
  export PATH="$PATH:/usr/local/go/bin"
  [[ "$(go env GOVERSION)" == "go$GO_VERSION" ]] || die E-SETUP-GO "Go install did not take effect" "check /usr/local/go and PATH"
  ok "installed $(go version)"
  info "new terminals get Go on PATH automatically; in this one run: export PATH=\$PATH:/usr/local/go/bin"
}
