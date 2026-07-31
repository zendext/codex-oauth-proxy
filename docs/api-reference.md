# API Reference

[English](api-reference.md) | [简体中文](zh-CN/api-reference.md)

This reference documents the API behavior owned by `codex-oauth-proxy` and the
supported proxy route surface. It does not reproduce the complete upstream
Responses API schema.

## Base URL

Examples use:

```text
http://127.0.0.1:8317
```

The OpenAI-compatible client base URL is:

```text
http://127.0.0.1:8317/v1
```

## Authentication

The server reads candidate credentials from:

```http
Authorization: Bearer <token>
```

and:

```http
X-API-Key: <token>
```

When both are present, either matching credential can authenticate the request.

| Route group | Credential |
| --- | --- |
| `/`, `/healthz` | None |
| `/v1/*` | Managed `cop_...` user API key |
| `/v0/user/*` | Managed `cop_...` user API key |
| `/v0/management/*` | Configured `admin-api-key` |
| `/v0/local-admin/*` | Loopback source address; no key |
| Selected `/backend-api/*` compatibility routes | Managed user API key; some routes also accept a currently loaded Codex access token |

Disabled users receive `403`. Missing, unknown, or rotated managed keys receive
`401`.

## Session Affinity

For proxied requests, the server accepts `Session-Id` or `Session_id` headers
and the JSON fields `session_id`, `sessionId`, `prompt_cache_key`,
`conversation_id`, and `conversation.id`.

Valid signals keep one logical session on the same available OAuth credential
across model changes, API-key rotation, concurrent first requests, and process
restarts. Managed bindings are isolated by user. Compatibility-token bindings
are isolated by the matched stable OAuth identity.

Values over 512 bytes, empty values, invalid UTF-8, unpaired UTF-16 surrogate
escapes, and values containing control characters are ignored for affinity.
JSON bodies are inspected as a token stream without an affinity-specific
body-size cutoff, and the exact body is restored before forwarding. If temporary
replay storage cannot be created or written, body inspection stops, body-derived
signals are ignored, and the stored prefix plus untouched request stream are
forwarded unchanged. Ignored or missing signals do not fail the request;
selection falls back to normal round-robin behavior.

## Health-Aware Failover

Auth health is global across sessions. A healthy affinity binding stays on its
current Codex credential; a binding whose credential is disabled, cooling,
credential-invalid, continued-unauthorized, or unable to serve the requested
model is rebound with compare-and-swap failover.

Cross-credential retry is available only for:

- Read-only `GET` and `HEAD` requests on whitelisted routes.
- JSON `/v1/chat/completions`.
- Responses, Responses compact, alpha search, JSON image generation, and trace
  summarization.
- A Responses WebSocket handshake before successful upgrade.

Replayable request bodies are buffered in memory up to and including 32 MiB.
Unknown-length or larger bodies and multipart, file, realtime, side-effecting
wham, hosted MCP, and unknown write requests are sent once. They are not
rejected merely because automatic replay is unavailable.

Within one replayable execution, `401` first refreshes and retries the same
credential once. A credential round then tries distinct eligible credentials.
After a round, the proxy may wait for the nearest cooldown and start another
round according to `request-retry`, `max-retry-credentials`, and
`max-retry-interval`. Cancellation stops waiting immediately. No retry occurs
after downstream response bytes or a successful WebSocket upgrade.

## Error Format

Project-owned handlers return:

```json
{
  "error": {
    "message": "invalid API key",
    "type": "Unauthorized"
  }
}
```

Proxied upstream routes may preserve upstream status codes, headers, and response
bodies instead. Request-scoped upstream `4xx` errors stop immediately and do
not penalize a credential.

When replayable candidates are exhausted, the proxy returns a safe aggregate
error:

```json
{
  "error": {
    "message": "upstream Codex service unavailable",
    "type": "proxy_error",
    "code": "upstream_unavailable"
  }
}
```

Deterministic aggregate outcomes are:

| Status | Code | Meaning |
| --- | --- | --- |
| `404` | `model_not_found` | Every known candidate is excluded for the requested model. |
| `429` | `rate_limited` | All serviceable candidates are quota-limited, or mixed failures have a clear near-term quota recovery. The response includes the earliest known `Retry-After`. |
| `503` | `auth_unavailable` | All candidates are disabled, credential-invalid, or continued-unauthorized. |
| `502` | `upstream_unavailable` | Network/retryable upstream failures were exhausted, including mixed failures without a near-term recovery deadline. |

