# Local and untagged builds report a development version. Release builds
# are driven by GoReleaser, which injects the Git tag and build metadata.
.DEFAULT_GOAL := help

GO ?= go
PKG ?= ./cmd/zut
BINARY ?= zut
BIN_DIR ?= bin
DEV_VERSION ?= 0.0.0-dev
VERSION ?= $(DEV_VERSION)
GO_INSTALL_VERSION ?= latest
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: mocks mocks-check fakes-check help build install go-install test test-fast vet lint lint-install fmt-check fmt check run clean release

help:
	@printf '%s\n' \
		'build       build ./bin/zut' \
		'install     install the current checkout with go install' \
		'go-install  install a published module version (GO_INSTALL_VERSION=latest)' \
		'test        run all tests with the race detector' \
		'test-fast   run all tests without the race detector' \
		'vet         run go vet ./...' \
		'lint        run golangci-lint, vet, and verify gofmt' \
		'lint-install install golangci-lint $(GOLANGCI_LINT_VERSION)' \
		'fmt         format Go source files' \
		'mocks       regenerate Mockery v3 test doubles' \
		'mocks-check verify generated test doubles' \
		'fakes-check reject handwritten test doubles in daemon packages' \
		'check       run tests, lint, and test-double checks' \
		'run         build and run ./bin/zut (pass ARGS="...")' \
		'clean       remove build output' \
		'release     cross-build release binaries (set VERSION=...)'

build:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)" $(PKG)

# Install the current checkout. With the default VERSION this is a dev build.
install:
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

# Install a published/module version, rather than the current checkout.
go-install:
	$(GO) install github.com/bnema/zut/cmd/zut@$(GO_INSTALL_VERSION)

test:
	$(GO) test -race ./...

test-fast:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt-check:
	@files="$$(gofmt -l . 2>&1)" || { printf '%s\n' "$$files" >&2; echo "gofmt failed" >&2; exit 1; }; \
	if [ -n "$$files" ]; then printf '%s\n' "$$files" >&2; echo "gofmt issues" >&2; exit 1; fi

lint: vet fmt-check
	$(GOLANGCI_LINT) run ./...

lint-install:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

check: test-fast lint mocks-check fakes-check

fmt:
	gofmt -w .

run: build
	"$(BIN_DIR)/$(BINARY)" $(ARGS)

clean:
	rm -rf "$(BIN_DIR)"

release:
	@if [ "$(VERSION)" = "$(DEV_VERSION)" ] || [ "$(VERSION)" = "0.0.0" ] || printf '%s\n' "$(VERSION)" | grep -Eq '(^|[.-])[0-9]{14}-[A-Za-z0-9]+'; then \
		echo "set VERSION to a release version, for example: make release VERSION=0.1.0" >&2; \
		exit 1; \
	fi
	@mkdir -p "$(BIN_DIR)"
	GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)-linux-amd64" $(PKG)
	GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)-linux-arm64" $(PKG)
	GOOS=darwin  GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)-darwin-amd64" $(PKG)
	GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)-darwin-arm64" $(PKG)
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/$(BINARY)-windows-amd64.exe" $(PKG)

MOCKERY ?= $(HOME)/go/bin/mockery
MOCK_PATHS := packages/agent/session/sessionmocks packages/provider/providermocks ':(glob)packages/agent/session/*_mock_test.go' packages/agent/tools/sessionDirectory_mock_test.go
NEW_PACKAGES := packages/agent/session packages/agent/daemon packages/agent/daemonproto packages/agent/daemonclient packages/agent/internal
# Remove only Mockery output, including mocks of deleted interfaces.
mocks:
	rm -rf packages/agent/session/sessionmocks packages/provider/providermocks
	@for dir in $(NEW_PACKAGES); do if [ -d "$$dir" ]; then find "$$dir" -name '*_mock_test.go' -delete; fi; done
	@if [ "$$($(MOCKERY) version)" = "v3.8.1" ]; then $(MOCKERY); else $(GO) run github.com/vektra/mockery/v3@v3.8.1; fi

mocks-check: mocks
	git diff --exit-code -- $(MOCK_PATHS)
	@test -z "$$(git ls-files --others --exclude-standard -- $(MOCK_PATHS))"

FAKES := ^\s*type\s+\w*(fake|stub|spy|dummy|mock)\w*|^\s*\w*(fake|stub|spy|dummy|mock)\w*(\[[^]]*\])?\s+(struct|interface)\b
fakes-check:
	@dirs=""; for dir in $(NEW_PACKAGES); do if [ -d "$$dir" ]; then dirs="$$dirs $$dir"; fi; done; \
	rc=0; grep -rniE '$(FAKES)' --include='*_test.go' --exclude='*_mock_test.go' $$dirs || rc=$$?; \
	[ $$rc -eq 1 ] || { [ $$rc -eq 0 ] && echo 'handwritten test double: use Mockery (see AGENTS.md)' >&2; exit 1; }
