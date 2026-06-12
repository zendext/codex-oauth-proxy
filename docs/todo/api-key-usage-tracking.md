# API Key Usage Tracking TODO

## Goal

Track Codex OAuth proxy usage by stored user API key after the User and API Key
Management API phase is implemented.

The first implementation should provide local statistics, user-visible today's
token totals, and threshold events. It must not enforce limits, block requests,
send Telegram messages, call webhooks, or add a Web UI.

## Dependency

This feature depends on `docs/todo/user-api-key-management-api.md`.

Usage records should be attributed through the authenticated `user_id` and
`api_key_id` produced by the user API key authentication path. Requests allowed
only through current Codex upstream access-token compatibility routes do not
have a stored user identity and should not be included in per-user usage totals.

## Product Scope

- Enable usage tracking by default once the management API foundation exists.
- Count Codex OAuth proxy traffic authenticated by stored user API keys.
- Attribute usage to both `user_id` and `api_key_id`.
- Provide a user API for today's token usage.
- Provide management APIs for usage snapshots and threshold events.
- Use local token accounting as an approximation for Codex subscription
  capacity.
- Track 5-hour and 7-day rolling windows.
- Record threshold-crossing events for later Telegram bot or external-service
  integration.

## Non-Goals

- No request blocking or local quota enforcement.
- No Telegram bot, webhook sender, or notification scheduler.
- No Web UI.
- No restoration of CPA management endpoints such as
  `/v0/management/api-key-usage` or `/v0/management/usage-queue`.
- No generic multi-provider usage abstraction.
- No config hot reload.

## Configuration Draft

```yaml
usage:
  enabled: true
  five-hour-reference-tokens: 0
  weekly-reference-tokens: 0
  alert-threshold: 0.8
  event-retention-days: 30
  debug-openai-response: false
```

- `enabled`: Enables built-in usage tracking. Default: `true`.
- `five-hour-reference-tokens`: Reference token capacity for the 5-hour window.
  This is not a hard limit. When `0`, return token totals without a ratio or
  threshold event for this window.
- `weekly-reference-tokens`: Reference token capacity for the 7-day window.
  This is not a hard limit. When `0`, return token totals without a ratio or
  threshold event for this window.
- `alert-threshold`: Ratio that creates a threshold event when crossed from
  below to above. Default: `0.8`.
- `event-retention-days`: Number of days to keep threshold events. Default:
  `30`.
- `debug-openai-response`: Enables safe response diagnostics when top-level
  `debug` is also true. Default: `false`.

Usage data should be stored in the same SQLite database configured by the
management API phase.

## Request Identity

Usage tracking should record data only when proxy authentication resolves a
stored user API key.

Required request identity fields:

- `user_id`
- `api_key_id`
- `key_hash`
- `masked_key`
- `auth_id`
- `request_id`

`auth_id` should use the existing auth file identity, such as `auth.json` or a
relative JSON filename, not an access token or account secret.

## Data Model Draft

Use 10-minute UTC buckets in SQLite.

### `usage_buckets`

Suggested identity and time fields:

- `bucket_start`: UTC timestamp rounded to a 10-minute boundary.
- `user_id`
- `api_key_id`
- `model`
- `auth_id`

Suggested counters:

- request count
- failed request count
- input tokens
- output tokens
- reasoning tokens
- cached input tokens
- cache read tokens, if present
- cache creation tokens, if present
- total tokens

Keep enough buckets for the 7-day window and today's user query. Derive:

- The 5-hour window from the most recent 30 buckets.
- The 7-day window from the most recent 1008 buckets.
- Today's usage from buckets whose timestamp falls on the current UTC day.

Prune buckets older than the required retention window.

### `usage_threshold_events`

Event fields:

- `id`
- `timestamp`
- `window`: `5h` or `7d`
- `user_id`
- `api_key_id`
- `key_hash`
- `masked_key`
- `ratio`
- `threshold`
- `total_tokens`
- `reference_tokens`
- `request_count`
- `failed_request_count`
- `model`
- `auth_id`
- `request_id`
- `diagnostics`

Prune events older than `event-retention-days`.

## Token Accounting

Token totals should come from safe response metadata when available.

For ordinary HTTP responses:

