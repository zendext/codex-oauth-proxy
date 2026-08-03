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
| `internal/codexonly/auth_management.go` | Safe auth status, bounded management actions, binding clearing, and active-connection counts. |
| `internal/codexonly/auth_health.go` | Global credential health, model exclusions, cooldown reconciliation, and authoritative-state persistence. |
| `internal/codexonly/refresh.go` | OAuth refresh and outbound HTTP transport construction. |
| `internal/codexonly/failover.go` | Replay eligibility, upstream response classification, retry layers, and deterministic aggregate errors. |
| `internal/codexonly/models.go` | Per-auth runtime model synchronization, version LRU, aggregation, and embedded fallback. |
| `internal/codexonly/server.go` | Routing, authentication, headers, HTTP reverse proxy, and management/user handlers. |
| `internal/codexonly/chat_completions.go` | Chat Completions to Responses conversion and response translation. |
| `internal/codexonly/zed_edit_predictions.go` | Zed Qwen FIM validation, Responses conversion, and text-completion aggregation. |
| `internal/codexonly/user_store.go` | SQLite schema, users, API keys, and authentication. |
| `internal/codexonly/session_affinity.go` | Bounded signal extraction, digesting, persistent binding, renewal, and CAS rebind. |
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
6. Resolve the SQLite path, run idempotent schema migrations, validate the
   initial persisted user state, ensure tenant-scope digest metadata for
   affinity management, remove targets for auth identities that are no longer
   present, and restore unexpired authoritative auth health.
7. Start one `net/http` server.

The server sets `ReadHeaderTimeout` to 10 seconds. It does not set read or write
timeouts that would terminate established streaming or WebSocket traffic.
Graceful shutdown has a 10-second timeout and force-closes remaining HTTP
connections when that deadline expires.

## Storage Failure Lifecycle

SQLite is a process-level hard dependency. A database path, open, migration, or
initial state-load failure prevents startup.

At runtime, the first unexpected SQLite read or write failure becomes the
process-level fatal error. It cancels active proxy request contexts, closes both
sides of established WebSocket bridges, starts bounded HTTP shutdown, and
causes the server process to exit with an error. Concurrent later failures reuse
the first fatal error and do not start additional shutdowns.

Expected application errors do not trigger this lifecycle. These include
invalid input, missing records, disabled or invalid credentials, handled
constraint conflicts, and canceled request contexts.

The process does not provide degraded, in-memory-only, or automatic database
recovery. Production deployments must use systemd, Docker, Kubernetes, or
another external supervisor to restart the process after the underlying SQLite
problem has been corrected.

## OAuth Credential Flow

`FileAuthStore` recursively scans the configured directory on selection and
reconciles the result with the previous successful scan.

For every upstream request:

1. Parse Codex auth files, including disabled records for reconciliation.
2. Resolve stable identity from `account_id`, token claims, or a normalized path
   fallback.
3. Group duplicate files for one account into one logical credential.
4. Sort selectable logical credentials by stable identity.
5. Follow a valid tenant-scoped session binding, or select the next logical
   credential in round-robin order when no binding exists.
6. Treat credentials expiring within five minutes as expired.
7. Coordinate refresh in process by stable credential ID so one effective
   refresh serves concurrent callers for that credential.
8. Reuse a newer access token when another caller already replaced the token
   that expired or received `401`, but only while the same stable credential ID
   still exists.
9. Refresh an expired credential and reparse account and email claims.
10. Atomically replace the selected source file after syncing a same-directory
    `0600` temporary file.
11. Forward the request with the selected access token and account ID.

Reconciliation reports additions, removals, credential changes, metadata
changes, eligibility changes, and source-file changes without including token
contents. Stable account IDs survive file renames and same-account token
replacement. Replacing one path with another account produces an old-identity
removal and a new-identity addition. Disabled logical credentials remain known
but are excluded from new selection. A five-second parse-error grace retains the
last good representation during partial editor writes; persistently malformed
files are then excluded.

Each effective token refresh has one 30-second deadline and at most three total
attempts. Retries are limited to transient network failures and HTTP `408`,
`429`, `500`, `502`, `503`, and `504`; a valid `Retry-After` is honored only
within the refresh deadline. Missing refresh credentials, `invalid_grant`,
definitive `400`/`401`/`403` responses, malformed responses, and successful
responses without an access token are terminal. Refresh errors expose only safe
status and OAuth error-code context, never raw token-endpoint response bodies.

## Auth Management

