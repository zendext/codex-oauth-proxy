# codex-oauth-proxy

`codex-oauth-proxy` is a Codex OAuth-backed proxy for the Codex CLI and clients
that can use the OpenAI Responses API wire format. It forwards supported Codex
traffic to ChatGPT/Codex backend endpoints using existing Codex OAuth
credentials stored on disk.

Codex CLI is the primary target. The proxy also provides API-only managed-user
endpoints and a small Responses-compatible public API surface for custom agents.
It is not a complete OpenAI-compatible gateway: it does not implement a web UI,
provider translation, SDK embedding, plugin hosting, Chat Completions, or raw
Images API compatibility routes.

## Build

```bash
go build -o codex-oauth-proxy ./cmd/server
```

## Docker

Build the local Docker image:

```bash
docker build -t codex-oauth-proxy:dev .
```

For Docker, set `host: "0.0.0.0"` in the mounted `config.yaml` so the
published port can reach the server inside the container. For non-root
deployments, also set `auth-dir: "/codex-oauth-proxy/auths"` and mount a
writable host directory there.

Run with a mounted config file and Codex auth directory:

```bash
docker run --rm -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/root/.codex" \
  codex-oauth-proxy:dev
```

To avoid root-owned auth and database files on the host, run the container with
the host UID/GID and mount `auth-dir` outside `/root`:

```bash
docker run --rm --user "$(id -u):$(id -g)" -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/codex-oauth-proxy/auths" \
  codex-oauth-proxy:dev
```

Or use Docker Compose:

```bash
docker compose up -d --build
```

## Configure

Create `config.yaml` from `config.example.yaml`.

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
debug: false
admin-api-key: ""
database:
  path: ""
usage:
  enabled: true
  five-hour-reference-tokens: 0
  weekly-reference-tokens: 0
  alert-threshold: 0.8
  event-retention-days: 30
  debug-openai-response: false
proxy-url: ""
request-retry: 3
codex-base-url: "https://chatgpt.com/backend-api/codex"
chatgpt-base-url: "https://chatgpt.com/backend-api"
codex-user-agent: ""
codex-beta-features: ""
codex-refresh-token-url: ""
```

Auth files are read from `auth-dir`. By default the proxy can use the official
Codex CLI `~/.codex/auth.json` file directly. It also supports flat JSON files
with `type: "codex"`, `access_token`, and `refresh_token`. The server refreshes
expired tokens automatically and writes the updated token fields back to the same
file without changing the official Codex CLI file shape.

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

Set `admin-api-key` to enable API-only user management under `/v0/management`.
Managed users and their generated API keys are stored in SQLite. If
`database.path` is empty, the database is created at
`<auth-dir>/codex-oauth-proxy.db`.

Managed user API keys authenticate Codex proxy routes and `/v0/user`
endpoints. Set `COP_API_KEY` to a generated managed user API key when running
Codex through this proxy.

Usage tracking is enabled by default for proxy requests authenticated by managed
user API keys. It stores 10-minute UTC buckets in the same SQLite database and
exposes today's user totals at `/v0/user/usage/today`, plus management snapshots
and threshold events at `/v0/management/usage` and
`/v0/management/usage/events`. Backend compatibility routes that authenticate
with the current upstream Codex OAuth access token, such as Codex file uploads,
account/status calls, and hosted MCP pass-through, are not included in per-user
usage totals.

Set `debug: true` while diagnosing Codex Desktop or custom-provider setup. Debug
logs show request arrival, route selection, token header sources, authentication
decisions, upstream targets, upstream response status, and final response status.
Full API keys, OAuth access tokens, and refresh tokens are not logged.
Set both `debug: true` and `usage.debug-openai-response: true` for safe usage
diagnostics that include request IDs, status, key metadata, auth ID, model, and
token summaries without logging response bodies or WebSocket frame bodies.

For Codex file uploads used by Apps/MCP tools, point Codex's ChatGPT backend URL
at the proxy too. Codex also uses this backend URL to prefetch account rate-limit
data for `/status`.

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

Pull and run a published release. For this example, set
`auth-dir: "/codex-oauth-proxy/auths"` in `config.yaml`.

```bash
docker pull zendext/codex-oauth-proxy:v0.1.0
docker run --rm --user "$(id -u):$(id -g)" -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/codex-oauth-proxy/auths" \
  zendext/codex-oauth-proxy:v0.1.0
```

## Root Files

The root directory is intentionally small:

- `cmd/` and `internal/codexonly/` contain the server.
- `config.example.yaml` documents the runtime configuration.
- `Dockerfile`, `docker-compose.yml`, and `docker-build.*` are optional Docker
  helpers.
- `auths/.gitkeep` keeps a default local auth mount directory for Docker users.
- `.github/workflows/release.yml` publishes tagged releases and Docker images.
