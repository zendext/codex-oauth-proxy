# Configuration

[English](configuration.md) | [简体中文](zh-CN/configuration.md)

The server reads YAML configuration from `config.yaml` by default. Use
`--config <path>` to select another file:

```bash
codex-oauth-proxy --config /etc/codex-oauth-proxy/config.yaml
codex-oauth-proxy serve --config /etc/codex-oauth-proxy/config.yaml
```

A missing or empty configuration file is accepted and code defaults are
applied. Copying `config.example.yaml` is recommended because it selects an
explicit local bind address.

Configuration is loaded only at startup. Restart the process after changing the
file.

## Reference

| Field | Code default | Description |
| --- | --- | --- |
| `host` | empty | Bind host. An empty host binds all available interfaces. The example config uses `127.0.0.1`; containers normally use `0.0.0.0`. |
| `port` | `8317` | HTTP listen port. |
| `auth-dir` | `~/.codex` | Directory recursively scanned for Codex OAuth JSON files. |
| `debug` | `false` | Enables masked request and proxy diagnostics. |
| `admin-api-key` | empty | Enables `/v0/management/*` when non-empty. |
| `database.path` | empty | SQLite path. Empty resolves to `<auth-dir>/codex-oauth-proxy.db`. |
| `usage.enabled` | `true` | Records usage for requests authenticated with managed user keys. |
| `usage.debug-openai-response` | `false` | Adds safe upstream usage metadata to debug logs when `debug` is also enabled. |
| `allow-fast-mode` | `false` | Allows `service_tier: "fast"` and `"priority"` and exposes Fast metadata in model responses. |
| `proxy-url` | empty | Explicit outbound proxy URL. Use `direct` or `none` to disable environment proxy discovery. |
| `request-retry` | `3` | Reserved retry count for retry-aware calls. HTTP proxy requests do not use a general retry loop; OAuth refresh always uses its fixed three-attempt policy. |
| `codex-base-url` | `https://chatgpt.com/backend-api/codex` | Upstream base for Codex Responses-compatible routes. |
| `chatgpt-base-url` | `https://chatgpt.com/backend-api` | Upstream base for file, account, and hosted MCP compatibility routes. |
| `codex-user-agent` | empty | Upstream User-Agent override. Empty forwards the client value or uses a Codex CLI fallback. |
| `codex-beta-features` | empty | Fallback `x-codex-beta-features` header when the client did not provide one. |
| `codex-refresh-token-url` | empty | OAuth refresh endpoint override. Empty uses `https://auth.openai.com/oauth/token`. |

## Minimal Native Configuration

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
```

The local admin CLI works without `admin-api-key`. Add an admin key only for the
remote management API or Grafana:

```yaml
admin-api-key: "replace-with-a-long-random-secret"
```

## Container Configuration

The default Compose setup mounts `./auths` at `/root/.codex`:

```yaml
host: "0.0.0.0"
port: 8317
auth-dir: "/root/.codex"

database:
  path: ""
```

With an empty database path, the SQLite database is stored at:

```text
/root/.codex/codex-oauth-proxy.db
```

That file therefore persists in the same mounted `./auths` directory.

## OAuth File Loading

The loader recursively checks JSON files below `auth-dir`.

Supported records include:

- The official Codex CLI `auth.json` shape with a nested `tokens` object.
- Flat records whose `type` is `codex` and whose token fields are at the top
  level.

Files are ignored when they:

- Are not JSON files.
- Declare a non-Codex `type`.
- Do not look like either supported Codex format.
- Contain neither an access token nor a refresh token.

Credentials use `account_id` as their stable identity. When that field is
missing, the loader attempts to recover `chatgpt_account_id` and email metadata
from the ID token and then the access token. If no account claim is available,
the normalized relative file path is used as a compatibility identity. Such an
unidentified credential remains eligible for proxy traffic but is not an
account-targetable management resource. Runtime and usage IDs use
`account:<account_id>` for identified credentials and
`path:<normalized-relative-path>` for compatibility credentials.

Multiple files resolving to the same account form one logical credential and
therefore one round-robin slot. Logical credentials are sorted by stable
identity, so file renames and token replacement do not change account ordering.
Records marked `disabled: true` remain part of reconciliation but are excluded
from new request selection when no enabled source for that account remains.

The directory is rescanned during request-time auth loading. Additions,
removals, renames, external token updates, and disabled changes are observed
without restarting the process. If a previously valid file becomes malformed,
its last successfully parsed representation is retained for five seconds. A
file that remains malformed after that grace period is reported by
reconciliation and excluded.

A credential expiring within five minutes is refreshed before use. Refreshes
are coordinated in process by stable credential identity, so concurrent callers
reuse one completed refresh or a newer token that already replaced their old
token. Each refresh has a 30-second overall deadline and up to three attempts,
with retries limited to transient network failures and HTTP `408`, `429`,
`500`, `502`, `503`, and `504`. Valid `Retry-After` values are bounded by that
deadline.

Updated tokens are written through a same-directory `0600` temporary file,
synced, and atomically renamed over the selected source file. Account and email
claims are reparsed before persistence. An HTTP upstream `401` can trigger one
same-credential refresh and one retry before the client response is committed;
it does not trigger cross-credential failover.

## Managed Users and Database

The SQLite database stores:

- Users.
- Generated and rotated user API keys.
- Ten-minute usage buckets.

Generated API keys are stored as SHA-256 hashes. API responses expose key
metadata and a masked value; plaintext is returned only by user creation and key
reset operations.

If `database.path` is relative, it is resolved relative to the server process
working directory. `~` and `~/...` are expanded.

## Usage

Usage tracking is enabled unless explicitly disabled:

```yaml
usage:
  enabled: false
  debug-openai-response: false
```

Only requests authenticated with a managed user API key are attributed to a
user. Requests accepted through Codex OAuth access-token compatibility do not
have managed user identity and are not included.

See [Usage and Observability](usage-and-observability.md) for bucket and window
semantics.

## Fast Mode

Fast mode is disabled by default:

```yaml
allow-fast-mode: false
```

When disabled:

- Fast tier metadata is removed from model catalog responses.
- HTTP requests with `service_tier: "fast"` or `"priority"` return `400`.
- WebSocket `response.create` frames with those tiers are rejected.

Enable it explicitly:

```yaml
allow-fast-mode: true
```

Usage records normalize both wire values to the `fast` service tier.

## Outbound Proxy

Set an explicit proxy:

```yaml
proxy-url: "http://127.0.0.1:7890"
```

Leave it empty to use the Go HTTP transport's normal environment proxy
behavior. Use either value below to force direct connections:

```yaml
proxy-url: "direct"
```

```yaml
proxy-url: "none"
```

The setting applies to upstream HTTP, WebSocket, and OAuth refresh connections.

## Debug Logging

Enable request and route diagnostics:

```yaml
debug: true
```

Enable additional usage metadata:

```yaml
debug: true
usage:
  enabled: true
  debug-openai-response: true
```

Debug output uses masked key metadata and token fingerprints. It does not
intentionally log access tokens, refresh tokens, managed plaintext API keys, or
upstream response bodies.

## Upstream Overrides

The `codex-base-url`, `chatgpt-base-url`, and
`codex-refresh-token-url` settings are primarily useful for controlled testing
or alternate network routing. Values must include a URL scheme and host.

`codex-user-agent` and `codex-beta-features` override compatibility headers only
when their configured behavior applies. Normal client headers are otherwise
preserved where supported.
