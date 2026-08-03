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
- JSON `/v1/zed/edit-predictions`.
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
clients. All management responses include `Cache-Control: no-store`.

### `GET /v0/management/auths`

Lists every logical Codex auth, including disabled and unidentified
credentials:

```json
{
  "auths": [
    {
      "account_id": "acct_xxx",
      "identity_state": "identified",
      "manageable": true,
      "email": "a***@example.com",
      "source_file_count": 2,
      "enabled": true,
      "runtime_state": "cooling",
      "runtime_reason": "quota",
      "token_expires_at": "2026-08-02T00:00:00Z",
      "last_refresh_at": "2026-07-31T00:00:00Z",
      "cooldown_until": "2026-08-01T01:00:00Z",
      "cooldown_reason": "quota",
      "model_capability_known": true,
      "known_supported_models": ["gpt-5.3-codex"],
      "model_exclusions": [],
      "session_binding_count": 3,
      "active_connection_count": 1,
      "last_error": {
        "code": "rate_limit_exceeded",
        "status": 429
      }
    }
  ]
}
```

`runtime_state` is `active`, `cooling`, or `unavailable`. Model exclusions are
reported separately because they apply to one model rather than the complete
auth. Supported models are included only when the runtime catalog has a known
per-auth support set.

An auth without a recoverable account ID is returned with
`identity_state: "unidentified"`, `manageable: false`, and no `account_id`.
It remains available for compatible proxy traffic but cannot receive
account-targeted mutations.

The response never includes OAuth tokens, raw auth JSON, source paths, raw
session identifiers, or raw upstream error bodies.

### `POST /v0/management/auths/refresh`

Forces the OAuth refresh flow for one identified auth:

```json
{"account_id":"acct_xxx"}
```

The action is allowed while the auth is disabled and always enters the
per-auth refresh singleflight. The OAuth operation has a 30-second overall
deadline. Success clears credential-related failures, but it does not enable
the auth, clear quota/model cooldowns, or change session bindings.

Response:

```json
{"auth": { "...": "updated safe auth status" }}
```

Token endpoint and persistence failures return sanitized errors without token
or upstream body content.

### `POST /v0/management/auths/enable`

### `POST /v0/management/auths/disable`

Request:

```json
{"account_id":"acct_xxx"}
```

These actions update `disabled` in every auth file for the account through the
same synchronized temporary-file and atomic-rename path used by refresh.
Every source is parsed and validated before writes begin. A failed multi-file
write rolls back already changed sources when possible and never returns a
successful mixed-state response.

Enable performs no OAuth request and does not clear health, cooldown, model
exclusions, active requests, or session bindings. Disable excludes the auth
from new selection after the action completes. Existing HTTP, SSE, and
WebSocket requests continue, and idle bindings remain until later reuse,
expiry, explicit clearing, or auth removal.

### `POST /v0/management/auths/cooldown/clear`

Request:

```json
{"account_id":"acct_xxx"}
```

Response:

```json
{
  "cleared": true,
  "auth": { "...": "updated safe auth status" }
}
```

Only time-based quota/`429`, network, `408`, and retryable `5xx` cooldowns can
be cleared. The action does not clear disabled state, `invalid_grant`, missing
or invalid refresh credentials, continued unauthorized state, or
model-specific exclusions. In those cases `cleared` is `false`.

### `POST /v0/management/session-bindings/clear`

The request must select exactly one scope.

One raw session key for one user:

```json
{
  "user_id": "usr_xxx",
  "session_key": "raw-session-key"
}
```

All bindings for one user:

```json
{"user_id":"usr_xxx"}
```

User scope requires `session_key` to be omitted. A present empty, whitespace,
or otherwise invalid `session_key` returns `400` and does not clear bindings.

All bindings targeting one identified auth:

```json
{"account_id":"acct_xxx"}
```

Response:

```json
{"deleted_count":2}
```

For exact-session clearing, the server computes all supported tenant-scoped
session digests from the raw key and deletes the complete alias group. The raw
key is never returned, logged, or persisted. There is no unconditional global
clear and no action that migrates a session to a selected auth.

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

Without `client_version`, returns an OpenAI-style view of the synchronized
catalog for the version derived from `codex-user-agent`:

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

When the `client_version` query key is present, the response uses the Codex CLI
catalog shape for that exact normalized version. An empty value uses the same
configured User-Agent version:

```json
{"models":[]}
```

