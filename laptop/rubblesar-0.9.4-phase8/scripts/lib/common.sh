#!/usr/bin/env bash
# Shared helpers for every RubbleSAR setup script.
#
# - Everything a script prints is also written to reports/setup/<script>-<time>.log
# - Any failing command stops the script and prints a framed block that can be
#   pasted into the chat as-is: script, step, command, line, exit code, system
#   facts and the last lines of the log.

set -Eeuo pipefail

SAR_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../VERSIONS.env
source "$SAR_ROOT/VERSIONS.env"
SAR_VERSION="$(cat "$SAR_ROOT/VERSION" 2>/dev/null || echo unknown)"

SCRIPT_NAME="$(basename "$0" .sh)"
SCRIPT_DIR_NAME="$(basename "$(cd "$(dirname "$0")" && pwd)")"
LOG_DIR="${SAR_LOG_DIR:-$SAR_ROOT/reports/setup}"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/${SCRIPT_DIR_NAME}-${SCRIPT_NAME}-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$LOG_FILE") 2>&1

CURRENT_STEP="start"
DOWNLOAD_DIR="${SAR_DOWNLOAD_DIR:-$SAR_ROOT/downloads}"

# resolve_path PATH: an absolute path for a file or folder given on the command
# line. Relative paths are tried from the current folder first, then from the
# release folder, so 'data/datasets/x' works from anywhere (e.g. from scripts/zbook).
resolve_path() {
  local p="$1"
  [[ -z "$p" ]] && return 0
  [[ "$p" == "~/"* ]] && p="$HOME/${p#\~/}"
  if [[ "$p" == /* ]]; then echo "$p"
  elif [[ -e "$p" ]]; then echo "$(cd "$(dirname "$p")" && pwd)/$(basename "$p")"
  elif [[ -e "$SAR_ROOT/$p" ]]; then echo "$SAR_ROOT/$p"
  else echo "$PWD/$p"
  fi
}

# need_data_dir: datasets, training runs and overlays live in <release>/data.
# A new release folder starts without one; if an earlier release next to it
# has one, stop and say how to share it (nothing is moved automatically).
need_data_dir() {
  [[ -e "$SAR_ROOT/data" ]] && return 0
  local d prev="" best=""
  for d in "$(dirname "$SAR_ROOT")"/rubblesar-*/data; do
    [[ -d "$d" && "$(cd "$d/.." && pwd)" != "$SAR_ROOT" ]] || continue
    prev="$(cd "$d" && pwd)"
    [[ -d "$d/datasets" || -d "$d/runs" ]] && best="$prev"   # prefer the one with training data
  done
  [[ -n "$best" ]] && prev="$best"
  [[ -z "$prev" ]] && return 0
  die E-SETUP-DATA "this release folder has no data/ folder, but $prev exists (datasets and training runs of an earlier release)" \
    "share it with this release (safe while training runs): ln -s $prev $SAR_ROOT/data   then run this command again"
}

step() { CURRENT_STEP="$*"; printf '\n==> %s\n' "$*"; }
ok()   { printf '    ok: %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    WARNING: %s\n' "$*"; }

system_facts() {
  local osname arch
  osname="$(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-unknown}")"
  arch="$(uname -m)"
  echo "host:    $(hostname) ($osname, $arch, kernel $(uname -r))"
  [[ -f /etc/nv_tegra_release ]] && echo "jetson:  $(head -n1 /etc/nv_tegra_release)"
  echo "user:    $(id -un) (uid $(id -u))"
  echo "go:      $(command -v go >/dev/null && go version || echo 'not on PATH')"
  echo "zenohc:  $(pkg-config --modversion zenohc 2>/dev/null || echo 'not installed')"
}

print_block() { # $1 = code, $2 = message, $3 = failing command (optional)
  local code="$1" msg="$2" cmd="${3:-}" recent
  # Read the log tail first: the block itself is about to be appended to the log.
  sleep 0.2
  recent="$(tail -n 40 "$LOG_FILE" 2>/dev/null || true)"
  {
    echo
    echo "=============== COPY FROM HERE: RubbleSAR setup error ==============="
    echo "script:  scripts/${SCRIPT_DIR_NAME}/${SCRIPT_NAME}.sh (RubbleSAR $SAR_VERSION)"
    echo "time:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    system_facts
    echo "step:    $CURRENT_STEP"
    echo "error:   $code: $msg"
    [[ -n "$cmd" ]] && echo "command: $cmd"
    echo "log:     $LOG_FILE"
    if [[ -n "${BLOCK_EXCERPT:-}" && -s "${BLOCK_EXCERPT:-}" ]]; then
      echo "--- what failed (from ${BLOCK_OUTPUT:-the output}) ---"
      cat "$BLOCK_EXCERPT"
      if [[ -n "${BLOCK_OUTPUT:-}" ]]; then
        echo "--- last 10 lines of the output ---"
        tail -n 10 "$BLOCK_OUTPUT"
      fi
    elif [[ -n "${BLOCK_OUTPUT:-}" && -s "${BLOCK_OUTPUT:-}" ]]; then
      echo "--- last 40 lines of ${BLOCK_OUTPUT} ---"
      tail -n 40 "$BLOCK_OUTPUT"
    else
      echo "--- last 40 log lines ---"
      printf '%s\n' "$recent"
    fi
    echo "================ COPY TO HERE ======================================="
    echo "Paste everything between the two lines above into the chat to get help."
  } >&2
}

on_err() {
  local rc=$1 line=$2 cmd=$3
  trap - ERR
  # Inside $(...) or a pipeline: leave the report to the script itself, which
  # sees the failure next (one block instead of two, one inside the other).
  if (( BASH_SUBSHELL > 0 )); then exit "$rc"; fi
  print_block "E-SETUP-$rc" "command failed with exit code $rc at line $line" "$cmd"
  exit "$rc"
}
trap 'on_err $? $LINENO "$BASH_COMMAND"' ERR

# die CODE MESSAGE [HINT]: stop with a coded error and a hint.
die() {
  local code="$1" msg="$2" hint="${3:-}"
  trap - ERR
  printf '\nERROR %s: %s\n' "$code" "$msg" >&2
  [[ -n "$hint" ]] && printf 'hint:  %s\n' "$hint" >&2
  print_block "$code" "$msg${hint:+ (hint: $hint)}"
  exit 1
}

need_sudo() {
  if [[ $EUID -eq 0 ]]; then
    die E-SETUP-ROOT "run this script as your normal user, not with sudo" "it calls sudo itself where needed"
  fi
  step "Checking sudo access (you may be asked for your password)"
  sudo -v || die E-SETUP-SUDO "sudo is required" "ask an administrator to add $(id -un) to the sudo group"
  ok "sudo available"
}

need_cmd() { # need_cmd cmd "apt package"
  command -v "$1" >/dev/null 2>&1 || die E-SETUP-CMD "'$1' is not installed" "sudo apt install ${2:-$1} (or run 00-prereqs.sh)"
}

arch_name() { # x86_64 | aarch64
  case "$(uname -m)" in
    x86_64|amd64) echo x86_64 ;;
    aarch64|arm64) echo aarch64 ;;
    *) die E-SETUP-ARCH "unsupported CPU architecture $(uname -m)" ;;
  esac
}

