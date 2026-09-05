BINARY    := bffs
INSTALL_PATH ?= /opt/bffs

# ── Tuning (override: make JOBS=16 GO_BUILD_FLAGS="-v" build) ────
JOBS           ?= $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
GO_P           ?= $(JOBS)
GO_BUILD_FLAGS ?=

build:
	go build -p $(GO_P) $(GO_BUILD_FLAGS) -o $(BINARY) .

# The rm before cp is load-bearing on macOS: cp onto an existing file rewrites
# the same inode, which invalidates the kernel's cached code signature for the
# binary — every subsequent exec is then SIGKILLed ("zsh: killed") even though
# `codesign -v` reports the file as valid. Unlinking first gives the copy a
# fresh inode and a clean signature registration.
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

.PHONY: build install release-check snapshot clean-dist hooks fmt lint fuzz

release-check:
	$(GORELEASER) check

# Full cross-platform build into ./dist without touching GitHub.
snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

clean-dist:
	rm -rf dist

# ── Contributor tooling ──────────────────────────────────────────
# Git will not run hooks from a fresh clone on its own (by design), so this
# is opt-in per checkout. Run it once after cloning.
hooks:
	@git config core.hooksPath .githooks
	@echo "  >  pre-commit hook enabled (gofmt + go vet, mirrors CI)"
	@echo "  >  bypass once with: git commit --no-verify"

fmt:
	gofmt -w .

# Fuzz the bundle reader and the entry-name grammar (CI runs the same two
# targets for 20 s each as a smoke test). Override: make FUZZTIME=5m fuzz
# Minimization is off (-fuzzminimizetime 0): with the default 60 s per new
# interesting input the workers stop mutating within seconds (FuzzUnpack
# stages files on disk per exec); inputs are a few KiB, so an unminimized
# crasher reproduces just as well.
FUZZTIME ?= 30s

fuzz:
	go test ./internal/bundle -run '^$$' -fuzz FuzzUnpack -fuzztime $(FUZZTIME) -fuzzminimizetime 0
	go test ./internal/bundle -run '^$$' -fuzz FuzzValidateEntryName -fuzztime $(FUZZTIME) -fuzzminimizetime 0

# Same checks the pre-commit hook and ci.yml run.
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt (run 'make fmt'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
