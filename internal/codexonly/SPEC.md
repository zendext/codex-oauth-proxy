---
id: codex-proxy
type: module-design
title: Codex proxy core
status: draft
parent: architecture
depends-on:
  - configuration
---

# Codex proxy core

[English](SPEC.md) | [简体中文](SPEC.zh-CN.md)

`internal/codexonly` owns the Codex and Grok proxy paths. It is one module because
routing, credential selection, failover, affinity, and usage share one request
lifecycle; splitting those files would not create an independently reusable
boundary.

## Responsibility

- Authenticate managed callers and selected OAuth-compatibility routes.
- Load and refresh Codex credentials, then select a healthy credential.
- Proxy only whitelisted routes. Unmatched routes return `404`.
- Serve Grok on an independent listener without exposing Codex-only routes, and renew imported refreshable logins during requests.
- Supply the CLI subscription identity headers that a custom base URL client omits. Do not rewrite Grok Build request bodies.
- Convert Chat Completions and Zed Edit Prediction requests locally.
- Persist users, key hashes, affinity digests, authoritative auth health, and
  usage in one SQLite database.
- Turn the first unexpected runtime SQLite failure into process-level shutdown.

## Boundary

The module may use the standard library, YAML decoding, the pure-Go SQLite
driver, and Gorilla WebSocket. It must not gain a second storage backend,
provider translation layer, plugin host, or catch-all upstream forwarder.

Credential files remain the source of truth for OAuth enablement. SQLite must
not override that state. Managed plaintext keys leave the process only at
creation or reset.

Grok login can be a one-time handoff from the CLI to the proxy. Automatic and
manual refresh share the same per-identity coordination and generation guards;
file updates are serialized so concurrent account refreshes cannot discard
neighboring records. Keep OAuth deadlines separate from inference stream
lifetimes and complete authentication retries before committing a response.
The operator-facing lifecycle is owned by `configuration`.

Session affinity stores digests rather than raw session identifiers. The code
enforces that storage boundary; the reason for choosing digests is unconfirmed.
