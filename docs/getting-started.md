# Getting Started

[English](getting-started.md) | [简体中文](zh-CN/getting-started.md)

This guide starts a native or containerized proxy, creates a managed API key,
and connects Codex CLI or an OpenAI-compatible client.

## Prerequisites

- A Codex CLI OAuth login.
- Go 1.26 or later when building from source.
- Docker and Docker Compose only when using the container setup.

The proxy does not implement an OAuth login flow. It reads credentials already
stored on disk by Codex CLI.

## OAuth Files

The default auth directory is `~/.codex`. The official Codex CLI file is:

```text
~/.codex/auth.json
```

The proxy also accepts flat Codex token JSON files below the configured
`auth-dir`. It recursively scans JSON files, ignores files that are not Codex
credentials, and ignores records marked with `disabled: true`.

At least one loaded credential must contain an access token or refresh token.
Expired credentials are refreshed automatically before use and written back to
the same file.

## Native Installation

### Release Binary

GitHub Releases publish a Linux `amd64` binary and SHA-256 file:

```text
codex-oauth-proxy-linux-amd64
codex-oauth-proxy-linux-amd64.sha256
```

After verifying the checksum, make the binary executable and place it in a
directory on `PATH`.

### Build From Source

```bash
git clone https://github.com/zendext/codex-oauth-proxy.git
cd codex-oauth-proxy
go build -o codex-oauth-proxy ./cmd/server
```

## Native Startup

From a source checkout, copy the configuration template:

```bash
cp config.example.yaml config.yaml
```

When using only the release binary, create `config.yaml` with the minimal
equivalent:

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
```

Both forms bind to `127.0.0.1:8317` and read `~/.codex`. Start the server:

```bash
codex-oauth-proxy --config config.yaml
```

The explicit `serve` form is equivalent:

```bash
codex-oauth-proxy serve --config config.yaml
```

Run `codex-oauth-proxy --help` for the generated command tree, or append
`--help` to a command for its arguments and flags.

Confirm that the process is reachable:

```bash
curl http://127.0.0.1:8317/healthz
```

Expected response:

```json
{"status":"ok"}
```

## Create a Managed User

Keep the server running and use another terminal:

```bash
codex-oauth-proxy admin users create alice
```

The command connects to `http://127.0.0.1:8317` by default. It uses a
loopback-only local administration route and does not read `config.yaml` or
require `admin-api-key`.

The plaintext `cop_...` API key is shown only when a user is created or a key is
reset. Store it before closing the terminal output.

## Connect Codex CLI

Add a Responses provider to the Codex CLI configuration:

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

Set the managed key in the environment used to start Codex:

```bash
export COP_API_KEY="cop_..."
codex
```

`requires_openai_auth = false` tells Codex CLI to send `COP_API_KEY` to this
proxy. The proxy replaces it with a selected Codex OAuth access token before
forwarding the request upstream.

`chatgpt_base_url` enables the small set of ChatGPT backend compatibility routes
that Codex CLI uses for files, account status, and hosted MCP behavior. Those
routes are not a public API for general clients.

## Connect an OpenAI-Compatible Client

Use the proxy base URL and managed API key:

```text
Base URL: http://127.0.0.1:8317/v1
API key:  cop_...
```

List models:

```bash
curl http://127.0.0.1:8317/v1/models \
  -H 'Authorization: Bearer cop_...'
```

Send a non-streaming Chat Completions request:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer cop_...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }'
```

The proxy translates Chat Completions requests to upstream Responses requests.
Use `/v1/responses` directly when the client already supports the Responses wire
format.

## Docker Compose

Create `config.yaml` from the template and change the bind host:

```yaml
host: "0.0.0.0"
port: 8317
auth-dir: "/root/.codex"
```

Place Codex OAuth files under `./auths`. The default Compose project mounts:

```text
./config.yaml -> /codex-oauth-proxy/config.yaml
./auths       -> /root/.codex
```

Start the proxy:

```bash
docker compose up -d
```

Because the local administration endpoint accepts only loopback clients, run
the admin CLI inside the proxy container:

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users create alice
```

The image can also be started directly:

```bash
docker run --rm -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/root/.codex" \
  zendext/codex-oauth-proxy:latest
```

## Next Steps

- Review every setting in [Configuration](configuration.md).
- Manage users and keys with [Administration](administration.md).
- See supported routes in [API Reference](api-reference.md).
- Understand accounting in [Usage and Observability](usage-and-observability.md).