Client cancellation does not synthesize a new proxy error.

## Service Endpoints

### `GET /`

Returns:

```json
{"message":"codex-oauth-proxy"}
```

### `GET /healthz`

Returns:

```json
{"status":"ok"}
```

The health endpoint checks that the HTTP handler is running. It does not perform
an upstream Codex request.

## Management API

Remote management is available only when `admin-api-key` is non-empty.
Equivalent local routes are available below `/v0/local-admin` to loopback
clients.

### `POST /v0/management/users`

Creates a user and initial API key.

Request:

```json
{
  "name": "alice",
  "enabled": true
}
```

`enabled` is optional and defaults to `true`.

Response status: `201 Created`

```json
{
  "user": {
    "id": "usr_xxx",
    "name": "alice",
    "enabled": true,
    "created_at": "2026-07-30T00:00:00Z",
    "updated_at": "2026-07-30T00:00:00Z"
  },
  "api_key": {
    "id": "key_xxx",
    "user_id": "usr_xxx",
    "key_prefix": "cop_...",
    "masked_key": "cop_...abcd",
    "enabled": true,
    "created_at": "2026-07-30T00:00:00Z"
  },
  "api_key_value": "cop_plaintext_returned_once"
}
```

Possible errors:

- `400` for an empty or invalid name.
- `409` for a case-insensitive duplicate name.

### `GET /v0/management/users`

Lists users and their active key metadata.

Optional query:

```text
enabled=true
enabled=false
```

Response:

```json
{
  "users": [
    {
      "user": {
        "id": "usr_xxx",
        "name": "alice",
        "enabled": true,
        "created_at": "2026-07-30T00:00:00Z",
        "updated_at": "2026-07-30T00:00:00Z"
      },
      "api_key": {
        "id": "key_xxx",
        "user_id": "usr_xxx",
        "key_prefix": "cop_...",
        "masked_key": "cop_...abcd",
        "enabled": true,
        "created_at": "2026-07-30T00:00:00Z"
      }
    }
  ]
}
```

### `GET /v0/management/users/{user_id}`

Returns one user and active key metadata. Returns `404` when the user does not
exist.

### `PATCH /v0/management/users/{user_id}`

Updates either or both fields:

```json
{
  "name": "alice2",
  "enabled": false
}
```

Returns the updated user and active key metadata.

### `POST /v0/management/users/{user_id}/api-key/reset`

Disables the previous active key and returns one new key.

Response status: `200 OK`

The response shape matches user creation and includes `api_key_value` once.

### `GET /v0/management/usage`

Returns rolling usage snapshots.

Optional queries:

```text
user_id=usr_xxx
api_key_id=key_xxx
```

Response:

```json
{
  "usage": [
    {
      "user_id": "usr_xxx",
      "name": "alice",
      "api_key_id": "key_xxx",
      "masked_key": "cop_...abcd",
      "windows": {
        "5h": {
          "request_count": 1,
          "total_tokens": 100
        },
        "7d": {
          "request_count": 2,
          "total_tokens": 200
        }
      },
      "models": []
    }
  ]
}
```

Counter objects can also contain:

- `failed_request_count`
- `input_tokens`
- `output_tokens`
- `reasoning_tokens`
- `cached_input_tokens`
- `cache_read_tokens`
- `cache_creation_tokens`

### `GET /v0/management/usage/timeseries`

Query parameters:

| Parameter | Default | Values |
| --- | --- | --- |
| `window` | `7d` | `5h`, `24h`, `7d`, `30d`, `today` |
| `step` | `auto` | `10m`, `30m`, `1h`, `6h`, `1d` |
| `group_by` | `user` | Repeat or comma-separate `user`, `api_key`, `model`, `reasoning_effort`, `service_tier` |
| `fill` | none | `none`, `zero` |
| `user_id` | empty | One user ID |
| `api_key_id` | empty | One API key ID |

Response:

```json
{
  "window": "7d",
  "step": "1h",
  "start": "2026-07-23T00:00:00Z",
  "end": "2026-07-30T00:10:00Z",
  "group_by": ["user"],
  "series": [
    {
      "bucket_start": "2026-07-30T00:00:00Z",
      "user_id": "usr_xxx",
      "name": "alice",
      "request_count": 1,
      "total_tokens": 100
    }
  ]
}
```

Invalid window, step, grouping, fill, or filter combinations return `400`.

## User API

These routes require the managed user key whose data is being accessed.

### `GET /v0/user/api-key`

Returns the authenticated user and current key metadata. Plaintext is never
returned.

### `POST /v0/user/api-key/reset`

Rotates the authenticated user's key and returns `api_key_value` once. The key
used for the reset request is disabled immediately.

### `GET /v0/user/usage/today`

Returns totals from `00:00:00` UTC through the current usage bucket:

```json
{
  "user_id": "usr_xxx",
  "api_key_id": "key_xxx",
  "date": "2026-07-30",
  "request_count": 2,
  "input_tokens": 100,
  "output_tokens": 50,
  "total_tokens": 150,
  "models": []
}
```

## OpenAI-Compatible API

### `GET /v1/models`

Without `client_version`, returns an OpenAI-style model list:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5.4",
      "object": "model",
      "owned_by": "openai"
    }
  ]
}
```

When the `client_version` query key is present, the response uses the embedded
Codex CLI model catalog shape:

```json
{"models":[]}
```

The actual list comes from
`internal/codexonly/codex_client_models.json`. Fast tier metadata is removed
unless `allow-fast-mode` is enabled.

### `POST /v1/chat/completions`

This endpoint translates Chat Completions input to an upstream Responses
request. The proxy always requests an upstream stream, then either aggregates it
or converts it back to Chat Completions SSE.

Required fields:

- `model`
- `messages`

Supported behavior includes:

- `system`, `developer`, `user`, `assistant`, and `tool` messages.
- Text and `image_url` message content.
- Function tools, tool choice, parallel tool calls, tool-call history, and tool
  outputs.
- `stream` and `stream_options.include_usage`.
- `response_format` with `json_object` or `json_schema`.
- `reasoning`, `reasoning_effort`, and `extra_body.reasoning`.
- `verbosity`.
- `stop` as a string or list.
- `service_tier`.

Reasoning effort aliases are normalized:

| Input | Upstream value |
| --- | --- |
| `minimal` | `low` |
| `max` | `xhigh` |

Sampling and other unknown Chat Completions fields are not automatically
forwarded. Clients that require full Responses behavior should call
`/v1/responses` directly.

Example:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer cop_...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      {"role": "user", "content": "Hello"}
    ],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

## Responses and Compatibility Routes

The proxy forwards these whitelisted Codex routes without defining their full
upstream request schema:

| Common method | Public path |
| --- | --- |
| `POST`, `GET`, WebSocket upgrade | `/v1/responses` |
| `POST` | `/v1/responses/compact` |
| `POST` | `/v1/alpha/search` |
| `POST` | `/v1/images/generations` |
| `POST` | `/v1/images/edits` |
| `POST` | `/v1/memories/trace_summarize` |
| `POST` | `/v1/realtime/calls` |
| `GET`, WebSocket upgrade | `/v1/realtime` |

Image generation is normally available through the Responses API image tool.
The two `/v1/images/*` paths are narrow compatibility routes; other image paths
are not proxied.

When Fast mode is disabled, requests containing `service_tier: "fast"` or
`"priority"` are rejected before upstream forwarding.

## Codex CLI Internal Compatibility

The following paths exist only to support known Codex CLI behavior and are not a
stable third-party API contract:

- `/backend-api/codex` aliases of the whitelisted `/v1` Codex routes.
- `POST /files`
- `POST /files/{file_id}/uploaded`
- `/backend-api/wham/usage`
- `/backend-api/wham/profiles/me`
- `/backend-api/wham/accounts/check`
- `/backend-api/wham/accounts/send_add_credits_nudge_email`
- `/backend-api/wham/apps`
- `/backend-api/ps/mcp`

The proxy does not expose arbitrary `/backend-api/*` paths. General clients
should use the supported `/v1/*` and `/v0/*` surfaces.
