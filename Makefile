GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: proto build run test

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

test:
	go test ./...
