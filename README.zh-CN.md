# codex-oauth-proxy

[English](README.md) | [简体中文](README.zh-CN.md)

> 本项目由 AI 编写，使用风险自负。

---

`codex-oauth-proxy` 是一个小型自托管 HTTP 代理，使用 Codex CLI 已保存的
OAuth 凭据转发受支持的 Codex 流量。它面向希望通过托管 API Key 暴露自己
Codex OAuth 访问能力、但不直接分发 OAuth 文件的运维者。

本代理仅面向 Codex。它支持 Codex CLI、供自定义 Agent 使用的有限
OpenAI 兼容 API、本地用户管理和按用户统计用量。它不是通用 OpenAI 网关，
也不是多提供商代理。

## 功能

- 加载 Codex CLI 官方 `~/.codex/auth.json` 格式和扁平 Codex Token JSON
  文件。
- 以轮询方式选择多个有效 OAuth 文件，并刷新过期的 Access Token。
- 代理 Codex CLI 使用的白名单 Responses、实时、图片、搜索、记忆、文件、
  账户和 Hosted MCP 路由。
- 基于 SQLite 签发托管的 `cop_...` API Key。
- 提供 OpenAI 兼容的 `/v1/chat/completions` 转换层。
- 使用 10 分钟桶记录按用户划分的 Token 用量，并保留 30 天。
- 提供本地管理 CLI 和可选 Grafana 仪表盘。

## 要求

- 从源码构建时需要 Go 1.26 或更高版本。
- 至少一个可用的 Codex OAuth 文件，通常由 Codex CLI 创建在
  `~/.codex/auth.json`。

Linux `amd64` 发布二进制可从
[GitHub Releases](https://github.com/zendext/codex-oauth-proxy/releases)
下载。

## 快速开始

构建代理：

```bash
go build -o codex-oauth-proxy ./cmd/server
cp config.example.yaml config.yaml
codex-oauth-proxy --config config.yaml
```

在另一个终端中创建托管用户：

```bash
codex-oauth-proxy admin users create alice
```

命令只会显示一次生成的 API Key。配置 Codex CLI 使用该 Key：

```toml
model_provider = "proxy"
chatgpt_base_url = "http://127.0.0.1:8317/backend-api/"

[model_providers.proxy]
name = "Codex OAuth Proxy"
base_url = "http://127.0.0.1:8317/v1"
env_key = "COP_API_KEY"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = false
```

```bash
export COP_API_KEY="cop_..."
codex
```

本地管理 CLI 调用仅允许回环地址访问的端点，不需要 `admin-api-key`。
只有远程管理 API 或 Grafana 需要访问时才设置 `admin-api-key`。

## Docker

容器中需要在 `config.yaml` 设置 `host: "0.0.0.0"`，将 Codex OAuth JSON
文件放到 `./auths`，然后启动服务：

```bash
docker compose up -d
```

在容器内执行本地管理：

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users create alice
```

原生运行和 Docker 的详细配置参见[快速入门](docs/zh-CN/getting-started.md)。

## 文档

- [快速入门](docs/zh-CN/getting-started.md)
- [配置](docs/zh-CN/configuration.md)
- [管理](docs/zh-CN/administration.md)
- [API 参考](docs/zh-CN/api-reference.md)
- [用量与可观测性](docs/zh-CN/usage-and-observability.md)
- [架构](docs/zh-CN/architecture.md)
- [开发](docs/zh-CN/development.md)

## 范围

代理只转发明确支持的路由。`/v1/*` 和项目自有的 `/v0/*` 端点是受支持的
集成接口。部分 `/backend-api/*` 路径仅用于兼容 Codex CLI，不是面向第三方
客户端的稳定公共 API。

服务提供 HTTP 监听。绑定地址、TLS 终止和网络暴露方式属于部署环境职责。

## 许可证

本项目使用 Apache License 2.0。参见 [LICENSE](LICENSE)。
