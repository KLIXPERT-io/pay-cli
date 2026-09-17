# PayCLI — developer targets.
#
# `make check` is exactly what CI runs. Everything else is a shortcut into it.
#
# Conventions:
#   * every test target runs with PAY_HOME pointed at a throwaway directory, so
#     a test can never read or write the developer's real config, cache or
#     credentials;
#   * PAY_NO_UPDATE=1 is set everywhere, so no test can reach the release API;
#   * integration targets FAIL with exit 2 when their environment is missing,
#     rather than skipping. The usual failure mode of integration tests is that
#     they skip forever while everyone believes they ran.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

VERSION     := $(shell cat VERSION 2>/dev/null || echo 0.0.0)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        := $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
GO_VERSION  := $(shell cat .go-version 2>/dev/null || echo 1.27.1)

MODULE  := github.com/KLIXPERT-io/pay-cli
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

# A throwaway home for every test invocation. Recomputed per recipe line, which
# is what we want: no two targets share state.
TESTHOME = PAY_HOME=$$(mktemp -d) PAY_NO_UPDATE=1

# ---------------------------------------------------------------------------
# Toolchain guard.
#
# go.mod declares the LANGUAGE minimum (1.26.0), not a patch pin, so a distro
# toolchain is acceptable — but only when GOTOOLCHAIN is allowed to fetch the
# version .go-version asks for. With GOTOOLCHAIN=local and an older system Go,
# every target below fails with an error about language features rather than
# about the toolchain, which costs an hour the first time.
# ---------------------------------------------------------------------------
.PHONY: toolchain
toolchain:
	@go_tc="$$(go env GOTOOLCHAIN 2>/dev/null || echo local)"; \
	sys="$$(go env GOVERSION 2>/dev/null | sed 's/^go//')"; \
	if [ "$$go_tc" = "local" ]; then \
	  want=1.26; \
	  have_major=$$(printf '%s' "$$sys" | cut -d. -f1); \
	  have_minor=$$(printf '%s' "$$sys" | cut -d. -f2); \
	  if [ "$$have_major" -lt 1 ] || { [ "$$have_major" -eq 1 ] && [ "$$have_minor" -lt 26 ]; }; then \
	    echo "error: GOTOOLCHAIN=local and the system Go is $$sys, but this module needs >= $$want."; \
	    echo "       Fix it with one of:"; \
	    echo "         export GOTOOLCHAIN=auto        # let Go fetch $(GO_VERSION) transparently"; \
	    echo "         GOTOOLCHAIN=go$(GO_VERSION) make <target>"; \
	    echo "         install Go $(GO_VERSION) (see .go-version)"; \
	    exit 2; \
	  fi; \
	fi

# ---------------------------------------------------------------------------
# The CI gate
# ---------------------------------------------------------------------------

.PHONY: check
check: fmt-check vet lint test ## everything CI runs

.PHONY: ci
ci: check tidy-check release-check ## check + the release config and go.mod hygiene

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build: toolchain ## build ./pay for this platform
	go build -trimpath -ldflags '$(LDFLAGS)' -o pay ./cmd/pay

.PHONY: install
install: toolchain ## go install the pay binary into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/pay

# ---------------------------------------------------------------------------
# Format, vet, lint
# ---------------------------------------------------------------------------

.PHONY: fmt
fmt: ## gofmt + goimports, in place
	gofmt -w .
	@if go tool goimports --help >/dev/null 2>&1; then go tool goimports -w .; \
	 else echo "note: go tool goimports is unavailable; gofmt only"; fi

.PHONY: fmt-check
fmt-check: ## fail when anything is unformatted
	@out="$$(gofmt -l . | grep -v '^$$' || true)"; \
	if [ -n "$$out" ]; then \
	  echo "error: these files are not gofmt-clean:"; echo "$$out"; \
	  echo "run: make fmt"; exit 1; \
	fi

.PHONY: vet
vet: toolchain ## go vet
	go vet ./...

.PHONY: lint
lint: vet ## staticcheck + the §3.1 architectural rules
	@if go tool staticcheck --version >/dev/null 2>&1; then go tool staticcheck ./...; \
	 else echo "note: go tool staticcheck is unavailable; skipping"; fi
	./scripts/arch-lint.sh

.PHONY: arch-lint
arch-lint: ## only the §3.1 architectural rules
	./scripts/arch-lint.sh

.PHONY: tidy-check
tidy-check: ## fail when go.mod/go.sum are not tidy
	go mod tidy
	git diff --exit-code go.mod go.sum

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

.PHONY: test
test: toolchain ## unit tests, race + shuffled, offline
	$(TESTHOME) go test -race -shuffle=on -count=1 ./...

.PHONY: test-short
test-short: ## unit tests without -race, for a fast inner loop
	$(TESTHOME) go test -count=1 ./...

.PHONY: test-cover
test-cover: ## unit tests with a coverage summary
	$(TESTHOME) go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 30

.PHONY: bench
bench: ## run benchmarks, including the §8.2 warm-path budget
	$(TESTHOME) go test -run '^$$' -bench . -benchmem ./...

.PHONY: test-live
test-live: ## integration tests against a real Payload instance
	@[ -n "$${PAY_TEST_BASE_URL:-}" ] || { \
	  echo 'error: set PAY_TEST_BASE_URL and PAY_TEST_API_KEY.'; \
	  echo '  export PAY_TEST_BASE_URL=http://localhost:3900'; \
	  echo '  export PAY_TEST_API_KEY=<a users API key>'; \
	  exit 2; }
	@[ -n "$${PAY_TEST_API_KEY:-}" ] || { echo 'error: set PAY_TEST_API_KEY.'; exit 2; }
	$(TESTHOME) go test -tags live -count=1 -race \
	  ./internal/payload/... ./internal/discovery/... ./internal/cli/...

.PHONY: test-contract
test-contract: ## the §21 portability contract, against a real instance
	@[ -n "$${PAY_TEST_BASE_URL:-}" ] || { echo 'error: set PAY_TEST_BASE_URL and PAY_TEST_API_KEY.'; exit 2; }
	$(TESTHOME) go test -tags live -run TestContract -count=1 ./internal/payloadtest/...

.PHONY: test-all
test-all: test test-live test-contract ## unit + live + contract

# ---------------------------------------------------------------------------
# Fixtures and golden files
# ---------------------------------------------------------------------------

.PHONY: fixtures
fixtures: ## re-record testdata/fixtures from a live instance (redacted)
	@[ -n "$${PAY_TEST_BASE_URL:-}" ] || { echo 'error: set PAY_TEST_BASE_URL and PAY_TEST_API_KEY.'; exit 2; }
	go run ./tools/recordfixtures \
	  -base "$$PAY_TEST_BASE_URL" -key "$$PAY_TEST_API_KEY" -out testdata/fixtures

.PHONY: golden
golden: ## regenerate testdata/golden and show what moved
	$(TESTHOME) UPDATE_GOLDEN=1 go test -count=1 ./...
	git diff --stat testdata/golden

# ---------------------------------------------------------------------------
# Release
# ---------------------------------------------------------------------------

.PHONY: snapshot
snapshot: ## build release artefacts locally, without publishing
	goreleaser release --snapshot --clean --skip=publish,sign,sbom

.PHONY: release-check
release-check: ## validate .goreleaser.yaml
	goreleaser check

.PHONY: version
version: ## print the version this build would stamp
	@echo "version=$(VERSION) commit=$(COMMIT) date=$(DATE) go=$(GO_VERSION)"

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## remove build and coverage artefacts
	rm -rf pay pay.exe dist coverage.out

.PHONY: help
help: ## list the targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
