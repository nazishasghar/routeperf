# routeperf — build / install / release
#   make install                     → ~/.local/bin/routeperf
#   make install PREFIX=/usr/local   → /usr/local/bin/routeperf (may need sudo)
#   make install BIN=perfcheck       → install under a custom command name

BIN     ?= routeperf
PREFIX  ?= $(HOME)/.local
MODULE  := $(shell go list -m)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) \
           -X $(MODULE)/internal/version.Commit=$(COMMIT) -X $(MODULE)/internal/version.Date=$(DATE)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: build install uninstall test lint release testbed testbed-seed clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/routeperf

install: build
	@mkdir -p $(PREFIX)/bin
	install -m 0755 bin/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "installed $(PREFIX)/bin/$(BIN)"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) echo "note: add $(PREFIX)/bin to your PATH, e.g.  echo 'export PATH=\"$(PREFIX)/bin:\$$PATH\"' >> ~/.zshrc";; esac

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test:
	go vet ./...
	go test ./...

release:   ## cross-compile archives + checksums into dist/ (same names as GitHub releases)
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ $$os = windows ] && ext=.exe; \
	  d=dist/routeperf_$${os}_$${arch}; mkdir -p $$d; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$d/routeperf$$ext ./cmd/routeperf || exit 1; \
	  cp README.md routeperf.example.yaml $$d/; \
	  if [ $$os = windows ]; then (cd dist && zip -qr routeperf_$${os}_$${arch}.zip routeperf_$${os}_$${arch}); \
	  else tar -C dist -czf dist/routeperf_$${os}_$${arch}.tar.gz routeperf_$${os}_$${arch}; fi; \
	  rm -rf $$d; echo "  dist/routeperf_$${os}_$${arch}"; \
	done
	@cd dist && (shasum -a 256 * 2>/dev/null || sha256sum *) > checksums.txt && echo "  dist/checksums.txt"

testbed:
	go build -o bin/testbed ./testbed

testbed-seed: testbed   ## make testbed-seed DB=postgres://localhost:5432/routeperf_testbed
	bin/testbed --db-url $(DB) --seed

clean:
	rm -rf bin dist routeperf-out
