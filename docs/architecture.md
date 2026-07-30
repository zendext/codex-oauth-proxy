# Architecture

[English](architecture.md) | [简体中文](zh-CN/architecture.md)

`codex-oauth-proxy` is one Go binary with two execution modes:

- `serve` starts the HTTP proxy and project APIs.
- `admin` calls the running server's loopback-only administration route.

The project intentionally keeps one Codex-specific implementation. It does not
include provider translation, a management UI, plugin hosting, or alternate
storage backends.

## Source Layout

| Path | Responsibility |
| --- | --- |
| `cmd/server/` | Process entrypoint, server lifecycle, admin CLI, HTTP client, and CLI formatting. |
| `internal/codexonly/config.go` | YAML loading and path/default resolution. |
| `internal/codexonly/auth.go` | OAuth file discovery, parsing, filtering, and persistence. |
| `internal/codexonly/refresh.go` | OAuth refresh and outbound HTTP transport construction. |
| `internal/codexonly/server.go` | Routing, authentication, model catalog, headers, HTTP reverse proxy, and management/user handlers. |
| `internal/codexonly/chat_completions.go` | Chat Completions to Responses conversion and response translation. |
| `internal/codexonly/user_store.go` | SQLite schema, users, API keys, and authentication. |
| `internal/codexonly/usage.go` | Usage storage, aggregation, windows, dimensions, and timeseries. |
| `internal/codexonly/usage_proxy.go` | HTTP, SSE, and WebSocket usage capture. |
| `observability/` | Optional Grafana Compose and provisioning assets. |

## Server Startup

Startup performs these steps:

1. Parse CLI flags and select server mode.
2. Read YAML configuration and apply code defaults.
3. Resolve `auth-dir` and validate the two upstream base URLs.
4. Build an upstream client and a timeout-limited OAuth refresh client.
5. Scan the auth directory to verify it is readable.
6. Resolve the SQLite path and run idempotent schema migrations.
7. Start one `net/http` server.

The server sets `ReadHeaderTimeout` to 10 seconds. It does not set read or write
timeouts that would terminate established streaming or WebSocket traffic.
Graceful shutdown has a 10-second timeout.

## OAuth Credential Flow

`FileAuthStore` recursively scans the configured directory on selection rather
than keeping a long-lived in-memory token snapshot.

For every upstream request:

1. Load active Codex auth files.
2. Sort them by relative file ID.
3. Select the next file in round-robin order.
4. Treat credentials expiring within five minutes as expired.
5. Refresh an expired credential with its refresh token.
6. Write refreshed values back to the same JSON shape and file.
7. Forward the request with the selected access token and account ID.

This model allows file additions, removals, disables, and external token updates
to be observed without a config reload.

## Authentication Boundaries

The incoming managed API key and the outgoing OAuth access token are separate
credentials.

### Managed User Authentication

For public proxy and user routes:

1. Read `Authorization` and `X-API-Key`.
2. Hash each candidate with SHA-256.
3. Look up the stored key hash.
4. Verify the API key and user are enabled.
5. Attach user and key identity to the request.

The incoming managed key is never forwarded upstream.

### Management Authentication

`/v0/management/*` compares candidate tokens with the configured
`admin-api-key` using constant-time comparison. The entire route group is hidden
with `404` when the key is not configured.

### Local Administration

`/v0/local-admin/*` checks the TCP remote address and accepts only loopback IPs.
It then reuses the management handlers without an admin key.

### OAuth Compatibility Authentication

Selected ChatGPT backend compatibility routes can accept a currently loaded
Codex OAuth access token. This exists for Codex CLI behavior that already holds
that token. Such requests have no managed user identity and are excluded from
per-user usage accounting.

## Route Selection

The HTTP handler checks project-owned routes before the reverse proxy whitelist.

| Route group | Owner |
| --- | --- |
| `/`, `/healthz` | Service handler |
| `/v0/local-admin/*` | Loopback administration |
| `/v0/management/*` | Admin-key management API |
| `/v0/user/*` | Managed user self-service API |
| `/v1/models` | Embedded model catalog |
| `/v1/chat/completions` | Local protocol conversion |
| Whitelisted `/v1/*` | Codex upstream reverse proxy |
| Selected `/backend-api/*` | Codex CLI compatibility reverse proxy |

All unmatched routes return `404`; there is no general catch-all upstream
forwarder.

## Reverse Proxy Flow

For a whitelisted proxy request:

1. Authenticate the incoming managed key or permitted OAuth compatibility token.
2. Reject Fast service tiers when Fast mode is disabled.
3. Select and refresh an upstream OAuth credential.
4. Rewrite the target URL to the configured Codex or ChatGPT base.
5. Replace `Authorization` with the selected OAuth access token.
6. Add the ChatGPT account ID and compatibility headers when available.
7. Forward HTTP streaming responses or bridge WebSocket frames.
8. Capture usage metadata for managed user requests.

The proxy preserves established HTTP streams. WebSocket forwarding uses Gorilla
WebSocket and forces HTTP/1.1 ALPN for the upstream upgrade path.

## Chat Completions Conversion

`/v1/chat/completions` is implemented locally rather than passed through:

1. Decode the Chat Completions JSON object.
2. Convert messages, tools, response format, reasoning, and service tier to a
   Responses request.
3. Force `stream: true` and `store: false` upstream.
4. Read Responses SSE events.
5. Aggregate them into a normal Chat Completions response or translate them into
   Chat Completions SSE chunks.
6. Apply local stop-sequence filtering and record usage.

This is a focused compatibility layer, not a generic schema-preserving
translation engine.

## Managed User Data Model

SQLite is opened through `database/sql` with the pure-Go `modernc.org/sqlite`
driver. The connection pool is limited to one open connection.

### Users

A user has:

- Random `usr_...` ID.
- Case-insensitively unique name.
- Enabled state.
- Creation and update timestamps.

### API Keys

An API key has:

- Random `key_...` ID.
- Owning user ID.
- SHA-256 key hash.
- Display prefix and masked value.
- Enabled state.
- Creation, rotation, and last-used timestamps.

A partial unique index permits only one enabled key per user. Resetting a key
disables the old active key and inserts the replacement in one transaction.

## Usage Data Model

`usage_buckets` aggregates managed-user usage into 10-minute UTC buckets. The
logical bucket key contains:

- Bucket start.
- User ID.
- API key ID.
- Model.
- Reasoning effort.
- Service tier.
- OAuth auth file ID.

Counters are added with an SQLite upsert. New writes prune buckets older than
the 30-day retention window.

Snapshots and timeseries are read from the same table. Grafana consumes the
timeseries management API rather than reading SQLite directly.

## Trust and Secret Boundaries

The service handles three distinct secret types:

- Codex OAuth access and refresh tokens on disk.
- The optional remote `admin-api-key`.
- Generated managed user API keys.

OAuth files are read and refreshed in place. Managed plaintext keys are returned
only at creation or reset; only hashes and masked metadata are persisted.

The server itself provides HTTP. Listen address selection and transport
termination belong to the deployment environment.