go_arch() { case "$(arch_name)" in x86_64) echo amd64 ;; aarch64) echo arm64 ;; esac; }

sha256_of() { sha256sum "$1" | awk '{print $1}'; }

# fetch URL FILE SHA256: download into DOWNLOAD_DIR unless a verified copy exists.
fetch() {
  local url="$1" file="$2" want="$3" dest="$DOWNLOAD_DIR/$2"
  mkdir -p "$DOWNLOAD_DIR"
  if [[ -f "$dest" ]] && [[ -z "$want" || "$(sha256_of "$dest")" == "$want" ]]; then
    ok "using cached $dest"
  else
    info "downloading $url"
    curl -fL --retry 3 --connect-timeout 20 -o "$dest.part" "$url" \
      || die E-SETUP-NET "download failed: $url" "check internet access, or run scripts/fetch-downloads.sh on a connected machine and copy the downloads/ folder here"
    mv "$dest.part" "$dest"
  fi
  if [[ -n "$want" ]]; then
    local got; got="$(sha256_of "$dest")"
    [[ "$got" == "$want" ]] || { rm -f "$dest"; die E-SETUP-SHA "checksum mismatch for $file (got $got, want $want)" "the download was corrupted or tampered with; run the script again"; }
    ok "checksum verified for $file"
  fi
}

ensure_go_path() {
  if ! command -v go >/dev/null 2>&1 && [[ -x /usr/local/go/bin/go ]]; then
    export PATH="$PATH:/usr/local/go/bin"
  fi
}

finish() {
  trap - ERR
  printf '\nDONE: %s completed successfully.\n' "scripts/${SCRIPT_DIR_NAME}/${SCRIPT_NAME}.sh"
  printf 'Log: %s\n' "$LOG_FILE"
  [[ -n "${1:-}" ]] && printf 'Next: %s\n' "$1"
  return 0
}
