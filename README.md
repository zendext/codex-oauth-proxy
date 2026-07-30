# codex-oauth-proxy

[English](README.md) | [简体中文](README.zh-CN.md)

> This project was written by AI. Use it at your own risk.

---

`codex-oauth-proxy` forwards Codex requests through managed API keys using OAuth
credentials from Codex CLI.

## Features

- Proxies Codex CLI and supported OpenAI-compatible requests.
- Manages `cop_...` API keys for proxy users.
- Rotates multiple Codex OAuth credentials and refreshes expired tokens.
- Records per-user token usage and provides an optional Grafana dashboard.

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

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
