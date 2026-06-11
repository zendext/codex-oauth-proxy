# Client API Key Usage Tracking TODO

## Goal

Track how much Codex OAuth capacity each configured proxy client API key consumes
when several people share the same Codex OAuth account through
`codex-oauth-proxy`.

The first implementation should provide local statistics and threshold events
only. It must not enforce limits, block requests, send Telegram messages, call
webhooks, or add a web UI.

## Current Project Context

`codex-oauth-proxy` is no longer a full CLIProxyAPI fork. The current runtime is
intentionally small:

- `cmd/server` starts one HTTP server.
- `internal/codexonly` owns configuration, Codex auth loading and refresh,
  request authorization, route mapping, and reverse proxy behavior.
- Codex OAuth credentials are loaded from `auth-dir`, defaulting to `~/.codex`.
- Client proxy API keys are configured by the top-level `api-keys` list.
- Supported upstream traffic is Codex/ChatGPT backend traffic only.
- There is no CPA provider registry, translator pipeline, executor usage
  reporter, SDK service, config watcher, management UI, or usage plugin system.

This TODO therefore replaces the old CPA-oriented `usage.Plugin` design with a
codex-only tracker inside `internal/codexonly`.

## Product Scope

- Enable the feature by default.
- Count Codex OAuth proxy traffic only. There is no separate provider filter in
  this project.
- Attribute usage to the matched client API key from `api-keys`.
- Ignore requests when `api-keys` is empty, because there is no configured client
  identity to aggregate.
- Use local token accounting as an approximation for Codex subscription capacity.
- Track 5-hour and 7-day windows.
- Record threshold-crossing events for later Telegram bot or external-service
  integration.
- Keep Telegram user to client API key mapping outside this project.

## Non-Goals

- No request blocking or local quota enforcement.
- No Telegram bot, webhook sender, or notification scheduler.
- No web page or management UI.
- No restoration of CPA management endpoints such as
  `/v0/management/api-key-usage` or `/v0/management/usage-queue`.
- No generic multi-provider usage abstraction.
- No config hot reload. The current server does not have a watcher, so config
  changes require restart.

## Configuration Draft

```yaml
debug: false

client-api-key-usage:
  enabled: true
  five-hour-reference-tokens: 0
  weekly-reference-tokens: 0
  alert-threshold: 0.8
  storage-file: ""
  event-retention-days: 30
  debug-openai-response: false
```

- `debug`: Top-level debug switch for optional diagnostic logs. Default: `false`.
- `enabled`: Enables the built-in usage tracker. Default: `true`.
- `five-hour-reference-tokens`: Reference token capacity for the 5-hour window.
  This is not a hard limit. When `0`, return token totals without a ratio or
  threshold event for this window.
- `weekly-reference-tokens`: Reference token capacity for the 7-day window. This
  is not a hard limit. When `0`, return token totals without a ratio or threshold
  event for this window.
- `alert-threshold`: Ratio that creates a threshold event when crossed from below
  to above. Default: `0.8`.
- `storage-file`: Optional JSON persistence path. Empty resolves to
  `<auth-dir>/client-api-key-usage.json`.
- `event-retention-days`: Number of days to keep threshold events. Default: `30`.
- `debug-openai-response`: Enables safe response diagnostics when top-level
  `debug` is also true. Default: `false`.

## Request Identity

Change request authorization from a boolean result to a matched client key
identity:

- Accept the same client token sources as today: `Authorization: Bearer ...` and
  `X-API-Key`.
- Match against configured `api-keys` using constant-time comparison.
- Return the configured key value that matched, not the arbitrary inbound header
  value.
- Hash the matched key with SHA-256 for storage and API output.
- Return only `key_hash` and `masked_key` in APIs, logs, and events.

When `api-keys` is empty, requests remain allowed as they are today, but usage is
not attributed to a client key.

## Data Model Draft

Add a small tracker under `internal/codexonly`, for example
`ClientAPIKeyUsageStore`.

Use 10-minute UTC buckets:

- Keep enough buckets for the 7-day window.
- Derive the 5-hour window from the most recent 30 buckets.
- Derive the 7-day window from the most recent 1008 buckets.
- Prune buckets older than the 7-day window.

Aggregate per key:

- request count
- failed request count
- input tokens
- output tokens
- reasoning tokens
- cached input tokens
- cache read tokens, if present
- cache creation tokens, if present
- total tokens

Persistence should be JSON written through a temporary file and atomic rename.
If loading the persistence file fails, log the error and start with an empty
state. Server startup should not fail because the usage file is damaged.

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
  inspect server-to-client text frames for usage-bearing JSON events while still
  forwarding frames transparently.
- Never log or persist raw frame bodies.

If no token usage can be extracted for a request, count the request and failure
status but leave token increments at zero.

## Threshold Events

Create an event only when a key crosses the configured threshold from below to
above for a window. Do not repeatedly create events while the same key remains
above the threshold. If the key later falls below the threshold and crosses
again, create a new event.

Event fields:

- `id`
- `timestamp`
- `window`: `5h` or `7d`
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

`auth_id` should use the existing auth file identity, such as `auth.json` or a
relative JSON filename, not an access token or account secret.

## Management API Draft

Add two JSON endpoints to the existing HTTP server. Protect them with the same
proxy API key authorization used by Codex routes.

### `GET /v0/management/client-api-key-usage`

Return a snapshot grouped by client API key. Each entry should include:

- `key_hash`
- `masked_key`
- `windows.5h`
- `windows.7d`
- token totals per window
- request and failure counts per window
- ratio when the reference token capacity is configured
- whether the window is currently over the threshold

### `GET /v0/management/client-api-key-usage/events?count=100`

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

When both `debug: true` and
`client-api-key-usage.debug-openai-response: true` are set, write safe
structured diagnostics containing only:

- `request_id`
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

- Keep all implementation under `internal/codexonly` unless the server entrypoint
  needs a config or startup change.
- Do not reintroduce CPA packages for this feature.
- Keep route behavior unchanged for existing Codex proxy endpoints.
- Keep the current `api-keys: []` local testing behavior unchanged.
- Prefer small, testable units:
  - config defaults and YAML parsing
  - client key matching, hashing, and masking
  - bucket aggregation and pruning
  - threshold crossing state
  - persistence load/save
  - management API response shaping
  - response usage extraction
  - WebSocket usage extraction

## Test Checklist

- Config defaults match the documented values.
- Codex OAuth requests with configured client API keys are counted.
- Requests are still allowed when `api-keys` is empty, but usage is not counted.
- Empty or unmatched client API keys are not counted.
- Full client API keys are never exposed by APIs, events, logs, or persistence.
- Zero-token records do not affect quota ratios.
- 5-hour and 7-day windows aggregate the expected buckets.
- Buckets older than the 7-day retention window are pruned.
- First threshold crossing creates one event.
- Remaining above the threshold does not create duplicate events.
- Falling below and crossing again creates a new event.
- Damaged persistence JSON does not crash startup.
- Management endpoints require valid proxy API key authorization when `api-keys`
  is configured.
- `count <= 0` for the events endpoint returns HTTP 400.
- Ordinary HTTP response usage is extracted when present.
- Codex response WebSocket usage is extracted when present.
- WebSocket traffic still proxies transparently after usage inspection is added.
- Debug logs never expose response bodies, frame bodies, tokens, or full API
  keys.
- `go test ./...` passes.
- `go build -o test-output ./cmd/server && rm test-output` passes.
