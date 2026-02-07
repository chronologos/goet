GO ?= go

.PHONY: build test test-race fuzz clean

build:
	$(GO) build -o goet ./cmd/goet

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

fuzz:
	$(GO) test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/

clean:
	rm -f goet
