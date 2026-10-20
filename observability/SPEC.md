---
id: observability
type: module-design
title: Usage observability
status: draft
parent: architecture
depends-on:
  - api-reference
  - administration
---

# Usage observability

[English](SPEC.md) | [简体中文](SPEC.zh-CN.md)

`observability` owns the optional Grafana deployment assets. It does not own
usage collection or retention; those belong to `codex-proxy`.

## Responsibility

- Provision a Grafana datasource and dashboard for the proxy's usage API.
- Read usage through the remote management API rather than opening SQLite.

## Boundary

This module may compose and provision Grafana. It must not add a second usage
database, scrape the proxy process directly, or bypass `administration`.

Remote dashboard access therefore requires the explicitly configured
management credential. The Grafana bundle remains optional for proxy operation.
