# codex-oauth-proxy

[English](README.md) | [简体中文](README.zh-CN.md)

> 本项目由 AI 编写，使用风险自负。

---

`codex-oauth-proxy` 使用 Codex CLI 的 OAuth 凭据，通过托管 API Key 转发
Codex 请求。

## 功能

- 代理 Codex CLI 和受支持的 OpenAI 兼容请求。
- 为 Qwen FIM Prompt 提供专用 Zed Edit Prediction 端点。
- 为代理用户管理 `cop_...` API Key。
- 使用健康感知的 Codex OAuth 故障转移、冷却和协调 Token 刷新。
- 持久化租户范围的会话亲和性，不存储原始会话标识符。
- 记录按用户划分的 Token 用量，并提供可选 Grafana 仪表盘。

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

## 许可证

本项目使用 Apache License 2.0。参见 [LICENSE](LICENSE)。
