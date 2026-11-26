# Build context: the repository root. Both stages are pinned by digest; the Go
# version here must be the one in go.mod, and `make image` checks it.

# The build stage runs on the machine that builds and cross-compiles for the
# target, so an image for another architecture needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY contract ./contract
COPY internal ./internal
# A static binary: no C library to match in the final stage. -trimpath keeps
# the paths of the build machine out of it.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/corinto ./cmd/corinto

# No shell, no package manager, an unprivileged user, and the certificates
# and time zones a static binary cannot bring itself.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/corinto /corinto
ENTRYPOINT ["/corinto"]
