# 配置

[English](../configuration.md) | [简体中文](configuration.md)

服务器默认从 `config.yaml` 读取 YAML 配置。使用 `--config <path>` 指定其他
文件：

```bash
codex-oauth-proxy --config /etc/codex-oauth-proxy/config.yaml
codex-oauth-proxy serve --config /etc/codex-oauth-proxy/config.yaml
```

配置文件不存在或为空时也可以启动，此时应用代码默认值。建议复制
`config.example.yaml`，因为该模板会选择明确的本地绑定地址。

配置只在启动时加载。修改文件后需要重启进程。

## 配置参考

| 字段 | 代码默认值 | 说明 |
| --- | --- | --- |
| `host` | 空 | 绑定主机。空值会绑定所有可用接口。示例配置使用 `127.0.0.1`；容器通常使用 `0.0.0.0`。 |
| `port` | `8317` | HTTP 监听端口。 |
| `auth-dir` | `~/.codex` | 递归扫描 Codex OAuth JSON 文件的目录。 |
| `debug` | `false` | 启用经过脱敏的请求和代理诊断。 |
| `admin-api-key` | 空 | 非空时启用 `/v0/management/*`。 |
| `database.path` | 空 | SQLite 路径。空值解析为 `<auth-dir>/codex-oauth-proxy.db`。 |
| `usage.enabled` | `true` | 为使用托管用户 Key 认证的请求记录用量。 |
| `usage.debug-openai-response` | `false` | 当 `debug` 也启用时，在调试日志中添加安全的上游用量元数据。 |
| `allow-fast-mode` | `false` | 允许 `service_tier: "fast"` 和 `"priority"`，并在模型响应中公开 Fast 元数据。 |
| `proxy-url` | 空 | 显式出站代理 URL。使用 `direct` 或 `none` 禁用环境代理发现。 |
| `request-retry` | `3` | 为支持重试的调用保留的重试次数。当前代理路径没有应用通用请求重试循环。 |
| `codex-base-url` | `https://chatgpt.com/backend-api/codex` | Codex Responses 兼容路由的上游 Base URL。 |
| `chatgpt-base-url` | `https://chatgpt.com/backend-api` | 文件、账户和 Hosted MCP 兼容路由的上游 Base URL。 |
| `codex-user-agent` | 空 | 上游 User-Agent 覆盖值。空值会转发客户端值或使用 Codex CLI 回退值。 |
| `codex-beta-features` | 空 | 客户端未提供时使用的 `x-codex-beta-features` 回退 Header。 |
| `codex-refresh-token-url` | 空 | OAuth 刷新端点覆盖值。空值使用 `https://auth.openai.com/oauth/token`。 |

## 最小原生配置

```yaml
host: "127.0.0.1"
port: 8317
auth-dir: "~/.codex"
```

本地管理 CLI 不需要 `admin-api-key`。只有远程管理 API 或 Grafana 需要时才添加
管理 Key：

```yaml
admin-api-key: "replace-with-a-long-random-secret"
```

## 容器配置

默认 Compose 配置将 `./auths` 挂载到 `/root/.codex`：

```yaml
host: "0.0.0.0"
port: 8317
auth-dir: "/root/.codex"

database:
  path: ""
```

数据库路径为空时，SQLite 数据库保存在：

```text
/root/.codex/codex-oauth-proxy.db
```

因此该文件会持久化到同一个 `./auths` 挂载目录中。

## OAuth 文件加载

加载器会递归检查 `auth-dir` 下的 JSON 文件。

支持的记录包括：

- 使用嵌套 `tokens` 对象的 Codex CLI 官方 `auth.json` 格式。
- `type` 为 `codex` 且 Token 字段位于顶层的扁平记录。

以下文件会被忽略：

- 不是 JSON 文件。
- 无法解析。
- 声明了非 Codex `type`。
- 不符合任一支持的 Codex 格式。
- 设置了 `disabled: true`。
- Access Token 和 Refresh Token 均不存在。

存在多个有效凭据时，请求会按照排序后的文件顺序轮询选择。将在五分钟内过期的
凭据会在使用前刷新，更新后的 Token 会写回源文件。

## 托管用户与数据库

SQLite 数据库存储：

- 用户。
- 生成和轮换的用户 API Key。
- 10 分钟用量桶。

生成的 API Key 以 SHA-256 Hash 存储。API 响应只公开 Key 元数据和脱敏值；
明文只会由创建用户和重置 Key 操作返回。

如果 `database.path` 是相对路径，则相对于服务器进程工作目录解析。`~` 和
`~/...` 会展开。

## 用量

除非显式禁用，否则默认启用用量统计：

```yaml
usage:
  enabled: false
  debug-openai-response: false
```

只有使用托管用户 API Key 认证的请求才会归属到用户。通过 Codex OAuth Access
Token 兼容机制接受的请求没有托管用户身份，不计入统计。

桶和窗口语义参见[用量与可观测性](usage-and-observability.md)。

## Fast 模式

默认禁用 Fast 模式：

```yaml
allow-fast-mode: false
```

禁用时：

- 从模型目录响应中移除 Fast Tier 元数据。
- 包含 `service_tier: "fast"` 或 `"priority"` 的 HTTP 请求返回 `400`。
- 包含上述 Tier 的 WebSocket `response.create` Frame 会被拒绝。

显式启用：

```yaml
allow-fast-mode: true
```

用量记录会将两个 Wire Value 都规范化为 `fast` Service Tier。

## 出站代理

设置显式代理：

```yaml
proxy-url: "http://127.0.0.1:7890"
```

留空时使用 Go HTTP Transport 的常规环境代理行为。使用以下任一值强制直连：

```yaml
proxy-url: "direct"
```

```yaml
proxy-url: "none"
```

该设置同时应用于上游 HTTP、WebSocket 和 OAuth 刷新连接。

## 调试日志

启用请求和路由诊断：

```yaml
debug: true
```

启用额外用量元数据：

```yaml
debug: true
usage:
  enabled: true
  debug-openai-response: true
```

调试输出使用脱敏 Key 元数据和 Token Fingerprint，不会有意记录 Access Token、
Refresh Token、托管明文 API Key 或上游响应 Body。

## 上游覆盖

`codex-base-url`、`chatgpt-base-url` 和 `codex-refresh-token-url` 主要用于
受控测试或替代网络路由。值必须包含 URL Scheme 和 Host。

`codex-user-agent` 和 `codex-beta-features` 只在相应配置行为适用时覆盖兼容
Header；其他情况下会尽可能保留正常客户端 Header。
