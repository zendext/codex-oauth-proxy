# Administration

[English](administration.md) | [简体中文](zh-CN/administration.md)

The proxy provides two administration surfaces:

- A loopback-only local route used by the bundled admin CLI.
- An optional remote management API protected by `admin-api-key`.

Both surfaces call the same user and usage handlers and operate on the same
SQLite database.

## Local Admin CLI

The CLI connects to `http://127.0.0.1:8317` by default:

```bash
codex-oauth-proxy admin users list
```

It calls `/v0/local-admin/*`. The server accepts this route only when the
request's remote address is loopback (`127.0.0.1` or `::1`). It does not require
or send `admin-api-key`.

Use `--url` for another port or path on the same local service:

```bash
codex-oauth-proxy admin users list \
  --url http://127.0.0.1:8318
```

Use `--json` with any admin command for machine-readable output:

```bash
codex-oauth-proxy admin users list --json
```

Global admin flags may appear after the leaf command.

## User Commands

### List Users

```bash
codex-oauth-proxy admin users list
codex-oauth-proxy admin users list --enabled true
codex-oauth-proxy admin users list --enabled false
```

### Create a User

```bash
codex-oauth-proxy admin users create alice
```

User names are required and unique without regard to case. A new user is enabled
by default and receives one active generated API key.

The create output includes the plaintext key once.

### Get a User

```bash
codex-oauth-proxy admin users get usr_xxx
```

### Rename a User

```bash
codex-oauth-proxy admin users update usr_xxx --name alice2
```

### Enable or Disable a User

```bash
codex-oauth-proxy admin users enable usr_xxx
codex-oauth-proxy admin users disable usr_xxx
```

A disabled user cannot authenticate proxy or user API requests. The stored API
key remains present and works again if the user is re-enabled.

### Reset an API Key

```bash
codex-oauth-proxy admin users reset-key usr_xxx
```

Resetting a key:

- Disables every previously active key for the user.
- Creates one new active key.
- Returns the new plaintext key once.
- Does not delete historical usage attributed to older key IDs.

## Usage Commands

### Rolling Snapshot

```bash
codex-oauth-proxy admin usage snapshot
codex-oauth-proxy admin usage snapshot --user-id usr_xxx
codex-oauth-proxy admin usage snapshot --api-key-id key_xxx
```

Human-readable output shows request and token totals for the rolling 5-hour and
7-day windows.

### Timeseries

```bash
codex-oauth-proxy admin usage timeseries \
  --window 7d \
  --step 1h \
  --group-by user
```

Available flags:

| Flag | Values |
| --- | --- |
| `--window` | `5h`, `24h`, `7d`, `30d`, `today` |
| `--step` | `auto`, `10m`, `30m`, `1h`, `6h`, `1d` |
| `--group-by` | Comma-separated `user`, `api_key`, `model`, `reasoning_effort`, `service_tier` |
| `--fill` | `none` or `zero` |
| `--user-id` | One user ID |
| `--api-key-id` | One API key ID |

The default window is `7d`; the default grouping is `user`. Automatic step size
depends on the selected window.

## Docker Administration

The local route sees the host-side CLI connection as non-loopback when the
server runs in a container. Run the CLI inside the proxy container:

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users list
```

For scripts or Grafana outside the container, enable and use the remote
management API instead.

## Remote Management API

Set a non-empty key:

```yaml
admin-api-key: "replace-with-a-long-random-secret"
```

Then authenticate `/v0/management/*` requests with either:

```http
Authorization: Bearer <admin-api-key>
```

or:

```http
X-API-Key: <admin-api-key>
```

When `admin-api-key` is empty, remote management routes return `404`. Local
admin routes remain available to loopback clients.

Example:

```bash
curl http://127.0.0.1:8317/v0/management/users \
  -H 'Authorization: Bearer admin-change-me'
```

See [API Reference](api-reference.md) for request and response shapes.

## User Self-Service

A managed user API key can access:

- Current user and key metadata.
- API key reset.
- Today's UTC usage.

Resetting through the user API invalidates the credential used for the request
and returns a new plaintext key. The caller must switch to the new key.

## Key Handling

- Managed keys use the `cop_` prefix.
- Only the SHA-256 hash is persisted for authentication.
- List and detail responses include `key_prefix` and `masked_key`, not plaintext.
- Creation and reset are the only operations that return plaintext.
- One user can have only one enabled API key at a time.
- A key reset disables the old key immediately.
