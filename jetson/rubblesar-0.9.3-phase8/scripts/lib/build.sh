#!/usr/bin/env bash
# Build and test steps shared by scripts/{jetson,zbook}/20-build.sh (requires common.sh).

# run_logged NAME CODE HINT -- command...: run a command, keep its full output in
# reports/setup/NAME.out and, on failure, put what failed into the copy block.
run_logged() {
  local name="$1" code="$2" hint="$3"; shift 4
  local out="$LOG_DIR/${name}-$(date -u +%H%M%S).out"
  info "running: $*"
  if "$@" >"$out" 2>&1; then
    ok "$name passed (full output: $out)"
    return 0
  fi
  # The Go toolchain itself crashing (usually on a damaged entry in its build
  # cache) is not a RubbleSAR failure: clear the cache and run once more.
  if toolchain_crash "$out"; then
    warn "the Go toolchain crashed while building ($(toolchain_crash_line "$out")); this is not a RubbleSAR test failure"
    info "clearing the Go build cache (go clean -cache) and running $name once more; the next build takes a few minutes longer"
    go clean -cache || warn "go clean -cache failed"
    local first="$out"
    out="$LOG_DIR/${name}-$(date -u +%H%M%S)-retry.out"
    info "running again: $*"
    if "$@" >"$out" 2>&1; then
      ok "$name passed on the second run (full output: $out; the crash: $first)"
      return 0
    fi
    if toolchain_crash "$out"; then
      echo "    ---- $name: the Go toolchain crashed again (full output: $out) ----"
      failure_excerpt "$out" >"$out.excerpt" || true
      BLOCK_EXCERPT="$out.excerpt" BLOCK_OUTPUT="$out" die E-BUILD-CACHE "the Go toolchain crashed twice, the second time with an empty build cache" \
        "check free disk space (df -h ~) and the memory (sudo apt install memtester; sudo memtester 2G 1), then run this script again"
    fi
  fi
  echo "    ---- $name FAILED (full output: $out) ----"
  failure_excerpt "$out" >"$out.excerpt" || true
  BLOCK_EXCERPT="$out.excerpt" BLOCK_OUTPUT="$out" die "$code" "$name failed" "$hint (full output: $out)"
}

# toolchain_crash FILE: the Go compiler, vet or linker crashed (a fault, an
# internal error, damaged export data), rather than a test failing. A fault
# counts only with "[build failed]": a crash inside a test binary is a test failure.
TOOLCHAIN_FRAMES_RE='internal compiler error|go/internal/gcimporter|internal/pkgbits|cmd/compile/internal|cmd/link/internal|could not import .*(export data|unexpected EOF)'
TOOLCHAIN_FAULT_RE='fatal error: (fault|unexpected signal)|unexpected fault address'
toolchain_crash() {
  grep -qE "$TOOLCHAIN_FRAMES_RE" "$1" || { grep -qE "$TOOLCHAIN_FAULT_RE" "$1" && grep -qF '[build failed]' "$1"; }
}
toolchain_crash_line() { grep -m1 -oE "$TOOLCHAIN_FAULT_RE|$TOOLCHAIN_FRAMES_RE" "$1" || true; }

# failure_excerpt FILE: the lines that say what failed. A failing Go test is
# shown whole (its log lines and the indented text under them), compile errors
# and panics with their context; passing packages are left out.
failure_excerpt() {
  awk '
    /^ *--- FAIL/ { show = 1; n = 0; print; next }
    show && /^(    |\t)/ { n++; if (n <= 160) print; else if (n == 161) print "        ... (more in the full output)"; next }
    { show = 0 }
    /^panic:/ { pan = 40 }
    pan > 0 { print; pan--; next }
    /^(FAIL|ERROR|E-[A-Z]+-[0-9]+)|\.go:[0-9]+:[0-9]+:|undefined|cannot find|error:|Traceback|Error:/ { print }
  ' "$1" | head -n 400
}

build_and_test() {
  ensure_go_path
  need_cmd go golang
  need_cmd gcc build-essential
  need_cmd pkg-config pkg-config
  export PKG_CONFIG_PATH="/usr/local/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
  export GOFLAGS=-mod=vendor

  step "Toolchain"
  info "$(go version)"
  info "zenohc $(pkg-config --modversion zenohc 2>/dev/null || echo MISSING)"
  pkg-config --exists zenohc || die E-BUS-001 "zenoh-c is not installed" "run scripts/$SCRIPT_DIR_NAME/10-zenoh-c.sh first"
  # Inference back ends (phase 2): ONNX Runtime on both machines, TensorRT on the Jetson.
  info "onnxruntime $(pkg-config --modversion rubblesar-onnxruntime 2>/dev/null || echo MISSING)"
  pkg-config --exists rubblesar-onnxruntime \
    || die E-SETUP-ORT "ONNX Runtime is not installed" "run scripts/$SCRIPT_DIR_NAME/12-onnxruntime.sh first"
  if [[ "$SCRIPT_DIR_NAME" == jetson ]]; then
    if [[ -f /usr/include/aarch64-linux-gnu/NvInfer.h && -f /usr/local/cuda/include/cuda_runtime_api.h ]]; then
      info "tensorrt $(dpkg-query -W -f='${Version}' libnvinfer10 2>/dev/null || echo present)"
    else
      die E-SETUP-TRT "TensorRT or CUDA headers are missing" "run scripts/jetson/75-tensorrt.sh first"
    fi
  fi

  step "Static checks (go vet, gofmt) - advisory"
  local vout="$LOG_DIR/vet-$(date -u +%H%M%S).out"
  if make -C "$SAR_ROOT" vet >"$vout" 2>&1; then
    ok "vet clean"
  else
    sed 's/^/    /' "$vout" | tail -n 20
    warn "go vet/gofmt reported the issues above (newer Go versions add checks); continuing. Please paste them into the chat."
  fi

  step "Building binaries"
  run_logged build E-BUILD-001 "compile error; paste this block into the chat" -- make -C "$SAR_ROOT" build
  ls -l "$SAR_ROOT/bin" | sed 's/^/    /'

  step "Unit tests (no network needed)"
  run_logged unit-tests E-TEST-UNIT "a unit test failed" -- make -C "$SAR_ROOT" test-unit

  step "Inference back-end tests (ONNX Runtime$([[ "$SCRIPT_DIR_NAME" == jetson ]] && echo ", TensorRT"))"
  run_logged infer-tests E-TEST-INFER "an inference back-end test failed" -- make -C "$SAR_ROOT" test-infer

  if [[ "$SCRIPT_DIR_NAME" == zbook ]]; then
    step "Training-script tests (Python, no GPU or Docker needed)"
    run_logged python-tests E-TEST-PYTHON "a training-script test failed" -- make -C "$SAR_ROOT" test-python
  fi

  step "zenoh library tests (local sessions over 127.0.0.1)"
  run_logged zenoh-tests E-TEST-ZENOH "the zenoh library does not work on this machine" -- make -C "$SAR_ROOT" test-zenoh

  step "Loopback self-test with the built sar-check"
  (cd "$SAR_ROOT" && ./bin/sar-check loopback --report-dir reports) || die E-CHK-009 "sar-check loopback failed" "see the table above"
}
