.PHONY: build build-all test lint fmt vet run dev clean release

GO        ?= go
BIN_DIR   ?= ./bin
PKG       ?= ./...
CONFIG    ?= ./wgserver.example.yaml

# Override on the command line or via env to embed a version.
#   make build VERSION=v0.1.0
#   make release VERSION=v0.1.0
VERSION   ?= dev
LDFLAGS   := -s -w -X main.version=$(VERSION)

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver ./cmd/wgserver
	$(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver-updater ./cmd/wgserver-updater

# Cross-compile for all supported targets. Output goes to bin/.
build-all:
	mkdir -p $(BIN_DIR)
	@for target in linux-amd64 linux-arm64; do \
	  os=$${target%%-*}; arch=$${target##*-}; \
	  echo "==> $$os/$$arch"; \
	  $(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver-$$target ./cmd/wgserver || exit 1; \
	  $(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver-updater-$$target ./cmd/wgserver-updater || exit 1; \
	  sha256sum $(BIN_DIR)/wgserver-$$target $(BIN_DIR)/wgserver-updater-$$target > $(BIN_DIR)/SHA256SUMS-$$target; \
	done

test:
	$(GO) test $(PKG)

fmt:
	$(GO) fmt $(PKG)

vet:
	$(GO) vet $(PKG)

golangci-lint:
	@command -v golangci-lint >/dev/null || (echo "golangci-lint not installed; run: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run $(PKG)

# Default lint target: gofmt + go vet (always available). Run
# `make golangci-lint` separately to invoke the full lint suite.
lint: fmt vet

run: build
	$(BIN_DIR)/wgserver -config $(CONFIG)

dev:
	WG_ENV=dev $(GO) run -ldflags='$(LDFLAGS)' ./cmd/wgserver -config $(CONFIG)

# Cut a GitHub release. Requires VERSION=vX.Y.Z and `gh auth login`.
# Builds for the current GOOS/GOARCH (use GOOS=linux GOARCH=amd64 make
# release for a Linux release from a macOS dev box), uploads the binary
# and its .sha256 sidecar so wgserver-updater can verify.
release:
	@test -n "$(VERSION)" || (echo "VERSION is required, e.g. make release VERSION=v0.1.0" && exit 1)
	@command -v gh >/dev/null || (echo "gh CLI is required" && exit 1)
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver-$$GOOS-$$GOARCH ./cmd/wgserver
	$(GO) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/wgserver-updater-$$GOOS-$$GOARCH ./cmd/wgserver-updater
	sha256sum $(BIN_DIR)/wgserver-$$GOOS-$$GOARCH | awk '{print $$1}' > $(BIN_DIR)/wgserver-$$GOOS-$$GOARCH.sha256
	gh release create $(VERSION) \
	  $(BIN_DIR)/wgserver-$$GOOS-$$GOARCH \
	  $(BIN_DIR)/wgserver-$$GOOS-$$GOARCH.sha256 \
	  $(BIN_DIR)/wgserver-updater-$$GOOS-$$GOARCH \
	  --title "$(VERSION)" \
	  --generate-notes

clean:
	rm -rf $(BIN_DIR)
