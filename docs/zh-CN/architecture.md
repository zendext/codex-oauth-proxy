# 架构

[English](../architecture.md) | [简体中文](architecture.md)

`codex-oauth-proxy` 是一个具有两种执行模式的 Go 二进制：

- `serve` 启动 HTTP 代理和项目 API。
- `admin` 调用运行中服务器仅允许回环地址访问的管理路由。

项目有意保持单一的 Codex 专用实现，不包含提供商转换、管理 UI、Plugin Host
或替代存储后端。

## 源码布局

| 路径 | 职责 |
| --- | --- |
| `cmd/server/` | 进程入口、服务器生命周期、管理 CLI、HTTP Client 和 CLI 格式化。 |
| `internal/codexonly/config.go` | YAML 加载和路径、默认值解析。 |
| `internal/codexonly/auth.go` | OAuth 文件发现、解析、过滤和持久化。 |
| `internal/codexonly/refresh.go` | OAuth 刷新和出站 HTTP Transport 构建。 |
| `internal/codexonly/server.go` | 路由、认证、模型目录、Header、HTTP Reverse Proxy 以及管理和用户 Handler。 |
| `internal/codexonly/chat_completions.go` | Chat Completions 到 Responses 的转换和响应翻译。 |
| `internal/codexonly/user_store.go` | SQLite Schema、用户、API Key 和认证。 |
| `internal/codexonly/usage.go` | 用量存储、聚合、窗口、维度和时间序列。 |
| `internal/codexonly/usage_proxy.go` | HTTP、SSE 和 WebSocket 用量采集。 |
| `observability/` | 可选 Grafana Compose 和 Provisioning 资源。 |

## 服务器启动

启动过程执行以下步骤：

1. 解析 CLI Flag 并选择服务器模式。
2. 读取 YAML 配置并应用代码默认值。
3. 解析 `auth-dir` 并验证两个上游 Base URL。
4. 构建上游 Client 和带超时的 OAuth 刷新 Client。
5. 扫描认证目录以验证可读性。
6. 解析 SQLite 路径并执行幂等 Schema Migration。
7. 启动一个 `net/http` 服务器。

服务器将 `ReadHeaderTimeout` 设置为 10 秒。它不会设置可能中断已建立 Stream
或 WebSocket 流量的 Read/Write Timeout。优雅关闭超时为 10 秒。

## OAuth 凭据流

`FileAuthStore` 在选择凭据时递归扫描配置目录，并将结果与上一次成功扫描进行
协调。

每个上游请求都会：

1. 解析 Codex 认证文件，并保留禁用记录用于协调。
2. 从 `account_id`、Token 声明或规范化路径回退值解析稳定身份。
3. 将同一账户的重复文件合并为一个逻辑凭据。
4. 按稳定身份排序可选择的逻辑凭据。
5. 以轮询方式选择下一个逻辑凭据。
6. 将五分钟内过期的凭据视为已过期。
7. 刷新过期凭据，并重新解析账户和邮箱声明。
8. 将刷新后的值写回选中的源文件。
9. 使用选中的 Access Token 和 Account ID 转发请求。

协调结果会报告新增、删除、凭据变化、元数据变化、可用性变化和源文件变化，
且不包含 Token 内容。稳定账户 ID 在文件重命名和同账户 Token 替换后保持不变。
在同一路径换入另一账户时，会产生旧身份删除和新身份新增。禁用的逻辑凭据仍然
保留在状态中，但不会用于新请求选择。五秒解析错误宽限期会在编辑器部分写入
期间保留最近一次有效表示；持续格式错误的文件随后会被排除。

## 认证边界

传入的托管 API Key 与传出的 OAuth Access Token 是两种独立凭据。

### 托管用户认证

对于公共代理和用户路由：

1. 读取 `Authorization` 和 `X-API-Key`。
2. 使用 SHA-256 Hash 每个候选值。
3. 查找已存储的 Key Hash。
4. 验证 API Key 和用户均已启用。
5. 将用户和 Key 身份附加到请求。

传入的托管 Key 永远不会转发到上游。

### 管理认证

`/v0/management/*` 使用常量时间比较候选 Token 和配置的 `admin-api-key`。
未配置 Key 时，整个路由组使用 `404` 隐藏。

### 本地管理

