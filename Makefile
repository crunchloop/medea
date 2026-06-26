# Medea — external control plane for Talos. See PRD.md.
#
# Requires (go install): buf, protoc-gen-go, protoc-gen-go-grpc.
# These live in $(go env GOPATH)/bin; ensure it's on PATH or run `make tools`.

GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: tools generate build test vet lint check

tools: ## install codegen tools into GOPATH/bin
	go install github.com/bufbuild/buf/cmd/buf@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

generate: ## regenerate Go types from proto/
	buf lint
	buf generate

build: ## build all packages
	go build ./...

test: ## run tests with the race detector
	go test -race ./...

vet:
	go vet ./...

check: vet test ## what CI runs
