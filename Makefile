# anymd — pure-Go any-document → Markdown.
# No cgo, no external toolchain: every target here is `go` and nothing else.

BINARY  := anymd
PKG     := ./cmd/anymd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# CGO_ENABLED=0 is the product promise, not an optimization: the binary must be
# static and cross-compile anywhere Go targets.
GO      := CGO_ENABLED=0 go

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: all build test lint fmt install release clean

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

test:
	go test ./...

lint:
	go vet ./...

fmt:
	gofmt -w .

install:
	$(GO) install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

release: clean
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="dist/$(BINARY)_$(VERSION)_$${os}_$${arch}$$ext"; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o "$$out" $(PKG) || exit 1; \
	done
	@ls -1 dist

clean:
	rm -rf dist $(BINARY)