- Capture response status and selected safe headers.
- Parse OpenAI/Codex `usage` objects from JSON responses.
- For streaming JSON event responses, parse final usage-bearing events when
  present.

For Codex response WebSocket traffic:

- Codex CLI is expected to use WebSocket transport when
  `supports_websockets = true`.
- Request and failure counts alone are not enough for this feature.
- Before considering the feature complete, add a WebSocket-aware path that can
  inspect server-to-client text frames for usage-bearing JSON events while
  forwarding frames transparently.
- Never log or persist raw frame bodies.

If no token usage can be extracted for a request, count the request and failure
status but leave token increments at zero.

## Threshold Events

Create an event only when a key crosses the configured threshold from below to
above for a window. Do not repeatedly create events while the same key remains
above the threshold. If the key later falls below the threshold and crosses
again, create a new event.

Threshold state should be tracked per `api_key_id` and window.

## User API Draft

All endpoints are under `/v0/user` and require the user's own API key.

### `GET /v0/user/usage/today`

Return today's usage for the authenticated user and current API key.

The first implementation uses the current UTC day.

Response should include:

- `user_id`
- `api_key_id`
- `date`
- token totals
- request count
- failed request count

## Management API Draft

All endpoints are under `/v0/management` and require the admin API key.

### `GET /v0/management/usage`

Return a snapshot grouped by user and API key. Each entry should include:

- `user_id`
- user `name`
- `api_key_id`
- `key_hash`
- `masked_key`
- `windows.5h`
- `windows.7d`
- token totals per window
- request and failure counts per window
- ratio when the reference token capacity is configured
- whether the window is currently over the threshold

Optional filters:

- `user_id`
- `api_key_id`

### `GET /v0/management/usage/events?count=100`

Return recent threshold events, newest first.

- `count` defaults to `100`.
- Reject non-positive `count` values with HTTP 400.
- Apply a reasonable maximum to avoid unbounded responses.

## Safe Debug Logging

The feature should support enough diagnostics to debug Codex responses without
leaking secrets.

Never log:

- Full upstream response bodies.
- WebSocket frame bodies.
- Access tokens.
- Full client API keys.
- Full upstream `Authorization` headers.

When both `debug: true` and `usage.debug-openai-response: true` are set, write
safe structured diagnostics containing only:

- `request_id`
- `user_id`
- `api_key_id`
- `key_hash`
- `masked_key`
- `auth_id`
- `model`
- HTTP status
- `retry-after`
- safe OpenAI request ID headers, if present
- `usage_limit_reached` reset metadata, if present
- token usage summary

The same safe diagnostic fields can be copied into threshold events so a later
Telegram bot or external service can explain why an alert fired.

## Implementation Notes

- Keep implementation under `internal/codexonly` unless the server entrypoint
  needs config or startup wiring.
- Do not reintroduce CPA packages for this feature.
- Keep route behavior unchanged for existing Codex proxy endpoints.
- Prefer small, testable units:
  - request identity propagation
  - bucket aggregation and pruning
  - threshold crossing state
  - SQLite persistence
  - management API response shaping
  - user API response shaping
  - response usage extraction
  - WebSocket usage extraction

## Test Checklist

- Config defaults match the documented values.
- Codex OAuth requests with stored user API keys are counted.
- Requests authenticated only by Codex upstream access-token compatibility are
  not counted as user usage.
- Full API keys are never exposed by APIs, events, logs, or SQLite.
- Zero-token records do not affect quota ratios.
- Today's usage aggregates the expected UTC-day buckets.
- 5-hour and 7-day windows aggregate the expected buckets.
- Buckets older than the retention window are pruned.
- First threshold crossing creates one event.
- Remaining above the threshold does not create duplicate events.
- Falling below and crossing again creates a new event.
- Management usage endpoints require the admin API key.
- User usage endpoints require a valid stored user API key.
- `count <= 0` for the events endpoint returns HTTP 400.
- Ordinary HTTP response usage is extracted when present.
- Codex response WebSocket usage is extracted when present.
- WebSocket traffic still proxies transparently after usage inspection is added.
- Debug logs never expose response bodies, frame bodies, tokens, or full API
  keys.
- `go test ./...` passes.
- `go build -o test-output ./cmd/server && rm test-output` passes.
