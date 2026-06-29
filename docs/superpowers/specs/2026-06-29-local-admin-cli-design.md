# Local Admin CLI Design

## Goal

Add a zero-config admin CLI for common `codex-oauth-proxy` operations without
requiring operators to pass `--config` for routine management tasks.

The CLI should manage the currently running local proxy service by default, keep
the existing admin API key flow for remote or dashboard callers, and avoid
direct SQLite access from the CLI.

## Compatibility

- Keep the current server startup behavior:
  - `codex-oauth-proxy --config config.yaml`
  - `codex-oauth-proxy`
- Add an explicit server subcommand:
  - `codex-oauth-proxy serve --config config.yaml`
- Keep `--local-model` as a compatibility no-op for server startup.
- Do not require config loading for `admin` commands unless a future command
  explicitly needs it.

## CLI Shape

Admin commands default to `http://127.0.0.1:8317` and support `--url` for
non-default local instances:

```bash
codex-oauth-proxy admin users list
codex-oauth-proxy admin users create alice
codex-oauth-proxy admin users get usr_xxx
codex-oauth-proxy admin users update usr_xxx --name alice2
codex-oauth-proxy admin users enable usr_xxx
codex-oauth-proxy admin users disable usr_xxx
codex-oauth-proxy admin users reset-key usr_xxx

codex-oauth-proxy admin usage snapshot
codex-oauth-proxy admin usage snapshot --user-id usr_xxx
codex-oauth-proxy admin usage snapshot --api-key-id key_xxx
codex-oauth-proxy admin usage timeseries --window 7d --step 1h --group-by user
codex-oauth-proxy admin usage timeseries --window 30d --step 1d --group-by user --fill zero

codex-oauth-proxy admin users list --url http://127.0.0.1:8318
```

All admin commands support `--json`. Human-readable output is the default.

## Server API

Add a local-only admin route prefix:

```text
/v0/local-admin/...
```

This route reuses the existing management handlers and response shapes, but uses
local request authorization instead of the configured `admin-api-key`.

Local authorization rules:

- Accept only loopback clients, including `127.0.0.1` and `::1`.
- Reject non-loopback clients without exposing management behavior.
- Do not change `/v0/management/...`; it remains protected by `admin-api-key`.
- Keep Grafana and remote management API callers on `/v0/management/...`.

The local route should support the same management resources needed by the CLI:

- `POST /v0/local-admin/users`
- `GET /v0/local-admin/users`
- `GET /v0/local-admin/users/{user_id}`
- `PATCH /v0/local-admin/users/{user_id}`
- `POST /v0/local-admin/users/{user_id}/api-key/reset`
- `GET /v0/local-admin/usage`
- `GET /v0/local-admin/usage/timeseries`

## Output

User commands should display compact tables by default. User creation and key
reset commands must show the plaintext API key once, matching the existing API
behavior.

Usage snapshot default output should show one row per user/API key with rolling
window totals:

```text
USER   USER_ID   API_KEY   REQ_5H   TOKENS_5H   REQ_7D   TOKENS_7D
alice  usr_xxx   key_xxx   1        20          1        20
```

`--json` returns the complete server payload, including model breakdowns.

Timeseries default output should show bucketed totals with grouping columns
included when requested. `--json` returns the complete timeseries payload.

## Error Handling

- If the server is not reachable, report the URL and suggest starting the
  service or passing `--url`.
- If the local route rejects the request, report that local admin is only
  available from loopback.
- Preserve existing management API validation errors for invalid users, duplicate
  names, invalid filters, and invalid usage query parameters.
- Do not print full API keys except in create and reset-key responses.
- Do not print admin API keys or upstream OAuth tokens.

## Testing

- Cover legacy server startup argument parsing.
- Cover `serve` subcommand startup argument parsing.
- Cover local admin route acceptance from loopback.
- Cover local admin route rejection from non-loopback clients.
- Cover that `/v0/management/...` still requires `admin-api-key`.
- Cover CLI user list/create/get/update/enable/disable/reset-key against a test
  HTTP server.
- Cover CLI usage snapshot table output and `--json`.
- Cover CLI usage timeseries request parameters and `--json`.
