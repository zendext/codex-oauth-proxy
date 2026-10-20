# 用量可观测性

[English](SPEC.md) | [简体中文](SPEC.zh-CN.md)

`observability` 负责可选的 Grafana 部署资源。它不负责用量采集或保留；那些属于 `codex-proxy`。

## 职责

- 为代理的用量 API 配置 Grafana 数据源和仪表盘。
- 通过远程管理 API 读取用量，而不是直接打开 SQLite。

## 边界

本模块可以编排和配置 Grafana。它不得增加第二个用量数据库，不得直接抓取代理进程，也不得绕过 `administration`。

因此，远程仪表盘访问需要显式配置的管理凭证。Grafana 套件对代理运行仍然是可选的。
