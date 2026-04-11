APP := imcodex
GO ?= go
UPX ?= upx
HAVE_UPX := $(shell command -v $(UPX) >/dev/null 2>&1 && echo yes || echo no)

LOCAL_GOOS := $(shell $(GO) env GOOS)
LOCAL_GOARCH := $(shell $(GO) env GOARCH)

LINUX_GOOS ?= linux
LINUX_GOARCH ?= amd64

BUILD_DIR := build
LOCAL_BIN := $(BUILD_DIR)/$(APP)-$(LOCAL_GOOS)-$(LOCAL_GOARCH)
LINUX_BIN := $(BUILD_DIR)/$(APP)-$(LINUX_GOOS)-$(LINUX_GOARCH)

.PHONY: all linux clean test release build-local build-linux check-go check-upx

all: check-go build-local

linux: check-go build-linux

check-go:
	@command -v $(GO) >/dev/null 2>&1 || { echo "$(GO) not found"; exit 1; }

check-upx:
	@command -v $(UPX) >/dev/null 2>&1 || { echo "$(UPX) not found"; exit 1; }

build-local:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(LOCAL_GOOS) GOARCH=$(LOCAL_GOARCH) $(GO) build -o $(LOCAL_BIN) .
ifeq ($(LOCAL_GOOS),darwin)
	@echo "skip upx for macOS: packed binary is killed by macOS at launch"
else
ifeq ($(HAVE_UPX),yes)
	$(UPX) -q $(LOCAL_BIN)
else
	@echo "skip upx for local build: $(UPX) not found"
endif
endif

build-linux:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(LINUX_GOOS) GOARCH=$(LINUX_GOARCH) $(GO) build -o $(LINUX_BIN) .
ifeq ($(HAVE_UPX),yes)
	$(UPX) -q $(LINUX_BIN)
else
	@echo "skip upx for linux build: $(UPX) not found"
endif

test:
	$(GO) test ./...
	$(GO) test -race ./...

release: check-go
	./tools/release/build-assets.sh

clean:
	rm -rf $(BUILD_DIR)
