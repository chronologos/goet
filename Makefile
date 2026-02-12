GO ?= go
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS := -X github.com/chronologos/goet/internal/version.Commit=$(COMMIT)
FUZZTIME ?= 30s

.PHONY: build install test test-race test-e2e fuzz lint clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o goet ./cmd/goet

install: build
	mkdir -p ~/.local/bin
	ln -sf $(CURDIR)/goet ~/.local/bin/goet

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-e2e: build
	./tests/integration_test.sh

lint:
	$(GO) vet ./...

# All 20 fuzz targets, 4 at a time. Override duration: make fuzz FUZZTIME=5m
FUZZ_TARGETS := \
	FuzzDecodeAuthRequest:./internal/protocol/ \
	FuzzDecodeHeartbeat:./internal/protocol/ \
	FuzzDecodeResize:./internal/protocol/ \
	FuzzDecodeData:./internal/protocol/ \
	FuzzDecodeSequenceHeader:./internal/protocol/ \
	FuzzDecodeTerminalInfo:./internal/protocol/ \
	FuzzReadMessage:./internal/protocol/ \
	FuzzRoundTripHeartbeat:./internal/protocol/ \
	FuzzRoundTripResize:./internal/protocol/ \
	FuzzRoundTripSequenceHeader:./internal/protocol/ \
	FuzzRoundTripData:./internal/protocol/ \
	FuzzRoundTripTerminalInfo:./internal/protocol/ \
	FuzzRoundTripAuthRequest:./internal/protocol/ \
	FuzzEscapeProcess:./internal/client/ \
	FuzzEscapeProcessMultiCall:./internal/client/ \
	FuzzParseDestination:./internal/client/ \
	FuzzBufferStoreReplay:./internal/catchup/ \
	FuzzBufferEviction:./internal/catchup/ \
	FuzzBufferDataIntegrity:./internal/catchup/ \
	FuzzCoalescerDataIntegrity:./internal/coalesce/

FUZZ_LOGDIR := /tmp/goet-fuzz

fuzz:
	@mkdir -p $(FUZZ_LOGDIR)
	@echo "Fuzzing all $(words $(FUZZ_TARGETS)) targets ($(FUZZTIME) each, 4 parallel)..."
	@echo "Logs: $(FUZZ_LOGDIR)/"
	@printf '%s\n' $(FUZZ_TARGETS) \
		| xargs -P4 -L1 sh -c ' \
			name=$$(echo "$$1" | cut -d: -f1); \
			pkg=$$(echo "$$1" | cut -d: -f2); \
			logfile=$(FUZZ_LOGDIR)/$${name}.log; \
			echo "  $$name"; \
			$(GO) test -fuzz="^$${name}$$" -fuzztime=$(FUZZTIME) $$pkg > "$$logfile" 2>&1 \
				|| { echo "FAIL: $$name (see $$logfile)"; exit 1; } \
		' _
	@echo "All fuzz targets passed."

clean:
	rm -f goet