The remote `/v0/management/*` and loopback `/v0/local-admin/*` route groups use
one internal auth-management implementation. Every response is marked
`Cache-Control: no-store`.

Status reconciles the auth directory, health registry, cached per-auth model
support, session-binding counts, and in-memory active connection counts. It
returns real account IDs, masked email, source counts, configuration and runtime
state, token and refresh timestamps, cooldown/error categories, model support
and exclusions, and logical binding/connection counts. Unidentified path-based
credentials are listed without exposing the fallback path and remain
read-only.

Force refresh enters the existing per-auth singleflight with a 30-second
deadline even when the auth is disabled. Successful refresh clears only
credential-related failures. Enable and disable validate every source first and
then reuse `Auth.Save` for synchronized temporary-file writes and atomic rename;
rollback is attempted for already written sources if a later source fails.
SQLite never becomes an alternate enablement source.

Cooldown clear removes only time-based quota and transient states. Credential
failures, disabled state, continued unauthorized state, and model exclusions
remain intact. Binding clear accepts only exact user/session, user, or account
scope. Exact clearing derives the tenant-scoped digest server-side, and all
scopes return a logical `deleted_count`; no global clear or target-auth
migration exists.

Active HTTP and SSE responses are counted until their upstream body closes.
Successful WebSocket bridges are counted until the bridge ends. Disabling an
auth changes later selection but does not cancel these established operations.

Every mutation performs a SQLite readiness check before external changes.
Subsequent storage errors still pass through the response commit guard, so a
request cannot return success while the process is entering fail-fast shutdown.

## Auth Health and Retry

Auth health is global to one stable logical credential in this single process.
All sessions skip credentials that are disabled, cooling down,
credential-invalid, continued-unauthorized, or excluded for the requested
model. Session affinity remains sticky while healthy and uses its existing
SQLite compare-and-swap rebind when failover is required. One request snapshots
the auth IDs known at its start, so a newly added replacement identity cannot
take over that in-flight execution.

The proxy classifies upstream outcomes before committing a downstream response:

- Request-scoped `4xx` responses stop without changing auth health.
- `401` performs one coordinated same-auth refresh and retry when replay is
  available; another `401` blocks that credential.
- `429` uses `Retry-After` or an explicit Codex quota reset deadline.
- Network failures, `408`, and retryable `5xx` responses create a short
  in-memory cooldown.
- Model-not-supported responses create a credential/model exclusion rather than
  an auth-global cooldown. Cacheable model IDs use a safe 128-byte identifier
  format, and each auth retains at most 64 exclusions with deterministic
  oldest-entry eviction.

Retry uses three independent budgets. Same-auth `401` repair is outside the
credential budget. One execution round tries distinct eligible auths up to
`max-retry-credentials`, where zero means all. After a round, the proxy waits
for the nearest cooldown only when it is within `max-retry-interval`, then
starts up to `request-retry` additional rounds. Context cancellation interrupts
selection, refresh, and cooldown waiting immediately.

Cross-auth retry requires an explicit replayable route. Eligible JSON requests
are buffered in memory up to and including 32 MiB; nothing is spooled to disk
for retry. Unknown-length, oversized, multipart, file, realtime,
side-effecting wham, hosted MCP, and unknown write requests remain one-shot.
Responses WebSocket handshakes can fail over before a successful upgrade.
HTTP streams and WebSockets never re-enter retry after downstream commitment.

Only explicit quota deadlines, `invalid_grant`, and continued unauthorized
state are stored in `auth_health_states`. Transient cooldowns and model
exclusions remain in memory. Credential-state fingerprints invalidate persisted
credential failures after real token material changes, while same-account token
updates preserve unexpired quota deadlines.

## Runtime Model Catalogs

`GET /v1/models` synchronizes the authenticated Codex `/models` endpoint for
every active stable auth ID and the normalized Codex CLI version. A cold or
expired request starts those per-auth fetches concurrently and waits for every
auth to resolve. Concurrent misses for the same auth and version share one
foreground fetch.

Successful per-auth snapshots remain fresh for three hours. The in-memory cache
keeps at most 16 normalized client versions and evicts the least recently used
version. Model payloads are never written to SQLite. Removed auth identities are
pruned from every cached version.

Aggregation exposes the union of model slugs. Duplicate slugs select one
complete canonical object by stable auth ID and deterministic model encoding;
fields from different accounts are never combined. A separate sorted support
set records which auth IDs advertised each slug. When a synchronized support set
exists for the request's Codex client version, routing preserves a healthy bound
auth only if it supports the requested model. Otherwise the existing affinity
CAS rebind moves the whole session to a healthy supporting auth. Model name
remains outside the affinity key.

