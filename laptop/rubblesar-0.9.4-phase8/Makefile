# RubbleSAR build and test targets. Run 'make help' for a summary.
#
# Builds use the vendored Go modules (vendor/), so no internet access is needed
# on the Jetson or the ZBook once the source tree is copied over.

SHELL      := /bin/bash
VERSION    := $(shell cat VERSION 2>/dev/null || echo 0.0.0-dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || cat COMMIT 2>/dev/null || echo unknown)
BUILDTIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG        := rubblesar/internal/version
LDFLAGS    := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildTime=$(BUILDTIME)
GOFLAGS    ?= -mod=vendor
export GOFLAGS

BIN        := bin
CGO_TOOLS  := edge-eye sar-check sar-monitor sar-infer casualty-analyst mission-planner rescue-dispatch rubblesar sar-web
PURE_TOOLS := sar-doctor scenario-tool

# Inference back ends are compiled in when their libraries are installed:
#   ort - ONNX Runtime (scripts/*/12-onnxruntime.sh), trt - TensorRT (Jetson, JetPack).
export PKG_CONFIG_PATH := /usr/local/lib/pkgconfig:$(PKG_CONFIG_PATH)
ORT_TAG   := $(shell pkg-config --exists rubblesar-onnxruntime 2>/dev/null && echo ort)
TRT_TAG   := $(shell test -f /usr/include/aarch64-linux-gnu/NvInfer.h -a -f /usr/local/cuda/include/cuda_runtime_api.h && echo trt)
INFER_TAGS ?= $(strip $(ORT_TAG) $(TRT_TAG))

.PHONY: demo-scenario help build doctor test test-unit test-zenoh test-infer test-python test-race vet fmt-check proto clean dist check-env

help:
	@echo "make build       build all binaries into ./bin (needs libzenohc: scripts/*/10-zenoh-c.sh)"
	@echo "make doctor      build only sar-doctor (pure Go, works before zenoh-c is installed)"
	@echo "make test        unit tests + zenoh library tests (needs libzenohc)"
	@echo "make test-unit   unit tests only (no zenoh needed)"
	@echo "make test-infer  ONNX Runtime / TensorRT back-end tests (when installed)"
	@echo "make test-python training-script tests (stand-ins, no GPU)"
	@echo "make demo-scenario  generate + render the demo scenario into data/scenarios/antakya-demo"
	@echo "make test-race   all tests with the race detector"
	@echo "make vet         go vet + gofmt check"
	@echo "make proto       regenerate protobuf code (developers only: needs protoc + protoc-gen-go)"
	@echo "make dist        source tarball in ./dist"

check-env:
	@command -v go >/dev/null || { echo "ERROR E-DOC-001: go not on PATH (export PATH=\$$PATH:/usr/local/go/bin)"; exit 2; }

doctor: check-env
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sar-doctor ./cmd/sar-doctor
	@echo "built $(BIN)/sar-doctor"

build: check-env doctor
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/scenario-tool ./cmd/scenario-tool
	@pkg-config --exists zenohc || { echo "ERROR E-BUS-001: pkg-config cannot find zenohc - run scripts/<machine>/10-zenoh-c.sh first"; exit 2; }
	@for t in $(CGO_TOOLS); do \
		echo "go build $$t"; \
		CGO_ENABLED=1 go build -trimpath -tags "$(INFER_TAGS)" -ldflags "$(LDFLAGS)" -o $(BIN)/$$t ./cmd/$$t || exit 1; \
	done
	@echo "inference back ends: $(if $(INFER_TAGS),$(INFER_TAGS),none (install ONNX Runtime / TensorRT for phase 2))"
	@echo "built: $$(ls $(BIN) | tr '\n' ' ')"
	@$(BIN)/sar-check version

test: test-unit test-zenoh test-infer

test-infer: check-env
	@if [ -n "$(INFER_TAGS)" ]; then \
		echo "go test -tags '$(INFER_TAGS)' ./internal/infer/"; \
		CGO_ENABLED=1 go test -count=1 -tags "$(INFER_TAGS)" ./internal/infer/; \
	else echo "no inference back end installed; skipping back-end tests"; fi

test-python:
	PYTHONDONTWRITEBYTECODE=1 python3 training/tests/test_train.py

test-unit: check-env
	go test -count=1 -timeout 45m -tags nozenoh ./...

test-zenoh: check-env
	@pkg-config --exists zenohc || { echo "ERROR E-BUS-001: zenohc not installed - run scripts/<machine>/10-zenoh-c.sh"; exit 2; }
	CGO_ENABLED=1 go test -count=1 ./internal/bus/zenoh/

test-race: check-env
	CGO_ENABLED=1 go test -count=1 -race -timeout 45m ./...

vet: check-env
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt needed:"; gofmt -l cmd internal; exit 1; }

proto:
	protoc -I proto --go_out=. --go_opt=module=rubblesar proto/sar/v1/*.proto

demo-scenario: check-env
	@mkdir -p $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/scenario-tool ./cmd/scenario-tool
	$(BIN)/scenario-tool generate --config configs/scenario/antakya-demo.toml --out data/scenarios/antakya-demo
	$(BIN)/scenario-tool render --scenario data/scenarios/antakya-demo

dist:
	@big=$$(find . \( -path ./dist -o -path ./bin -o -path ./data -o -path ./reports -o -path ./downloads -o -path ./.git \) -prune -o -type f -size +1M -print); \
	if [ -n "$$big" ]; then echo "refusing to package: files over 1 MB outside bin/ (a stray build?):"; echo "$$big"; exit 1; fi
	@mkdir -p dist
	@if ! git rev-parse --short HEAD >/dev/null 2>&1; then \
		find cmd internal proto configs deploy scripts docs training tools vendor Makefile VERSION VERSIONS.env go.mod go.sum -type f ! -name '*_dev_test.go' ! -path '*/__pycache__/*' ! -name '*.pyc' -print0 \
			| sort -z | xargs -0 sha256sum | sha256sum | cut -c1-10 > COMMIT; \
		echo "source tree id $$(cat COMMIT) (COMMIT)"; fi
	tar --exclude=__pycache__ --exclude="*.pyc" --exclude="*_dev_test.go" --exclude=./dist --exclude=./bin --exclude=./reports --exclude=./downloads --exclude=./data --exclude=.git -czf dist/rubblesar-$(VERSION).tar.gz --transform 's,^\.,rubblesar-$(VERSION),' .
	@echo "dist/rubblesar-$(VERSION).tar.gz"

clean:
	rm -rf $(BIN) dist
