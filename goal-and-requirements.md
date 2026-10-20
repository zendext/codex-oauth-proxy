---
id: goal-and-requirements
type: goal-and-requirements
title: Codex OAuth proxy
status: draft
---

# Codex OAuth proxy

[English](goal-and-requirements.md) | [简体中文](goal-and-requirements.zh-CN.md)

## Goal

Let a local operator share existing Codex CLI OAuth credentials with Codex CLI
and supported clients through managed API keys, without exposing those OAuth
credentials to the clients.

## Problem

Codex CLI stores usable OAuth credentials on disk, but clients that should use
those credentials still need a controlled local entry point. Directly sharing
the OAuth files would expose refreshable credentials and provide no per-user
key, failover, or usage boundary.

## Audience

The product is for an operator who already has Codex CLI credentials and wants
to run one local proxy for their own Codex-compatible clients. It is not a
hosted multi-tenant service. (Unconfirmed: whether the intended operator is
always a single person or may administer a small shared local deployment.)

## Capabilities

- Proxy whitelisted Codex and selected ChatGPT compatibility requests.
- Authenticate callers with managed `cop_...` API keys.
- Refresh, select, and fail over Codex OAuth credentials.
- Keep a session on one healthy credential without storing the raw session
  identifier.
- Convert Chat Completions and Zed Edit Prediction requests into the Codex
  Responses flow used by the proxy.
- Record managed-user usage and expose it through administration APIs and an
  optional Grafana bundle.
- Provide loopback administration by default and remote administration only
  when explicitly enabled.

## Non-goals

- Generic provider translation, a management UI, SDK embedding, a plugin host,
  and alternate storage backends are excluded. These are product boundaries,
  not unfinished work.
- The proxy does not implement a Codex OAuth login flow.
- The process does not terminate TLS. The deployment environment owns listen
  address selection and transport termination.
- Published release artifacts are Linux amd64 only.

## Principles

- Keep the implementation Codex-specific and small enough to run as one Go
  process.
- Treat managed API keys, the optional admin key, and Codex OAuth tokens as
  separate credentials.
- Do not continue proxying after the required SQLite state becomes unreliable.
- Do not cut off established streams or WebSocket connections with server read
  or write timeouts.
