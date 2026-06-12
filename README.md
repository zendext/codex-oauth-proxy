# codex-oauth-proxy

`codex-oauth-proxy` is a Codex OAuth-backed proxy for the Codex CLI and clients
that can use the OpenAI Responses API wire format. It forwards supported Codex
traffic to ChatGPT/Codex backend endpoints using existing Codex OAuth
credentials stored on disk.

Codex CLI is the primary target. The proxy also provides API-only managed-user
endpoints and a small Responses-compatible public API surface for custom agents.

## Build

```bash
go build -o codex-oauth-proxy ./cmd/server
```

## Docker

```bash
docker run --rm -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/root/.codex" \
  zendext/codex-oauth-proxy:latest
```

```bash
docker compose up -d
```

## Configure

Create `config.yaml` from `config.example.yaml`.

| Field | Default | Description |
| --- | --- | --- |
| `host` | `127.0.0.1` | Bind host. Use `0.0.0.0` in containers. |
| `port` | `8317` | Bind port. |
| `auth-dir` | `~/.codex` | Codex auth directory. Supports official `auth.json` and flat token JSON files. |
| `debug` | `false` | Enable masked request/proxy debug logs. |
| `admin-api-key` | empty | Enables `/v0/management` when set. |
| `database.path` | empty | SQLite path. Empty resolves to `<auth-dir>/codex-oauth-proxy.db`. |
| `usage.enabled` | `true` | Track per-user token usage for managed API keys. |
| `usage.five-hour-reference-tokens` | `0` | Reference capacity for 5-hour usage ratios. |
| `usage.weekly-reference-tokens` | `0` | Reference capacity for weekly usage ratios. |
| `usage.alert-threshold` | `0.8` | Threshold-event ratio. |
| `usage.event-retention-days` | `30` | Threshold-event retention. |
| `usage.debug-openai-response` | `false` | Log safe usage metadata from upstream responses. |
| `proxy-url` | empty | Optional outbound proxy. Use `direct` or `none` to bypass proxy settings. |
| `request-retry` | `3` | Upstream retry attempts for retry-aware calls. |
| `codex-base-url` | `https://chatgpt.com/backend-api/codex` | Codex upstream base URL. |
| `chatgpt-base-url` | `https://chatgpt.com/backend-api` | ChatGPT backend base URL for Codex compatibility calls. |
| `codex-user-agent` | empty | Optional upstream User-Agent override. |
| `codex-beta-features` | empty | Optional `x-codex-beta-features` fallback. |
| `codex-refresh-token-url` | empty | Optional OAuth refresh endpoint override. |

Expired OAuth tokens are refreshed automatically and written back to the same
auth file.

Configure Codex CLI to use the proxy as a Responses provider. Use
`supports_websockets = true` for realtime routes and
`requires_openai_auth = false` so Codex sends the managed user API key from
`COP_API_KEY` to this proxy. The `chatgpt_base_url` value below is for Codex
client compatibility only; `/backend-api/*` is not a public API surface.

```toml
model_provider = "proxy"
chatgpt_base_url = "http://127.0.0.1:8317/backend-api/"

[model_providers.proxy]
name = "OpenAI using LLM proxy"
base_url = "http://127.0.0.1:8317/v1"
env_key = "COP_API_KEY"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = false
```

Set `admin-api-key` to enable `/v0/management`. Generated user API keys
authenticate proxy routes and `/v0/user`; set `COP_API_KEY` when running Codex
through this proxy.

Usage tracking stores 10-minute UTC buckets for managed user API keys. User
totals are exposed at `/v0/user/usage/today`; management snapshots and threshold
events are exposed at `/v0/management/usage` and
`/v0/management/usage/events`.

Set `debug: true` for masked request/proxy logs. Add
`usage.debug-openai-response: true` for request IDs, model, key metadata, and
token summaries; secrets and response bodies are not logged.

## Run

```bash
./codex-oauth-proxy --config config.yaml
```

Public API routes:

- `GET /`
- `GET /healthz`
- `POST /v0/management/users`
- `GET /v0/management/users`
- `GET /v0/management/users/{user_id}`
- `PATCH /v0/management/users/{user_id}`
- `POST /v0/management/users/{user_id}/api-key/reset`
- `GET /v0/management/usage`
- `GET /v0/management/usage/events`
- `GET /v0/user/api-key`
- `POST /v0/user/api-key/reset`
- `GET /v0/user/usage/today`
- `GET /v1/models`
- `POST /v1/responses`
- `GET /v1/responses`
- `POST /v1/responses/compact`
- `POST /v1/alpha/search`
- `POST /v1/memories/trace_summarize`
- `POST /v1/realtime/calls`
- `GET /v1/realtime`

Image generation is available only through `/v1/responses` by using the upstream
Responses API `image_generation` tool, normally with `stream: true`. Raw
`/v1/images/*` and `/backend-api/*` endpoints are not public API and should not
be used by general clients.

Protected proxy routes require a managed user API key:

```bash
curl http://localhost:8317/v1/models \
  -H 'Authorization: Bearer cop_...'
```

Create a managed user and one generated API key:

```bash
curl -X POST http://localhost:8317/v0/management/users \
  -H 'Authorization: Bearer admin-change-me' \
  -H 'Content-Type: application/json' \
  -d '{"name":"alice"}'
```

The plaintext user API key is returned only by user creation and key reset
responses. List/detail responses only return key metadata such as `masked_key`.

## Verify

```bash
gofmt -w .
go test ./...
go build -o test-output ./cmd/server && rm test-output
```

## Release

Pushing a version tag that starts with `v` runs
`.github/workflows/release.yml`.

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow:

- runs `go test -count=1 ./...`
- builds `codex-oauth-proxy-linux-amd64`
- uploads the binary and its SHA-256 file to the GitHub Release
- builds and pushes a `linux/amd64` Docker image to Docker Hub

Required GitHub repository secrets:

- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

Docker Hub image tags are published under `zendext/codex-oauth-proxy` with the
release tag, semver aliases, and `latest`.