A failed refresh reuses that auth's last successful snapshot when available.
Otherwise its unique models are absent from the current union. One deduplicated
background task performs up to three additional retries with exponential
backoff, jitter, and `Retry-After`. The catalog retry budget is independent from
proxy request retries. An exhausted cycle reopens only after the auth is proven
healthy, the next three-hour refresh period begins, or another client version
is requested.

Model fetch `401` responses use coordinated same-auth OAuth refresh and one
retry. Continued `401` and `429` responses update the shared auth health and
cooldown state but never mutate session bindings by themselves. If no auth has
a usable snapshot, the embedded release catalog is returned as the final
fallback; it does not claim per-auth support for routing.

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

## Session Affinity

Session affinity is enabled by default and stored in the same SQLite database
as managed users and usage.

The proxy accepts only these explicit signals:

- `Session-Id` or `Session_id` request headers.
- Top-level JSON `session_id` or `sessionId`.
- Top-level JSON `prompt_cache_key`.
- Top-level JSON `conversation_id` or `conversation.id`.

Signal values are trimmed, limited to 512 bytes, and rejected for affinity when
empty, invalid UTF-8, containing unpaired UTF-16 surrogate escapes, or containing
control characters. JSON request bodies are tokenized as a stream without an
affinity-specific body-size cutoff. Replay keeps up to 64 KiB in memory and uses
a `0600` temporary file beyond that threshold so the exact body can still be
forwarded. If temporary replay storage cannot be created or written, inspection
stops before additional unbounded buffering, body-derived signals are discarded,
and the stored prefix is joined with the untouched source stream for exact
one-shot forwarding. An invalid, missing, or oversized signal does not reject or
truncate the proxied request; it uses normal round-robin selection instead.

Managed requests are scoped by stable user ID, so API-key rotation preserves
bindings and two users cannot collide. OAuth compatibility requests are scoped
by the stable identity of the access token that authenticated the request.
Model name is not part of the affinity key.

The database stores only SHA-256 digests derived from tenant scope, signal type,
and normalized value, plus a separate SHA-256 tenant-scope digest used for
user-scoped management deletion. Prompt-cache, conversation, and session
aliases observed together share one digest-only binding group. The first
binding is inserted atomically; concurrent first requests follow the database
winner. Rebinding uses compare-and-swap so concurrent failovers converge
without overwriting another request's winner.

Bindings expire after one hour of inactivity. Active bindings renew only after
30 minutes since their previous persistence write, and expired rows are removed
in bounded batches. Bindings survive process restarts. Disabling an auth leaves
idle bindings intact; reuse while disabled rebinds to an active auth. Removing
or replacing an auth identity invalidates bindings that target the old identity.

## Route Selection

The HTTP handler checks project-owned routes before the reverse proxy whitelist.

| Route group | Owner |
| --- | --- |
| `/`, `/healthz` | Service handler |
| `/v0/local-admin/*` | Loopback administration |
| `/v0/management/*` | Admin-key user, usage, auth-status, recovery, and binding management API |
| `/v0/user/*` | Managed user self-service API |
| `/v1/models` | Runtime per-auth model aggregation with embedded fallback |
| `/v1/chat/completions` | Local protocol conversion |
| `/v1/zed/edit-predictions` | Local Zed Edit Prediction conversion |
| Whitelisted `/v1/*` | Codex upstream reverse proxy |
| Selected `/backend-api/*` | Codex CLI compatibility reverse proxy |

All unmatched routes return `404`; there is no general catch-all upstream
forwarder.

## Reverse Proxy Flow

For a whitelisted proxy request:

1. Authenticate the incoming managed key or permitted OAuth compatibility token.
2. Reject Fast service tiers when Fast mode is disabled.
3. Reconcile auth files and global health, apply synchronized model support when
   available, then resolve healthy session affinity or choose the next eligible
   credential.
4. Rewrite the target URL to the configured Codex or ChatGPT base.
5. Replace `Authorization` with the selected OAuth access token.
6. Add the ChatGPT account ID and compatibility headers when available.
7. Apply same-auth repair, distinct-credential failover, and bounded cooldown
   rounds only while the request is replayable and no client response is
   committed.
8. Update auth health and use affinity CAS when selection moves to another
   credential.
9. Forward one-shot requests once, forward HTTP streaming responses, or bridge
   WebSocket frames after a successful handshake.
