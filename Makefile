GO ?= go

# PLUGIN_ID is both the distribution id and the library file name: the host derives the
# plugin id from the loaded file name, so it must match plugins.configs.<id> and registry.json.
PLUGIN_ID := mimo-cliproxyapi

VERSION ?= 0.0.0-dev
DIST_DIR := $(CURDIR)/dist

# The extension follows the host platform: the host loads <PLUGIN_ID>.<ext> for its own GOOS.
PLUGIN_EXT := $(shell $(GO) env GOOS | sed -e 's/^darwin$$/dylib/' -e 's/^windows$$/dll/' -e 's/^linux$$/so/')

GOOS := $(shell $(GO) env GOOS)
GOARCH := $(shell $(GO) env GOARCH)
LIB := $(DIST_DIR)/$(PLUGIN_ID).$(PLUGIN_EXT)

.PHONY: build test dist clean

## build compiles the C-ABI shared library the host loads.
build:
	mkdir -p "$(DIST_DIR)"
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "-X main.pluginVersion=$(VERSION)" -o "$(LIB)" .
	rm -f "$(DIST_DIR)/$(PLUGIN_ID).h"
	@echo "$(LIB)"

## dist packages the host archive and checksums for the current platform only.
dist: build
	cd "$(DIST_DIR)" && rm -f "$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip" && zip -q -X "$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip" "$(PLUGIN_ID).$(PLUGIN_EXT)"
	cd "$(DIST_DIR)" && shasum -a 256 "$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip" > checksums.txt 2>/dev/null || cd "$(DIST_DIR)" && sha256sum "$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip" > checksums.txt

test:
	$(GO) test ./...

clean:
	rm -rf "$(DIST_DIR)"
