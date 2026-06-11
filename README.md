# codex-oauth-proxy

`codex-oauth-proxy` is a small local proxy for the Codex CLI. It forwards Codex
CLI traffic to ChatGPT/Codex backend endpoints using existing Codex OAuth
credentials stored on disk.

It does not implement a web UI, provider translation, management APIs, SDK
embedding, plugin hosting, or non-Codex provider compatibility routes.

## Build

```bash
go build -o codex-oauth-proxy ./cmd/server
```

## Configure

Create `config.yaml` from `config.example.yaml`.

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
api-keys: []
proxy-url: ""
codex-base-url: "https://chatgpt.com/backend-api/codex"
chatgpt-base-url: "https://chatgpt.com/backend-api"
```

Auth files are read from `auth-dir`. By default the proxy can use the official
Codex CLI `~/.codex/auth.json` file directly. It also supports flat JSON files
with `type: "codex"`, `access_token`, and `refresh_token`. The server refreshes
expired tokens automatically and writes the updated token fields back to the same
file without changing the official Codex CLI file shape.

Configure Codex CLI to use the proxy as a Responses provider. The important
setting is `supports_websockets = true`; without it Codex falls back to HTTP SSE
and `/status` may show the wrong limit bucket.

```toml
[model_providers.proxy]
name = "OpenAI using LLM proxy"
base_url = "http://127.0.0.1:8317/v1"
env_key = "CLIPROXYAPI_API_KEY"
wire_api = "responses"
supports_websockets = true
```

If `api-keys` is non-empty, set `CLIPROXYAPI_API_KEY` to one of those keys. For
local-only testing with `api-keys: []`, any non-empty value is enough.

For Codex file uploads used by Apps/MCP tools, point Codex's ChatGPT backend URL
at the proxy too:

```toml
chatgpt_base_url = "http://127.0.0.1:8317/backend-api/"
```

## Run

```bash
./codex-oauth-proxy --config config.yaml
```

Supported routes:

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/responses`
- `GET /v1/responses`
- `POST /v1/responses/compact`
- `POST /v1/alpha/search`
- `POST /v1/images/generations`
- `POST /v1/images/edits`
- `POST /v1/memories/trace_summarize`
- `POST /v1/realtime/calls`
- `GET /v1/realtime`
- `POST /backend-api/codex/responses`
- `GET /backend-api/codex/responses`
- `POST /backend-api/codex/responses/compact`
- `POST /backend-api/codex/alpha/search`
- `POST /backend-api/codex/images/generations`
- `POST /backend-api/codex/images/edits`
- `POST /backend-api/codex/memories/trace_summarize`
- `POST /backend-api/codex/realtime/calls`
- `GET /backend-api/codex/realtime`
- `POST /backend-api/files`
- `POST /backend-api/files/{file_id}/uploaded`

When `api-keys` is non-empty, protected routes require one configured proxy API
key:

```bash
curl http://localhost:8317/v1/models \
  -H 'Authorization: Bearer change-me'
```

## Verify

```bash
gofmt -w .
go test ./...
go build -o test-output ./cmd/server && rm test-output
```

## Root Files

The root directory is intentionally small:

- `cmd/` and `internal/codexonly/` contain the server.
- `config.example.yaml` documents the runtime configuration.
- `Dockerfile`, `docker-compose.yml`, and `docker-build.*` are optional Docker
  helpers.
- `auths/.gitkeep` keeps a default local auth mount directory for Docker users.
- `.github/workflows/pr-test-build.yml` runs the minimal Go CI check.
