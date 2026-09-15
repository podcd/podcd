# podcd, packaged as a container image.
#
# Build: podman build -t podcd:dev -f Containerfile .
# Run:   podman run --rm podcd:dev version

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -ldflags "-s -w -X github.com/podcd/podcd/internal/cli.Version=${VERSION}" \
      -o /out/podcd ./cmd/podcd

FROM docker.io/library/debian:bookworm-slim
# systemd is here for its systemctl client and podman for its CLI
# both to talk to the *host's* podman/systemd --user session over mounted sockets -
# see examples/quadlet for running podcd itself this way.
RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates systemd podman && \
    rm -rf /var/lib/apt/lists/* && \
    useradd --create-home --uid 1000 podcd
COPY --from=build /out/podcd /usr/local/bin/podcd
USER podcd
WORKDIR /home/podcd
ENTRYPOINT ["podcd"]
CMD ["--help"]
