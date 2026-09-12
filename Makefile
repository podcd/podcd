# podcd - build, test, install.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/podcd/podcd/internal/cli.Version=$(VERSION)
DIST    := dist
GO ?= go
GOLANGCILINT_VERSION ?= v2.13.2

.PHONY: all build test test-e2e vet fmt lint clean install

all: build

## build: compile the binary into dist/
build:
	@mkdir -p $(DIST)
	go build -ldflags "$(LDFLAGS)" -o $(DIST)/podcd ./cmd/podcd
	@echo "built $(DIST)/podcd ($(VERSION))"

## test: unit tests. Fast, no containers, no network.
test:
	go test ./...

## test-e2e: the real thing - rootless podman, quadlet, systemd, on this machine.
## It writes unit files into ~/.config/containers/systemd and cleans up after itself.
test-e2e:
	podman pull docker.io/library/nginx:alpine
	PODCD_E2E=1 go test ./test/e2e/ -v -timeout 10m

vet:
	go vet ./...

fmt:
	gofmt -l -w .

## lint: run golangci-lint using the repo's Go toolchain.
lint:
	GO111MODULE=on $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCILINT_VERSION) run --timeout 10m --default=none --enable=govet

## install: put the binary on this machine (needs root).
install: build
	install -m 0755 $(DIST)/podcd /usr/local/bin/podcd

clean:
	rm -rf $(DIST)
