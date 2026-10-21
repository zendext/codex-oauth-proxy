---
id: server-process
type: module-design
title: Server process
status: draft
parent: architecture
depends-on:
  - configuration
---

# Server process

[English](SPEC.md) | [简体中文](SPEC.zh-CN.md)

`cmd/server` owns process startup, shutdown, and the admin CLI. Proxy behavior
belongs to `codex-proxy`.

## Responsibility

- Parse `serve` and `admin` commands. Omitting a command runs the server.
- Load configuration, construct the proxy handler, and listen on the Codex and
  Grok addresses with a 10-second header timeout and no body read/write timeout.
- Stop on interrupt, `SIGTERM`, handler context cancellation, or a fatal
  storage error. Allow 10 seconds for graceful HTTP shutdown, then force-close
  remaining connections.
- Call the running server's loopback administration API. The CLI does not read
  server configuration or send `admin-api-key`.

## Boundary

Allowed dependencies are the standard library, the CLI parser, and
`codex-proxy`. This module must not reimplement routing, credential selection,
SQLite access, or upstream protocol conversion.

`--local-model` is accepted and ignored. Runtime model synchronization remains
enabled.
