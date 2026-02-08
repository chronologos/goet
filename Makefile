GO ?= go
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS := -X github.com/chronologos/goet/internal/version.Commit=$(COMMIT)

.PHONY: build install test test-race fuzz clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o goet ./cmd/goet

install: build
	mkdir -p ~/.local/bin
	ln -sf $(CURDIR)/goet ~/.local/bin/goet

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

fuzz:
	$(GO) test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/

clean:
	rm -f goet
