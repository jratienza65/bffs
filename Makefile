BINARY    := bffs
INSTALL_PATH ?= /opt/bffs

# ── Tuning (override: make JOBS=16 GO_BUILD_FLAGS="-v" build) ────
JOBS           ?= $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
GO_P           ?= $(JOBS)
GO_BUILD_FLAGS ?=

build:
	go build -p $(GO_P) $(GO_BUILD_FLAGS) -o $(BINARY) .

install: build
	@echo "  >  Installing $(BINARY) to $(INSTALL_PATH)"
	@if [ "$(INSTALL_PATH)" = "/opt/bffs" ]; then \
		sudo mkdir -p $(INSTALL_PATH); \
		sudo cp $(BINARY) $(INSTALL_PATH)/$(BINARY); \
		sudo chmod +x $(INSTALL_PATH)/$(BINARY); \
	else \
		mkdir -p $(INSTALL_PATH); \
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

.PHONY: build install release-check snapshot clean-dist hooks fmt lint

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

# Same checks the pre-commit hook and ci.yml run.
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt (run 'make fmt'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
