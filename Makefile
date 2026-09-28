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
#
# To drive the CLI by hand against a local Payload:
#
#   make dev                      # build, log a sandbox profile in, check it
#   make run ARGS='get posts -l3' # run this build, never touching ~/.config/pay
#   make dev-shell                # a shell where `pay` IS this build
#
# Those need PAY_TEST_BASE_URL and PAY_TEST_API_KEY. Export them, or write a
# .env.local (gitignored) so every shell gets them:
#
#   PAY_TEST_BASE_URL ?= http://localhost:3900
#   PAY_TEST_API_KEY  ?= 1a2b3c...
#
# `?=` is the point: it lets a variable already exported in your shell win over
# the file, so a one-off `PAY_TEST_BASE_URL=... make test-live` still works.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

-include .env.local
export PAY_TEST_BASE_URL PAY_TEST_API_KEY PAY_TEST_EMAIL PAY_TEST_PASSWORD PAY_TEST_USERNAME

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

# Hand-runs need the opposite of TESTHOME: a home that PERSISTS, so `make
# dev-login` once is enough, but that is still not the developer's real one.
# .pay-test/ is gitignored; `make dev-reset` throws it away.
DEVHOME = $(CURDIR)/.pay-test
DEVENV  = PAY_HOME=$(DEVHOME) PAY_NO_UPDATE=1

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
# Run it locally
#
# Every target here runs ./pay with PAY_HOME=$(DEVHOME), so driving the CLI by
# hand can neither read your real credentials nor rewrite your real config —
# the same guarantee the test targets give, minus the mktemp.
# ---------------------------------------------------------------------------

.PHONY: dev
dev: ## one-stop local setup: build, log the sandbox profile in, check it
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory dev-login
	@$(MAKE) --no-print-directory dev-doctor

.PHONY: run
run: build ## run this build in the sandbox: make run ARGS='get posts --limit 3'
	@$(DEVENV) ./pay $(ARGS)

.PHONY: dev-login
dev-login: build require-live-env ## create the sandbox 'local' profile from PAY_TEST_*
	@mkdir -p $(DEVHOME)
	@printf '%s' "$$PAY_TEST_API_KEY" | $(DEVENV) ./pay auth login \
	  --profile local --base-url "$$PAY_TEST_BASE_URL" --api-key-stdin
	@echo "profile 'local' now lives in $(DEVHOME) — try: make run ARGS=collections"

.PHONY: dev-doctor
dev-doctor: build ## pay doctor, against the sandbox home
	@$(DEVENV) ./pay doctor

.PHONY: dev-shell
dev-shell: build ## a subshell where `pay` is this build and PAY_HOME is the sandbox
	@echo "pay -> $(CURDIR)/pay, PAY_HOME=$(DEVHOME). exit to leave."
	@mkdir -p $(DEVHOME)
	@$(DEVENV) PATH="$(CURDIR):$$PATH" $${SHELL:-/bin/bash}

.PHONY: dev-reset
dev-reset: ## throw the sandbox home away (profiles, cache, audit log)
	rm -rf $(DEVHOME)

# ---------------------------------------------------------------------------
# The guard every live target shares.
#
# One copy: four near-identical error messages drift, and the version that
# forgets to mention PAY_TEST_API_KEY costs someone a second run.
# ---------------------------------------------------------------------------
.PHONY: require-live-env
require-live-env:
	@[ -n "$${PAY_TEST_BASE_URL:-}" ] && [ -n "$${PAY_TEST_API_KEY:-}" ] || { \
	  echo 'error: this target needs a real Payload instance.'; \
	  echo '  export PAY_TEST_BASE_URL=http://localhost:3900'; \
	  echo '  export PAY_TEST_API_KEY=<a users API key>'; \
	  echo '  (or put both in .env.local — see the top of this Makefile)'; \
	  exit 2; }

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
lint: vet ## staticcheck + golangci-lint + the §3.1 architectural rules
	go tool staticcheck ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint config verify && golangci-lint run ./...; \
	 else echo "note: golangci-lint is not installed; CI runs it. go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; fi
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

.PHONY: test-focus
test-focus: ## one test, verbose: make test-focus RUN=TestGet [PKG=./internal/cli]
	@[ -n "$(RUN)" ] || { echo 'error: set RUN, e.g. make test-focus RUN=TestGet'; exit 2; }
	$(TESTHOME) go test -count=1 -v -run '$(RUN)' $(if $(PKG),$(PKG),./...)

.PHONY: test-cover
test-cover: ## unit tests with a coverage summary
	$(TESTHOME) go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 30

.PHONY: bench
bench: ## run benchmarks, including the §8.2 warm-path budget
	$(TESTHOME) go test -run '^$$' -bench . -benchmem ./...

.PHONY: test-live
test-live: require-live-env ## integration tests against a real Payload instance
	$(TESTHOME) go test -tags live -count=1 -race \
	  ./internal/payload/... ./internal/discovery/... ./internal/cli/...

.PHONY: test-contract
test-contract: require-live-env ## the §21 portability contract, against a real instance
	$(TESTHOME) go test -tags live -run TestContract -count=1 ./internal/payloadtest/...

.PHONY: test-all
test-all: test test-live test-contract ## unit + live + contract

# ---------------------------------------------------------------------------
# Fixtures and golden files
# ---------------------------------------------------------------------------

.PHONY: fixtures
fixtures: require-live-env ## re-record testdata/fixtures from a live instance (redacted)
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
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