`/v0/local-admin/*` 检查 TCP 远端地址，只接受回环 IP，然后在没有管理 Key
的情况下复用管理 Handler。

### OAuth 兼容认证

部分 ChatGPT 后端兼容路由可以接受当前已加载的 Codex OAuth Access Token。
这是为了支持已经持有该 Token 的 Codex CLI 行为。这类请求没有托管用户身份，
不会计入按用户统计。

## 路由选择

HTTP Handler 会在 Reverse Proxy 白名单之前检查项目自有路由。

| 路由组 | 所有者 |
| --- | --- |
| `/`、`/healthz` | 服务 Handler |
| `/v0/local-admin/*` | 回环管理 |
| `/v0/management/*` | 管理 Key API |
| `/v0/user/*` | 托管用户自助 API |
| `/v1/models` | 嵌入式模型目录 |
| `/v1/chat/completions` | 本地协议转换 |
| 白名单 `/v1/*` | Codex 上游 Reverse Proxy |
| 部分 `/backend-api/*` | Codex CLI 兼容 Reverse Proxy |

所有未匹配路由都返回 `404`，不存在通用的 Catch-all 上游转发。

## Reverse Proxy 流程

对于白名单代理请求：

1. 认证传入的托管 Key 或允许的 OAuth 兼容 Token。
2. Fast 模式禁用时拒绝 Fast Service Tier。
3. 选择并刷新一个上游 OAuth 凭据。
4. 将目标 URL 重写到配置的 Codex 或 ChatGPT Base。
5. 使用选中的 OAuth Access Token 替换 `Authorization`。
6. 可用时添加 ChatGPT Account ID 和兼容 Header。
7. 转发 HTTP Stream 响应或桥接 WebSocket Frame。
8. 为托管用户请求采集用量元数据。

代理会保持已建立的 HTTP Stream。WebSocket 转发使用 Gorilla WebSocket，并在
上游 Upgrade 路径强制使用 HTTP/1.1 ALPN。

## Chat Completions 转换

`/v1/chat/completions` 在本地实现，不直接透传：

1. 解码 Chat Completions JSON 对象。
2. 将消息、Tool、Response Format、Reasoning 和 Service Tier 转换为 Responses
   请求。
3. 强制上游使用 `stream: true` 和 `store: false`。
4. 读取 Responses SSE Event。
5. 聚合为普通 Chat Completions 响应，或转换为 Chat Completions SSE Chunk。
6. 应用本地 Stop Sequence 过滤并记录用量。

这是专用兼容层，不是通用的 Schema 保留型转换引擎。

## 托管用户数据模型

SQLite 通过 `database/sql` 和纯 Go `modernc.org/sqlite` Driver 打开。连接池
限制为一个 Open Connection。

### 用户

用户包含：

- 随机 `usr_...` ID。
- 不区分大小写且唯一的名称。
- 启用状态。
- 创建和更新时间。

### API Key

API Key 包含：

- 随机 `key_...` ID。
- 所属用户 ID。
- SHA-256 Key Hash。
- 显示前缀和脱敏值。
- 启用状态。
- 创建、轮换和最后使用时间。

Partial Unique Index 保证每个用户只能有一个启用的 Key。重置 Key 会在同一事务
中禁用旧有效 Key 并插入替代 Key。

## 用量数据模型

`usage_buckets` 将托管用户用量聚合到 UTC 10 分钟桶。逻辑桶 Key 包含：

- 桶开始时间。
- 用户 ID。
- API Key ID。
- 模型。
- Reasoning Effort。
- Service Tier。
- 稳定 OAuth 凭据 ID。

计数器通过 SQLite Upsert 累加。新写入会清理超过 30 天保留窗口的桶。

快照和时间序列都从同一张表读取。Grafana 使用时间序列管理 API，而不直接读取
SQLite。

## 信任与 Secret 边界

服务处理三类不同 Secret：

- 磁盘上的 Codex OAuth Access Token 和 Refresh Token。
- 可选的远程 `admin-api-key`。
- 生成的托管用户 API Key。

OAuth 文件会被原地读取和刷新。托管明文 Key 只在创建或重置时返回；持久化时
只保存 Hash 和脱敏元数据。

服务器自身提供 HTTP。监听地址选择和传输终止属于部署环境职责。
