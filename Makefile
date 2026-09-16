BINARY    := bffs
INSTALL_PATH ?= /opt/bffs

# ── Tuning (override: make JOBS=16 GO_BUILD_FLAGS="-v" build) ────
JOBS           ?= $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
GO_P           ?= $(JOBS)
GO_BUILD_FLAGS ?=

# Stamp the version the way a release does, so a local install reports
# what it is: 0.3.0 at the tag, 0.3.0-5-gabc1234 after it, -dirty with
# uncommitted changes. Outside a git checkout this is empty and the
# binary falls back to what the build info knows (see cmd/root.go).
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS  := $(if $(VERSION),-ldflags "-X github.com/jratienza65/bffs/cmd.Version=$(VERSION)")

## build: build the binary into the repo root
build:
	go build -p $(GO_P) $(GO_BUILD_FLAGS) $(LDFLAGS) -o $(BINARY) .

# The rm before cp is load-bearing on macOS: cp onto an existing file rewrites
# the same inode, which invalidates the kernel's cached code signature for the
# binary — every subsequent exec is then SIGKILLed ("zsh: killed") even though
# `codesign -v` reports the file as valid. Unlinking first gives the copy a
# fresh inode and a clean signature registration.
## install: build, then install to $(INSTALL_PATH) (sudo for /opt/bffs)
install: build
	@echo "  >  Installing $(BINARY) to $(INSTALL_PATH)"
	@if [ "$(INSTALL_PATH)" = "/opt/bffs" ]; then \
		sudo mkdir -p $(INSTALL_PATH); \
		sudo rm -f $(INSTALL_PATH)/$(BINARY); \
		sudo cp $(BINARY) $(INSTALL_PATH)/$(BINARY); \
		sudo chmod +x $(INSTALL_PATH)/$(BINARY); \
	else \
		mkdir -p $(INSTALL_PATH); \
		rm -f $(INSTALL_PATH)/$(BINARY); \
		cp $(BINARY) $(INSTALL_PATH)/$(BINARY); \
		chmod +x $(INSTALL_PATH)/$(BINARY); \
	fi;
	@echo "  >  $(BINARY) installed successfully!"
	@echo "  >  IMPORTANT: Please ensure $(INSTALL_PATH) is in your PATH."
	@echo "  >  Example: export PATH=\$$PATH:$(INSTALL_PATH)"

# ── Release (goreleaser) ─────────────────────────────────────────
# Uses `go run` so there is nothing to install; drop the `go run ...@latest`
# prefix if you have goreleaser on PATH.
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@latest

.PHONY: build install release-check snapshot clean-dist hooks fmt lint lint-go test test-race cover golden check tools help fuzz skill-validate bench-shim

## release-check: validate .goreleaser.yaml
release-check:
	$(GORELEASER) check

# Full cross-platform build into ./dist without touching GitHub.
## snapshot: cross-build every target into ./dist without publishing
snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

## clean-dist: remove ./dist
clean-dist:
	rm -rf dist

# ── Contributor tooling ──────────────────────────────────────────
# Git will not run hooks from a fresh clone on its own (by design), so this
# is opt-in per checkout. Run it once after cloning.
## hooks: enable the pre-commit hook for this checkout
hooks:
	@git config core.hooksPath .githooks
	@echo "  >  pre-commit hook enabled (gofmt + go vet, mirrors CI)"
	@echo "  >  bypass once with: git commit --no-verify"

## fmt: format the tree in place
fmt:
	gofmt -w .

# Fuzz the bundle reader and the entry-name grammar (CI runs the same two
# targets for 20 s each as a smoke test). Override: make FUZZTIME=5m fuzz
# Minimization is off (-fuzzminimizetime 0): with the default 60 s per new
# interesting input the workers stop mutating within seconds (FuzzUnpack
# stages files on disk per exec); inputs are a few KiB, so an unminimized
# crasher reproduces just as well.
FUZZTIME ?= 30s

## fuzz: fuzz the bundle reader and the entry-name grammar (FUZZTIME=5m)
fuzz:
	go test ./internal/bundle -run '^$$' -fuzz FuzzUnpack -fuzztime $(FUZZTIME) -fuzzminimizetime 0
	go test ./internal/bundle -run '^$$' -fuzz FuzzValidateEntryName -fuzztime $(FUZZTIME) -fuzzminimizetime 0

