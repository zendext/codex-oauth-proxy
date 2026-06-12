# User and API Key Management API TODO

## Goal

Add an API-only user and API key management layer for `codex-oauth-proxy`.
This is the prerequisite for per-user and per-key usage accounting.

The first implementation must not add a Web UI, login page, browser session,
registration flow, quota enforcement, request blocking, Telegram integration,
or webhook integration.

## Relationship to Usage Tracking

This document owns identity and API key management. Usage tracking must be
implemented in a later phase and should depend on the user and API key identity
returned by this phase.

The later usage phase should add the user API for today's token usage and the
management APIs for usage summaries and threshold events.

## Current Project Context

`codex-oauth-proxy` is currently a small Codex-only reverse proxy:

- `cmd/server` starts one HTTP server.
- `internal/codexonly` owns config loading, Codex auth parsing and refresh,
  route whitelisting, proxy authorization, and reverse proxy behavior.
- Codex OAuth credentials are loaded from `auth-dir`, defaulting to `~/.codex`.
- Static proxy API keys are configured by the top-level `api-keys` list.
- There is no persistent application database, management API, user model,
  Web UI, login flow, provider registry, or plugin host.

## Product Scope

- Provide API-only user and API key management.
- Authenticate management APIs with one configured admin API key.
- Store users and generated API keys in SQLite.
- Let a user's API key authenticate both Codex proxy requests and user APIs.
- Resolve proxy requests authenticated by a stored user API key to `user_id`
  and `api_key_id`.
- Preserve existing local testing behavior where `api-keys: []` allows proxy
  requests without authentication.
- Keep existing static `api-keys` support as a legacy proxy-only path.

## Non-Goals

- No Web UI, login page, browser session, or user password flow.
- No self-registration.
- No user roles beyond enabled or disabled.
- No per-user quota enforcement.
- No token usage counters in this phase.
- No request blocking based on usage.
- No notification delivery.
- No config hot reload.

## Configuration Draft

```yaml
admin-api-key: ""

database:
  path: ""
```

- `admin-api-key`: Secret used to authenticate management APIs. Empty disables
  management APIs.
- `database.path`: Optional SQLite path. Empty resolves to
  `<auth-dir>/codex-oauth-proxy.db`.

Existing `api-keys` remain supported. They are accepted for proxy
authorization, but they are not attached to a user, cannot authenticate user
APIs, and do not create a `user_id` or `api_key_id` for later usage tracking.

## Authentication Model

### Management API Authentication

Management APIs require:

```http
Authorization: Bearer <admin-api-key>
```

The same token may also be accepted from `X-API-Key` for consistency with
existing proxy key handling.

Management APIs must not accept:

- User API keys.
- Static proxy `api-keys`.
- Codex upstream access tokens.

When `admin-api-key` is empty, management endpoints should return `404 Not
Found` without exposing unauthenticated management behavior.

### User API Authentication

User APIs require a currently enabled user API key. The same API key also works
for Codex proxy routes.

The key may be supplied through:

- `Authorization: Bearer <user-api-key>`
- `X-API-Key: <user-api-key>`

Authentication succeeds only when:

- The key hash exists in SQLite.
- The key row is enabled.
- The owning user is enabled.

User APIs must not accept the admin API key, static proxy `api-keys`, or Codex
upstream access tokens.

### Proxy Route Authentication

Proxy route authorization should support three cases:

- `api-keys: []`: keep today's unauthenticated local testing behavior.
- Static `api-keys`: accept configured static keys as proxy-only credentials.
- Stored user API keys: accept enabled user keys and return the matched
  `user_id` and `api_key_id` for downstream usage tracking.

When a request is allowed because `api-keys: []` and no valid user API key is
present, the request has no user identity and should not be counted by later
per-user usage tracking.

## Data Model Draft

Use SQLite with automatic startup migrations.

### `users`

- `id`: Stable generated ID.
- `name`: Required display name. Trim whitespace. Names should be unique
  case-insensitively after trimming.
- `enabled`: Boolean. Disabled users cannot use any user API key.
- `created_at`: UTC timestamp.
- `updated_at`: UTC timestamp.

### `api_keys`

