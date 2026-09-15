# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# 构建阶段
#
# 本项目零第三方依赖，所以不需要 go mod download，也不需要在构建时访问
# 模块代理——这让镜像构建可以在任何网络环境下稳定复现。
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# 运行阶段
#
# distroless static 而非 scratch：它自带 CA 证书，而访问
# https://api.cline.bot 必须要证书链。同时是非 root、无 shell、无包管理器，
# 攻击面最小。
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/cline-pin-proxy /usr/local/bin/cline-pin-proxy

# 容器内必须监听 0.0.0.0，否则宿主机的端口映射无法命中。
# 对外的暴露范围由宿主机端口绑定控制——docker-compose.yml 里绑的是 127.0.0.1。
ENV CLINE_PIN_LISTEN=0.0.0.0:8787

USER nonroot:nonroot
EXPOSE 8787

# distroless 里没有 curl/wget，只能让二进制自己探自己。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/cline-pin-proxy", "healthcheck", "-url", "http://127.0.0.1:8787/healthz"]

ENTRYPOINT ["/usr/local/bin/cline-pin-proxy"]
CMD ["serve"]
