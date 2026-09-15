# podcd, packaged as a container image.
#
# Build: podman build -t podcd:dev -f Containerfile .
# Run:   podman run --rm podcd:dev version

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

ARG VERSION=dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/podcd/podcd/internal/cli.Version=${VERSION}" \
      -o /out/podcd ./cmd/podcd

FROM docker.io/library/alpine:3.20
RUN apk add --no-cache git ca-certificates && \
    adduser -D -u 1000 podcd
COPY --from=build /out/podcd /usr/local/bin/podcd
USER podcd
WORKDIR /home/podcd
ENTRYPOINT ["podcd"]
CMD ["--help"]