- `id`: Stable generated ID.
- `user_id`: Owning user ID.
- `key_hash`: SHA-256 hash of the generated key, unique.
- `key_prefix`: Non-secret prefix for identification.
- `masked_key`: Safe display value such as `cop_abc...xyz`.
- `enabled`: Boolean. Disabled keys cannot authenticate.
- `created_at`: UTC timestamp.
- `rotated_at`: UTC timestamp, set when the key was created by reset.
- `last_used_at`: Nullable UTC timestamp updated after successful auth.

The first version should keep one active API key per user. Resetting a key
should disable previous keys for that user and insert a new key row so old
usage rows can still reference the old `api_key_id` in the later usage phase.

## API Key Generation and Storage

- Generate keys from at least 32 random bytes and encode them with a
  recognizable prefix, for example `cop_`.
- Return the plaintext key only in create-user and reset-key responses.
- Never persist plaintext keys.
- Never log plaintext keys.
- Compare candidate keys through hashes and constant-time comparison where
  practical.
- API responses should use `key_prefix`, `masked_key`, and key IDs for display.

## Management API Draft

All endpoints are under `/v0/management` and require the admin API key.

### `POST /v0/management/users`

Create a user and initial API key.

Request:

```json
{
  "name": "alice",
  "enabled": true
}
```

Response includes the created user, API key metadata, and the plaintext API key
once.

Validation:

- Reject empty names with HTTP 400.
- Reject duplicate names with HTTP 409.
- Default `enabled` to `true` when omitted.

### `GET /v0/management/users`

Return users sorted by name ascending with their current API key metadata.

Optional filters:

- `enabled=true`
- `enabled=false`

### `GET /v0/management/users/{user_id}`

Return one user and current API key metadata.

### `PATCH /v0/management/users/{user_id}`

Update mutable user fields:

```json
{
  "name": "alice-renamed",
  "enabled": false
}
```

Changing `enabled` immediately affects user API and proxy authentication.
Disabling a user should not delete or rotate the user's API key.

### `POST /v0/management/users/{user_id}/api-key/reset`

Disable the user's previous active key, create a replacement key, and return
the plaintext replacement key once.

## User API Draft

All endpoints are under `/v0/user` and require the user's own API key.

### `GET /v0/user/api-key`

Return the authenticated user and current API key metadata.

Response must not include the plaintext key.

### `POST /v0/user/api-key/reset`

Reset the authenticated user's own API key. The old key authenticates the reset
request and is invalid after the response is created.

Response includes the plaintext replacement key once.

### Future Usage Endpoint

`GET /v0/user/usage/today` is intentionally deferred to the usage tracking
phase.

## Error Handling

- Return HTTP 401 for missing or invalid credentials.
- Return HTTP 403 when credentials are valid but the user or key is disabled.
- Return HTTP 404 for missing users.
- Return HTTP 400 for invalid request bodies or empty names.
- Return HTTP 409 for duplicate user names.
- Do not leak whether a specific API key hash exists.
- Do not include secrets in error responses.

## Implementation Notes

- Keep implementation under `internal/codexonly` unless the server entrypoint
  needs config or startup wiring.
- Add a small storage boundary for SQLite operations.
- Use context-aware database calls.
- Apply migrations idempotently at startup.
- Keep route behavior unchanged for existing Codex proxy endpoints.
- Do not add read/write timeouts that can cut off upstream streaming or
  WebSocket traffic.

## Test Checklist

- Config defaults resolve the database path under `auth-dir`.
- Empty `admin-api-key` does not expose management APIs.
- Management endpoints require the configured admin API key.
- Management endpoints reject user API keys and static proxy keys.
- Creating a user creates exactly one active API key and returns plaintext once.
- Duplicate user names are rejected.
- Disabled users cannot call user APIs.
- Disabled users cannot use proxy routes through stored user API keys.
- Re-enabled users can use their current stored API key again.
- User API key reset disables the old key and returns a new plaintext key once.
- Old user API keys stop authenticating after reset.
- Static `api-keys` still authenticate proxy routes.
- Static `api-keys` cannot call user APIs.
- `api-keys: []` still allows local proxy requests without authentication.
- Full API keys are never exposed by list/detail responses, logs, or SQLite.
- `go test ./...` passes.
- `go build -o test-output ./cmd/server && rm test-output` passes.
