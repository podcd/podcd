# podcd - build, test, install.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/podcd/podcd/internal/cli.Version=$(VERSION)
DIST    := dist

.PHONY: all build test test-e2e vet fmt lint clean install

all: build

## build: compile both binaries into dist/
build:
	@mkdir -p $(DIST)
	go build -ldflags "$(LDFLAGS)" -o $(DIST)/podcd-agent ./cmd/agent
	go build -ldflags "$(LDFLAGS)" -o $(DIST)/podcd ./cmd/cli
	@echo "built $(DIST)/podcd and $(DIST)/podcd-agent ($(VERSION))"

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

## install: put the binaries on this machine (needs root).
install: build
	install -m 0755 $(DIST)/podcd /usr/local/bin/podcd
	install -m 0755 $(DIST)/podcd-agent /usr/local/bin/podcd-agent

clean:
	rm -rf $(DIST)
