BINARY      := decision-engine
BIN_DIR     := bin
CMD_SERVER  := ./cmd/server
PROTO_DIR   := proto
IMAGE_NAME  := ratelimiter-decision-engine

PROTO_SRC   := $(PROTO_DIR)/scorer.proto
PROTO_OUT   := $(PROTO_DIR)/scorer.pb.go $(PROTO_DIR)/scorer_grpc.pb.go

GO          := go
GOFLAGS     := -trimpath
LDFLAGS     := -ldflags="-s -w"

.PHONY: all build test lint proto docker-build clean

## Default target
all: build

## ── Build ─────────────────────────────────────────────────────────────────────
build: $(BIN_DIR)/$(BINARY)

$(BIN_DIR)/$(BINARY): $(shell find . -name '*.go' -not -path './vendor/*')
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux $(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ $(CMD_SERVER)
	@echo "✓ built $@"

## ── Test ──────────────────────────────────────────────────────────────────────
test:
	$(GO) test -race -count=1 -timeout=60s ./...

## ── Lint ──────────────────────────────────────────────────────────────────────
lint:
	@which golangci-lint > /dev/null || (echo "install golangci-lint first"; exit 1)
	golangci-lint run ./...

## ── Proto generation ──────────────────────────────────────────────────────────
proto: $(PROTO_OUT)

$(PROTO_OUT): $(PROTO_SRC)
	@which protoc > /dev/null || (echo "install protoc first"; exit 1)
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		$(PROTO_SRC)
	@echo "✓ proto generated"

## ── Docker ────────────────────────────────────────────────────────────────────
docker-build:
	docker build -t $(IMAGE_NAME) .
	@echo "✓ docker image $(IMAGE_NAME) built"

## ── Clean ─────────────────────────────────────────────────────────────────────
clean:
	rm -rf $(BIN_DIR)
	@echo "✓ cleaned"