10. Return deterministic safe aggregate errors when all candidates are
    exhausted.
11. Capture usage metadata for managed user requests.

The proxy preserves established HTTP streams during normal operation. WebSocket
forwarding uses Gorilla WebSocket and forces HTTP/1.1 ALPN for the upstream
upgrade path. Server shutdown or a fatal storage failure cancels established
streams and closes both WebSocket peers. No retry occurs after response
commitment or successful upgrade. Non-replayable request bodies keep the first
upstream response unchanged.

## Chat Completions Conversion

`/v1/chat/completions` is implemented locally rather than passed through:

1. Decode the Chat Completions JSON object.
2. Convert messages, tools, response format, reasoning, and service tier to a
   Responses request.
3. Force `stream: true` and `store: false` upstream.
4. Preflight the upstream SSE before downstream commitment. For non-stream
   clients, validate the complete upstream stream inside the retry attempt; for
   stream clients, validate the first event before committing headers or the
   assistant role chunk.
5. Feed pre-commit terminal errors into the same health-aware executor used by
   replayable Responses requests.
6. Read bounded Responses SSE events and require exactly one
   `response.completed` or `response.incomplete` terminal.
7. Reconcile indexed `response.output_item.done` snapshots with terminal output.
   Non-stream output uses the terminal snapshot as authoritative. Streaming
   emits only missing text or tool-argument suffixes and rejects conflicts.
8. Aggregate into a normal Chat Completions response or translate into Chat
   Completions SSE chunks. Post-commit failures emit one sanitized SSE error and
   never re-enter retry.
9. Apply Unicode-safe local stop filtering, continue draining terminal state and
   usage after a stop match, and record success, upstream failure, or client
   cancellation outcomes.

This is a focused compatibility layer, not a generic schema-preserving
translation engine.

## Zed Edit Prediction Conversion

`/v1/zed/edit-predictions` is another focused local conversion:

1. Authenticate only with a managed user API key and require JSON `POST`.
2. Validate `gpt-5.6-luna`, a positive `max_tokens` from 1 through 4096, and
   exactly one ordered Qwen
   `<|fim_prefix|>...<|fim_suffix|>...<|fim_middle|>` sequence.
3. Build a tool-free Responses request with low reasoning effort, `stream:
   true`, `store: false`, and separate prefix and suffix input text.
4. Use the same health-aware executor, model capability filtering, retry,
   session affinity, active-connection tracking, and managed usage accounting
   as other replayable Codex requests.
5. Require one valid `response.completed` or `response.incomplete` terminal,
   reject tool output and malformed or missing terminal state, and aggregate
   text and terminal usage before committing the client response.
6. Apply local stop matching and the output budget without forwarding sampling
   or token-limit fields upstream. Matching and truncation are Unicode-safe,
   and the upstream stream is fully drained before either is applied.
7. Return one OpenAI text-completion-shaped JSON response. Incomplete max-token
   reasons and local budget truncation use `finish_reason: "length"`;
   content filtering uses `content_filter`.

The local output cap counts Unicode code points. The route does not create a
generic `/v1/completions` contract and does not alter `/v1/chat/completions`.

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

### Session Affinity Bindings

A binding has:

- One or more tenant-scoped SHA-256 session digests.
- A digest-only alias group identifier.
- The selected stable OAuth credential ID.
- Creation, renewal, and expiry timestamps.

No raw session ID, prompt-cache key, conversation ID, managed API key, or model
name is stored in the binding table.

### Auth Health States

An authoritative persisted auth health row has:

- Stable OAuth credential ID.
- State kind and safe reason.
- Optional recovery deadline.
- Credential fingerprint for credential-related states.
- Safe upstream status and error code.
- Update timestamp.

The table never stores access tokens, refresh tokens, raw upstream bodies, or
short transport cooldowns.

## Usage Data Model

`usage_buckets` aggregates managed-user usage into 10-minute UTC buckets. The
logical bucket key contains:

- Bucket start.
- User ID.
- API key ID.
- Model.
- Reasoning effort.
- Service tier.
- Stable OAuth credential ID.

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
Raw session affinity signals are not written to SQLite, returned by management
APIs, or written to debug logs. A large JSON request body may be staged in a
process-owned `0600` temporary replay file for the lifetime of that request; the
file is removed when replay closes, including when another request-processing
step replaces the replay body. That temporary storage exists only for affinity
signal extraction. Cross-auth retry bodies are memory-only and capped at
32 MiB.

The server itself provides HTTP. Listen address selection and transport
termination belong to the deployment environment.
