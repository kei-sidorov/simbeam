GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: proto build run run-remote test

# Broker baked into local remote-mode runs; override: make run-remote BROKER=wss://host/ws
BROKER ?= wss://simcast.sidorov.tech/ws

proto:
	mkdir -p internal/idbpb
	protoc \
		--go_out=. --go_opt=module=github.com/kei-sidorov/simcast \
		--go-grpc_out=. --go-grpc_opt=module=github.com/kei-sidorov/simcast \
		proto/idb.proto

build:
	go build ./...

run:
	go run ./cmd/simcastd serve --web ./web/debug

# One command: connects to the baked $(BROKER); press P → QR. --web serves the
# debug client at http://localhost:8080/ so you can click the pairing URL on the Mac.
run-remote:
	go run -ldflags "-X main.defaultSignalURL=$(BROKER)" ./cmd/simcastd serve --web ./web/debug

test:
	go test ./...
