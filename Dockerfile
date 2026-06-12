FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./codex-oauth-proxy ./cmd/server/

FROM debian:bookworm

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="codex-oauth-proxy" \
      org.opencontainers.image.description="Local Codex OAuth proxy for Codex CLI traffic" \
      org.opencontainers.image.source="https://github.com/zendext/codex-oauth-proxy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /codex-oauth-proxy

COPY --from=builder ./app/codex-oauth-proxy /codex-oauth-proxy/codex-oauth-proxy

COPY config.example.yaml /codex-oauth-proxy/config.example.yaml

WORKDIR /codex-oauth-proxy

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

ENTRYPOINT ["./codex-oauth-proxy"]
CMD ["--config", "/codex-oauth-proxy/config.yaml"]
