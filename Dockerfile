# syntax=docker/dockerfile:1

# Build with the standard library; no Go module download is required.
# Base images must still be available locally or from their registries.
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/cline-pin-proxy ./cmd/cline-pin-proxy

# The runtime image includes CA certificates and runs without a shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/cline-pin-proxy /usr/local/bin/cline-pin-proxy

# Listen on the container network interface for port publishing.
# Compose restricts the published host port to 127.0.0.1.
ENV CLINE_PIN_LISTEN=0.0.0.0:8787

USER nonroot:nonroot
EXPOSE 8787

# Use the binary's healthcheck command; curl and wget are not installed.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/cline-pin-proxy", "healthcheck", "-url", "http://127.0.0.1:8787/healthz"]

ENTRYPOINT ["/usr/local/bin/cline-pin-proxy"]
CMD ["serve"]
