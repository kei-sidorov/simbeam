GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: proto build bin run run-remote test

# Broker baked into local remote-mode runs; override: make run-remote BROKER=wss://host/ws
BROKER ?= wss://signal.simbeam.dev/ws
# Hosted web client baked into `make bin`; override alongside BROKER for self-hosting.
CLIENT ?= https://app.simbeam.dev/

proto:
	mkdir -p internal/idbpb
	protoc \
		--go_out=. --go_opt=module=github.com/kei-sidorov/simbeam \
		--go-grpc_out=. --go-grpc_opt=module=github.com/kei-sidorov/simbeam \
		proto/idb.proto

build:
	go build ./...

# Release-like local daemon at ./bin/simbeamd: bakes the same broker, client, and
# short-pairing flag as .goreleaser so `./bin/simbeamd serve` behaves like a shipped
# binary (remote mode, pairing links to $(CLIENT), signal dropped from the URL). Keep
# these ldflags in sync with .goreleaser.yaml.
bin:
	go build -ldflags "-X main.defaultSignalURL=$(BROKER) -X main.defaultClientURL=$(CLIENT) -X main.omitSignalInPairURL=1" -o bin/simbeamd ./cmd/simbeamd

run:
	go run ./cmd/simbeamd serve --web ./web/debug

# One command: connects to the baked $(BROKER); press P → QR. --web serves the
# debug client at http://localhost:8080/ so you can click the pairing URL on the Mac.
run-remote:
	go run -ldflags "-X main.defaultSignalURL=$(BROKER)" ./cmd/simbeamd serve --web ./web/debug

test:
	go test ./...
