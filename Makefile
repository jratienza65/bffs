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
