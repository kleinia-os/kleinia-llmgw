GO ?= $(HOME)/.local/go/bin/go
BIN := bin

.PHONY: build test test-live fmt vet all clean

build:
	$(GO) build -o $(BIN)/llmgwd ./cmd/llmgwd
	$(GO) build -o $(BIN)/llmgwctl ./cmd/llmgwctl

test:
	$(GO) test ./...

# live tests hit real upstreams; require test/live.env (LLMGW_LIVE=1,
# LLMGW_TEST_KEY, LLMGW_TEST_BASE, LLMGW_TEST_MODEL)
test-live:
	LLMGW_LIVE=1 $(GO) test -tags live ./test/...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

all: fmt vet build test

clean:
	rm -rf $(BIN) var
