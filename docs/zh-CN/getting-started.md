# 快速入门

[English](../getting-started.md) | [简体中文](getting-started.md)

本指南介绍如何启动原生或容器化代理、创建托管 API Key，并连接 Codex CLI
或 OpenAI 兼容客户端。

## 前置条件

- 已完成 Codex CLI OAuth 登录。
- 从源码构建时需要 Go 1.26 或更高版本。
- 只有使用容器部署时才需要 Docker 和 Docker Compose。

代理不实现 OAuth 登录流程，而是读取 Codex CLI 已保存在磁盘上的凭据。

## OAuth 文件

默认认证目录为 `~/.codex`。Codex CLI 官方文件是：

```text
~/.codex/auth.json
```

代理也接受配置的 `auth-dir` 下的扁平 Codex Token JSON 文件。它会递归扫描
JSON 文件，忽略非 Codex 凭据以及设置了 `disabled: true` 的记录。

至少一个已加载凭据必须包含 Access Token 或 Refresh Token。过期凭据会在使用
前自动刷新，并写回同一个文件。

## 原生安装

### 发布二进制

GitHub Releases 会发布 Linux `amd64` 二进制和 SHA-256 文件：

```text
codex-oauth-proxy-linux-amd64
codex-oauth-proxy-linux-amd64.sha256
```

校验 Checksum 后，为二进制添加可执行权限，并将其放入 `PATH` 中的目录。

### 从源码构建

```bash
git clone https://github.com/zendext/codex-oauth-proxy.git
cd codex-oauth-proxy
go build -o codex-oauth-proxy ./cmd/server
```

## 原生启动

在源码 Checkout 中复制配置模板：

```bash
cp config.example.yaml config.yaml
```

只使用发布二进制时，创建包含以下最小等价内容的 `config.yaml`：

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
```

两种形式都绑定到 `127.0.0.1:8317` 并读取 `~/.codex`。启动服务：

```bash
codex-oauth-proxy --config config.yaml
```

显式使用 `serve` 的形式与之等价：

```bash
codex-oauth-proxy serve --config config.yaml
```

运行 `codex-oauth-proxy --help` 可查看生成的命令树，也可以在具体命令后追加
`--help` 查看该命令的参数和 Flag。

确认进程可以访问：

```bash
curl http://127.0.0.1:8317/healthz
```

预期响应：

```json
{"status":"ok"}
```

## 进程监管

SQLite 是启动和持续运行所必需的依赖。当数据库无法打开、Migration、初始读取，
或运行期间无法执行读写时，进程会带错误退出。

生产部署必须通过 systemd、Docker、Kubernetes 或其他外部 Supervisor 运行代理，
并配置合适的重启策略。应先修复数据库路径、权限、损坏、磁盘或文件系统问题，
再预期重启后的进程保持健康。代理不会重新连接 SQLite，也不会以内存降级模式
继续运行。

## 创建托管用户

保持服务器运行，并使用另一个终端：

```bash
codex-oauth-proxy admin users create alice
```

命令默认连接 `http://127.0.0.1:8317`。它使用仅允许回环地址访问的本地管理
路由，不读取 `config.yaml`，也不需要 `admin-api-key`。

明文 `cop_...` API Key 只会在创建用户或重置 Key 时显示。关闭终端输出前需要
妥善保存。

## 连接 Codex CLI

在 Codex CLI 配置中添加 Responses 提供商：

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

在启动 Codex 的环境中设置托管 Key：

```bash
export COP_API_KEY="cop_..."
codex
```

`requires_openai_auth = false` 会让 Codex CLI 将 `COP_API_KEY` 发送给本代理。
代理在向上游转发请求前，会将其替换为选中的 Codex OAuth Access Token。

`chatgpt_base_url` 启用 Codex CLI 用于文件、账户状态和 Hosted MCP 行为的一小组
ChatGPT 后端兼容路由。这些路由不是面向通用客户端的公共 API。

## 连接 OpenAI 兼容客户端

使用代理 Base URL 和托管 API Key：

```text
Base URL: http://127.0.0.1:8317/v1
API key:  cop_...
```

列出模型：

```bash
curl http://127.0.0.1:8317/v1/models \
  -H 'Authorization: Bearer cop_...'
```

发送非流式 Chat Completions 请求：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer cop_...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }'
```

代理会将 Chat Completions 请求转换为上游 Responses 请求。当客户端已经支持
Responses Wire Format 时，应直接使用 `/v1/responses`。

## 连接 Zed Edit Prediction

为 Zed 的 `open_ai_compatible_api` Edit Prediction Provider 配置专用端点：

```json
{
  "edit_predictions": {
    "provider": "open_ai_compatible_api",
    "mode": "eager",
    "allow_data_collection": "no",
    "open_ai_compatible_api": {
      "api_url": "http://127.0.0.1:8317/v1/zed/edit-predictions",
      "model": "gpt-5.6-luna",
      "prompt_format": "qwen",
      "max_output_tokens": 256
    }
  }
}
```

请使用 Zed 0.227 或更高版本。Key 独立于 `settings.json` 配置，可选择以下
任一方式：

- 打开 Zed 的 Edit Prediction Provider 设置，选择 **OpenAI Compatible API**，
  在 **API Key** 中录入托管 `cop_...`。
- 在同一环境中启动 Zed 前设置环境变量：

  ```bash
  export ZED_OPEN_AI_COMPATIBLE_EDIT_PREDICTION_API_KEY="cop_..."
  zed
  ```

Zed 会将该值作为 `Authorization: Bearer cop_...` 发送。该兼容端点仅用于 Edit
Prediction；项目不实现 `/v1/completions`。

## Docker Compose

根据模板创建 `config.yaml`，并修改绑定地址：

```yaml
host: "0.0.0.0"
port: 8317
auth-dir: "/root/.codex"
```

将 Codex OAuth 文件放到 `./auths`。默认 Compose 项目挂载：

```text
./config.yaml -> /codex-oauth-proxy/config.yaml
./auths       -> /root/.codex
```

启动代理：

```bash
docker compose up -d
```

由于本地管理端点只接受回环客户端，需要在代理容器内执行管理 CLI：

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users create alice
```

也可以直接启动镜像：

```bash
docker run --rm -p 8317:8317 \
  -v "$PWD/config.yaml:/codex-oauth-proxy/config.yaml:ro" \
  -v "$PWD/auths:/root/.codex" \
  zendext/codex-oauth-proxy:latest
```

## 后续步骤

- 在[配置](configuration.md)中检查所有设置。
- 使用[管理](administration.md)维护用户和 Key。
- 在 [API 参考](api-reference.md)中查看受支持的路由。
- 在[用量与可观测性](usage-and-observability.md)中了解统计方式。
