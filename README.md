# codex-oauth-proxy

[English](README.md) | [简体中文](README.zh-CN.md)

> This project was written by AI. Use it at your own risk.

---

`codex-oauth-proxy` is a small self-hosted HTTP proxy that forwards supported
Codex traffic with OAuth credentials already stored by Codex CLI. It is intended
for operators who want to expose their own Codex OAuth access through managed
API keys without distributing the OAuth files themselves.

The proxy is Codex-specific. It supports Codex CLI, a narrow OpenAI-compatible
API surface for custom agents, local user administration, and per-user usage
statistics. It is not a general OpenAI gateway or a multi-provider proxy.

## Features

- Loads the official Codex CLI `~/.codex/auth.json` format and flat Codex token
  JSON files.
- Selects multiple active OAuth files in round-robin order and refreshes expired
  access tokens.
- Proxies whitelisted Responses, realtime, image, search, memory, file, account,
  and hosted MCP routes used by Codex CLI.
- Issues managed `cop_...` API keys backed by SQLite.
- Provides an OpenAI-compatible `/v1/chat/completions` translation layer.
- Records per-user token usage in 10-minute buckets with 30-day retention.
- Includes a local admin CLI and an optional Grafana dashboard.

## Requirements

- Go 1.26 or later when building from source.
- At least one usable Codex OAuth file, normally created by Codex CLI at
  `~/.codex/auth.json`.

Linux `amd64` release binaries are available from
[GitHub Releases](https://github.com/zendext/codex-oauth-proxy/releases).

## Quick Start

Build the proxy:

```bash
go build -o codex-oauth-proxy ./cmd/server
cp config.example.yaml config.yaml
codex-oauth-proxy --config config.yaml
```

In another terminal, create a managed user:

```bash
codex-oauth-proxy admin users create alice
```

The command prints the generated API key once. Configure Codex CLI to use it:

```toml
model_provider = "proxy"
chatgpt_base_url = "http://127.0.0.1:8317/backend-api/"

[model_providers.proxy]
name = "Codex OAuth Proxy"
base_url = "http://127.0.0.1:8317/v1"
env_key = "COP_API_KEY"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = false
```

```bash
export COP_API_KEY="cop_..."
codex
```

The local admin CLI calls a loopback-only endpoint and does not require
`admin-api-key`. Set `admin-api-key` only when remote management API or Grafana
access is needed.

## Docker

For containers, set `host: "0.0.0.0"` in `config.yaml`, place Codex OAuth JSON
files under `./auths`, and start the service:

```bash
docker compose up -d
```

Run local administration inside the container:

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users create alice
```

See [Getting Started](docs/getting-started.md) for native and Docker setup
details.

## Documentation

- [Getting Started](docs/getting-started.md)
- [Configuration](docs/configuration.md)
- [Administration](docs/administration.md)
- [API Reference](docs/api-reference.md)
- [Usage and Observability](docs/usage-and-observability.md)
- [Architecture](docs/architecture.md)
- [Development](docs/development.md)

## Scope

The proxy forwards only explicitly supported routes. `/v1/*` and project-owned
`/v0/*` endpoints are the supported integration surface. Selected
`/backend-api/*` paths exist for Codex CLI compatibility and are not a stable
public API for third-party clients.

The service listens over HTTP. Binding, TLS termination, and network exposure
are deployment concerns outside the proxy.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