On a cold or expired version, the proxy concurrently fetches the authenticated
upstream catalog for every active logical credential, waits for all results, and
returns the deterministic union of model slugs. One complete per-auth object is
selected for duplicate slugs; fields are not merged across accounts.

Successful per-auth snapshots have a three-hour TTL. Failed refreshes reuse
stale snapshots and schedule one deduplicated background task with three
additional retries. One failed auth does not block successful auths. If no auth
has a usable snapshot, the response falls back to
`internal/codexonly/codex_client_models.json`.

Versions are limited to 64 safe ASCII bytes, and invalid values return `400`.
At most 16 normalized versions are retained in memory with LRU eviction. Fast
tier metadata is removed unless `allow-fast-mode` is enabled.

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

The converter requires exactly one successful upstream terminal event.
`response.completed` is a complete success. `response.incomplete` is a partial
success with these standard Chat Completions finish reasons:

| Incomplete reason | `finish_reason` |
| --- | --- |
| `max_tokens`, `max_output_tokens` | `length` |
| `content_filter` | `content_filter` |
| unknown | `length` |

An incomplete reason takes precedence over `tool_calls`. The response does not
include a non-standard `native_finish_reason` field.

For streaming requests, the proxy waits for the first valid upstream event
before committing the downstream SSE response. A failure before commitment is
classified by the same OAuth refresh, health cooldown, model failover, and
request-error rules as an HTTP failure. A failure after commitment emits one
sanitized OpenAI error envelope as a `data:` event, closes the stream, and does
not emit a finish chunk or `[DONE]`.

EOF before a successful terminal, malformed or oversized events, duplicate
terminals, and data after a terminal are failures. One upstream SSE event is
limited to 50 MB. `response.output_item.done` snapshots are reconciled by output
index with terminal output; non-stream terminal output is authoritative, while
streaming can emit only a missing suffix. Local stop matching is UTF-8 safe
across event boundaries and suppresses later text and tool deltas while the
proxy continues draining terminal state and usage.

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

### `POST /v1/zed/edit-predictions`

This managed-key-only endpoint accepts the Completion request shape emitted by
Zed's `open_ai_compatible_api` edit prediction provider. It is a dedicated
compatibility contract, not a generic `/v1/completions` endpoint.

The request must use `Content-Type: application/json` and include:

- `model`: exactly `gpt-5.6-luna`.
- `prompt`: exactly one ordered Qwen FIM sequence:

  ```text
  <|fim_prefix|>{prefix}<|fim_suffix|>{suffix}<|fim_middle|>
  ```

The prompt is rejected before upstream contact when a marker is missing,
duplicated, out of order, preceded by other text, or followed by other text.

Optional fields:

- `max_tokens`: an integer from 1 through 4096; default `256`. It is enforced
  locally as a Unicode code-point output cap and is not forwarded upstream.
- `temperature`: a JSON number accepted for Zed wire compatibility but not
  forwarded upstream.
- `stop`: one non-empty string or a list of non-empty strings. Matching is
  applied locally and is Unicode-safe.

The proxy extracts the prefix and suffix, sends a tool-free low-reasoning
Responses request with `stream: true` and `store: false`, and fully validates
and aggregates the upstream SSE before returning:

```json
{
  "id": "cmpl_...",
  "object": "text_completion",
  "created": 0,
  "model": "gpt-5.6-luna",
  "choices": [
    {
      "index": 0,
      "text": "missing code",
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 0,
    "completion_tokens": 0,
    "total_tokens": 0
  }
}
```

`response.completed` returns `finish_reason: "stop"` unless a local output
budget truncates first. `response.incomplete` maps max-token reasons to
`length`, content filtering to `content_filter`, and unknown reasons to
`length`. A local budget truncation also uses `length`; a local stop match uses
`stop`.

Stop or budget matches do not cancel the upstream read. The proxy continues
draining the terminal event and usage first. EOF before a terminal, malformed
or oversized events, duplicate terminals, data after a terminal, tool output,
and upstream failure events return deterministic safe errors. Client
cancellation stops the upstream request without retrying or synthesizing a new
response body.

## Responses and Compatibility Routes

The proxy forwards these whitelisted Codex routes without defining their full
upstream request schema:

These native Responses HTTP and WebSocket routes remain transparent; the Chat
Completions terminal validation and error conversion described above do not
modify their event or frame payloads.

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