# Shim init-cost guard (plan §11). The same binary is the `claude` shim,
# so every package it links runs its init on every launch — the TUI stack
# included, even though only cmd/ imports it. This times the shim path
# alone (bffs init + account resolution + exec of /usr/bin/true) through
# the shim `bffs init` installed on PATH. Requires hyperfine (brew install
# hyperfine). Run it before and after a change that links new packages and
# compare p50: the budget is +10 % or +5 ms, whichever is larger; beyond
# it, bffs_notui becomes the default build until the init cost is gone.
# The shim execs the installed binary, so `make install` between the two
# runs. BFFS_HOME points at a throwaway store so the 300 launches neither
# read the real accounts nor land in the real launch log.
## bench-shim: time the shim path (hyperfine); the init-cost guard
bench-shim:
	@command -v hyperfine >/dev/null 2>&1 || { echo "  >  hyperfine not on PATH (brew install hyperfine)"; exit 1; }
	@command -v claude >/dev/null 2>&1 || { echo "  >  no claude shim on PATH (run bffs init)"; exit 1; }
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	BFFS_HOME="$$tmp" BFFS_REAL_CLAUDE=/usr/bin/true hyperfine -N --runs 300 'claude --version'

# ── Checks ───────────────────────────────────────────────────────
# GOLANGCI is whichever golangci-lint this machine can reach: mise's copy
# first (mise installs it without putting it on PATH unless the shell is
# activated), then PATH. Empty means the tool is absent, and `make lint`
# says so instead of failing — the pre-commit hook runs this target and
# must never be stricter than a machine that has not run `make tools`.
GOLANGCI := $(shell command -v golangci-lint 2>/dev/null || { command -v mise >/dev/null 2>&1 && mise which golangci-lint 2>/dev/null; })

## lint: gofmt, go vet, and golangci-lint when it is installed
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt (run 'make fmt'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
	go vet -tags bffs_notui ./...
	@if [ -n "$(GOLANGCI)" ]; then \
		echo "  >  $(GOLANGCI) run ./..."; \
		"$(GOLANGCI)" run ./...; \
	else \
		echo "  >  golangci-lint not installed (make tools); gofmt and vet only"; \
	fi

## lint-go: golangci-lint alone, failing when it is not installed
lint-go:
	@test -n "$(GOLANGCI)" || { echo "golangci-lint not found (make tools)"; exit 1; }
	"$(GOLANGCI)" run ./...

## test: the suite
test:
	go test ./...

## test-race: the suite the way CI runs it
test-race:
	go test -race ./...

## cover: write and summarise a coverage profile
cover:
	@mkdir -p dist
	go test -coverprofile=dist/coverage.out -covermode=atomic ./...
	@go tool cover -func=dist/coverage.out | tail -1
	@echo "  >  html: go tool cover -html=dist/coverage.out"

## golden: regenerate the TUI frame goldens after an intentional layout change
golden:
	go test ./internal/tui -update
	@git status --short internal/tui/testdata 2>/dev/null || true
	@echo "  >  the diff of the goldens is the review — read it before committing"

## check: everything CI gates on (lint + race tests)
check: lint test-race

## tools: install the developer tools pinned in mise.toml
# mise refuses a config file it has not been told to trust, so the first
# run on a checkout needs `mise trust` — it is your decision, not the
# Makefile's, which is why this stops and says so rather than doing it.
tools:
	@mise trust --check 2>/dev/null || { echo "  >  mise does not trust ./mise.toml yet: run 'mise trust' first"; exit 1; }
	mise install
	@echo "  >  golangci-lint: $$(command -v golangci-lint || mise which golangci-lint 2>/dev/null || echo 'not found — check mise activation')"

## help: list these targets
help:
	@echo "bffs — make targets"
	@echo
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort
	@echo
	@echo "  binary: $(BINARY)   version: $(VERSION)"

# Install the embedded bffs-rehome skill into a throwaway Claude config dir
# (a throwaway BFFS_HOME too, so no real account is touched) and run Claude
# Code's own validator over that config dir (a bare skill dir is validated as
# a plugin and only reports the missing manifest). --strict turns the
# validator's warnings (missing description and the like; it does not flag
# unknown frontmatter keys) into failures. Skipped when claude is not on PATH.
#
# The freshly built ./bffs is not the installed one, so its PATH walk for a
# real claude cannot recognise an installed bffs shim as itself and may cache
# the shim (which then execs itself until the 20 s timeout). BFFS_REAL_CLAUDE
# pointing nowhere skips the in-install check; the explicit run below is the
# check, and `claude` on PATH resolves normally there (no throwaway home).
## skill-validate: install the embedded skill into a temp dir and validate it
skill-validate: build
	@if ! command -v claude >/dev/null 2>&1; then echo "  >  claude not on PATH; skipping skill validation"; exit 0; fi; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	BFFS_HOME="$$tmp/bffs" BFFS_REAL_CLAUDE="$$tmp/no-claude" ./$(BINARY) skill install --claude-dir "$$tmp/claude" && \
	claude plugin validate "$$tmp/claude" --strict
