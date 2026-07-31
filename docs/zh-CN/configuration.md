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
| `request-retry` | `3` | 初始凭据轮次之后允许的额外冷却重试轮数。`0` 禁用冷却轮次。 |
| `max-retry-credentials` | `0` | 每轮最多尝试的不同且当前可用的 Codex 凭据数。`0` 表示尝试全部可用凭据。 |
| `max-retry-interval` | `30` | 为开始下一轮而等待最近凭据冷却的最长秒数。`0` 禁用冷却等待。 |
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
- 声明了非 Codex `type`。
- 不符合任一支持的 Codex 格式。
- Access Token 和 Refresh Token 均不存在。

凭据使用 `account_id` 作为稳定身份。缺少该字段时，加载器会先从 ID Token、
再从 Access Token 中尝试恢复 `chatgpt_account_id` 和邮箱元数据。若没有可用的
账户声明，则使用规范化的相对文件路径作为兼容身份。此类未识别凭据仍可用于
代理流量，但不能作为按账户定位的管理资源。运行时和用量 ID 对已识别凭据使用
`account:<account_id>`，对兼容凭据使用
`path:<normalized-relative-path>`。

解析到同一账户的多个文件会组成一个逻辑凭据，因此只占一个轮询槽位。逻辑
凭据按稳定身份排序，所以文件重命名和 Token 替换不会改变账户顺序。设置了
`disabled: true` 的记录仍参与协调；仅当该账户不存在任何启用的来源文件时，
才会从新请求选择中排除。

请求期间加载身份时会重新扫描目录，因此无需重启进程即可发现文件新增、删除、
重命名、外部 Token 更新和禁用状态变化。如果此前有效的文件变为格式错误，
加载器会保留最近一次成功解析的表示五秒。超过宽限期后仍然格式错误的文件会
由协调结果报告并排除。

凭据在五分钟内过期时会先刷新再使用。刷新会按稳定凭据身份在进程内协调，因此
并发调用方会复用一次已完成刷新，或在相同稳定身份仍然存在时复用已经替换其旧
Token 的较新 Token。源路径被另一账户替换时，旧请求不会附着到替换后的凭据。
每次刷新共用一个 30 秒 Deadline，最多尝试三次；仅临时网络故障和 HTTP
`408`、`429`、`500`、`502`、`503`、`504` 可以重试。有效的
`Retry-After` 受该 Deadline 限制。

更新后的 Token 会先写入同目录 `0600` 临时文件，完成 Sync 后原子重命名覆盖
选中的源文件。持久化前会从刷新后的 Token 重新解析账户和邮箱声明。HTTP 上游
返回 `401`，且原始请求已经可重放时，可以在客户端响应提交前触发一次同凭据
刷新和一次重试。刷新后继续返回未授权、`invalid_grant` 或显式配额恢复 Deadline
时，该逻辑凭据会在状态清除或过期前不可用。

## 健康感知故障转移

健康状态以稳定 Codex 凭据为全局范围，跨所有用户和会话生效。选择会跳过禁用
凭据、活动冷却、凭据故障和模型专用能力排除。会话绑定请求的凭据不再可用时，
会使用现有 Compare-and-swap 亲和性重绑定。

故障处理包含三个相互独立的层次：

1. 可重放请求收到 `401` 时，刷新并重试同一凭据一次；这不会消耗凭据切换上限。
2. 一个轮次尝试不同且当前可用的凭据，并受 `max-retry-credentials` 限制。
3. 一个轮次耗尽可用凭据后，仅当最近冷却不超过 `max-retry-interval` 时等待，
   随后最多启动 `request-retry` 个额外轮次。

请求范围的 `4xx` 响应会立即停止，且不会惩罚凭据。`429` 遵循
`Retry-After` 或显式 Codex 配额重置时间。网络故障、`408` 和可重试 `5xx`
使用短期内存冷却。模型不支持响应只排除对应凭据和模型组合。

SQLite 只持久化显式配额恢复 Deadline、`invalid_grant` 和刷新后继续未授权的
状态。短期传输冷却和模型排除只存在于当前进程。凭据材料变化或后续刷新、请求
证明凭据健康时，会清除凭据相关的持久化状态；未过期的显式配额 Deadline 在
同账户 Token 更新和进程重启后仍然保留。

跨凭据重试只适用于只读 `GET`/`HEAD`、JSON `/v1/chat/completions`、
Responses、Responses Compact、Alpha Search、JSON Image Generation、
Trace Summarization，以及 Upgrade 成功前的 Responses WebSocket Handshake。
可重放 Body 只在内存中缓冲，最大包含 32 MiB。未知长度、更大、Multipart、
文件、Realtime、具有副作用的 Wham、Hosted MCP 和未知写请求只转发一次，不会
仅因无法重放而被拒绝。符合条件的缓冲请求在模糊网络故障后可能重复执行；这是
为了可用性而接受的极少量重复生成或重复计费风险。

## 托管用户与数据库

SQLite 数据库存储：

- 用户。
- 生成和轮换的用户 API Key。
- 租户范围的会话亲和性 Digest 和稳定 OAuth 认证目标。
- 权威 Codex 凭据健康状态。
- 10 分钟用量桶。

生成的 API Key 以 SHA-256 Hash 存储。API 响应只公开 Key 元数据和脱敏值；
明文只会由创建用户和重置 Key 操作返回。

会话亲和性没有配置开关，默认启用，并且只存储 SHA-256 Digest、稳定认证 ID
和时间戳，不持久化原始会话标识符。

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
