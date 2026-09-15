# podcd - build, test, install.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/podcd/podcd/internal/cli.Version=$(VERSION)
DIST    := dist
GO ?= go
GOLANGCILINT_VERSION ?= v2.13.2
IMAGE ?= podcd:$(VERSION)

# A failing `go test` must fail the pipeline it is tee'd through.
SHELL := bash
.SHELLFLAGS := -o pipefail -c

# Coverage and test-report output. CI uploads these; locally `make cover`
# opens the HTML report. The race detector needs cgo, so it is on wherever a
# C compiler is (CI, most workstations) and skipped where there is none.
RACE      := $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)
TESTCOVER ?= $(RACE) -covermode=atomic -coverprofile=coverage.out -coverpkg=./...
GOJUNITREPORT_VERSION ?= v2.1.0

.PHONY: all build image test test-report cover test-e2e vet fmt lint clean install

all: build

## build: compile the binary into dist/
build:
	@mkdir -p $(DIST)
	go build -ldflags "$(LDFLAGS)" -o $(DIST)/podcd ./cmd/podcd
	@echo "built $(DIST)/podcd ($(VERSION))"

## image: build the container image (see Containerfile for what it's for).
image:
	podman build --build-arg VERSION=$(VERSION) -t $(IMAGE) -f Containerfile .
	@echo "built $(IMAGE)"

## test: unit tests with the race detector and coverage. Fast, no containers, no network.
test:
	$(GO) test $(TESTCOVER) ./...

## test-report: the same tests, with a JUnit file (junit.xml) for CI to publish.
test-report:
	$(GO) test $(TESTCOVER) -v -json ./... 2>&1 | tee test.json | \
		$(GO) run github.com/jstemmer/go-junit-report/v2@$(GOJUNITREPORT_VERSION) -parser gojson -out junit.xml
	@scripts/coverage-summary.sh coverage.out

## cover: coverage per package, then the HTML report in the browser.
cover: test
	@scripts/coverage-summary.sh coverage.out
	$(GO) tool cover -html=coverage.out

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
	rm -rf $(DIST) coverage.out coverage.html junit.xml test.json
