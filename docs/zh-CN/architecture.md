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
| `internal/codexonly/auth_health.go` | 全局凭据健康、模型排除、冷却协调和权威状态持久化。 |
| `internal/codexonly/refresh.go` | OAuth 刷新和出站 HTTP Transport 构建。 |
| `internal/codexonly/failover.go` | 重放资格、上游响应分类、重试层次和确定性聚合错误。 |
| `internal/codexonly/server.go` | 路由、认证、模型目录、Header、HTTP Reverse Proxy 以及管理和用户 Handler。 |
| `internal/codexonly/chat_completions.go` | Chat Completions 到 Responses 的转换和响应翻译。 |
| `internal/codexonly/user_store.go` | SQLite Schema、用户、API Key 和认证。 |
| `internal/codexonly/session_affinity.go` | 有界信号提取、Digest、持久化绑定、续期和 CAS 重绑定。 |
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
6. 解析 SQLite 路径、执行幂等 Schema Migration、验证初始持久化用户状态，
   删除指向已不存在认证身份的亲和性目标，并恢复未过期的权威认证健康状态。
7. 启动一个 `net/http` 服务器。

服务器将 `ReadHeaderTimeout` 设置为 10 秒。它不会设置可能中断已建立 Stream
或 WebSocket 流量的 Read/Write Timeout。优雅关闭超时为 10 秒；超过该期限后，
会强制关闭剩余 HTTP 连接。

## 存储故障生命周期

SQLite 是进程级硬依赖。数据库路径、打开、Migration 或初始状态加载失败都会
阻止启动。

运行期间，第一个非预期 SQLite 读写故障会成为进程级致命错误。它会取消活动的
代理请求 Context、关闭已建立 WebSocket 桥接的两端、启动有界 HTTP 关闭，并使
服务器进程带错误退出。后续并发故障会复用第一个致命错误，不会启动额外关闭
流程。

预期应用错误不会触发该生命周期，包括无效输入、记录不存在、凭据已禁用或
无效、已处理的约束冲突以及已取消的请求 Context。

进程不提供降级、仅内存或自动数据库恢复。生产部署必须使用 systemd、Docker、
Kubernetes 或其他外部 Supervisor，在修复底层 SQLite 问题后重启进程。

## OAuth 凭据流

`FileAuthStore` 在选择凭据时递归扫描配置目录，并将结果与上一次成功扫描进行
协调。

每个上游请求都会：

1. 解析 Codex 认证文件，并保留禁用记录用于协调。
2. 从 `account_id`、Token 声明或规范化路径回退值解析稳定身份。
3. 将同一账户的重复文件合并为一个逻辑凭据。
4. 按稳定身份排序可选择的逻辑凭据。
5. 跟随有效的租户范围会话绑定；不存在绑定时，以轮询方式选择下一个逻辑凭据。
6. 将五分钟内过期的凭据视为已过期。
7. 按稳定凭据 ID 在进程内协调刷新，使同一凭据的并发调用方共享一次有效刷新。
8. 当其他调用方已经替换过期或收到 `401` 的 Token 时，仅在相同稳定凭据 ID
   仍然存在的情况下复用较新的 Access Token。
9. 刷新过期凭据，并重新解析账户和邮箱声明。
10. 同目录 `0600` 临时文件完成 Sync 后，原子替换选中的源文件。
11. 使用选中的 Access Token 和 Account ID 转发请求。

协调结果会报告新增、删除、凭据变化、元数据变化、可用性变化和源文件变化，
且不包含 Token 内容。稳定账户 ID 在文件重命名和同账户 Token 替换后保持不变。
在同一路径换入另一账户时，会产生旧身份删除和新身份新增。禁用的逻辑凭据仍然
保留在状态中，但不会用于新请求选择。五秒解析错误宽限期会在编辑器部分写入
期间保留最近一次有效表示；持续格式错误的文件随后会被排除。

每次有效 Token 刷新共用一个 30 秒 Deadline，最多尝试三次。仅临时网络故障和
HTTP `408`、`429`、`500`、`502`、`503`、`504` 可以重试；有效的
`Retry-After` 仅在刷新 Deadline 内执行。缺少刷新凭据、`invalid_grant`、
确定性的 `400`/`401`/`403`、格式错误的响应以及不含 Access Token 的成功响应
都是终止错误。刷新错误只公开安全的状态码和 OAuth 错误码上下文，不包含 Token
端点原始响应 Body。

## 认证健康与重试

在该单进程实现中，认证健康以稳定逻辑凭据为全局范围。所有会话都会跳过已禁用、
正在冷却、凭据无效、刷新后继续未授权或不支持请求模型的凭据。健康时会话亲和性
保持不变；需要故障转移时，使用现有 SQLite Compare-and-swap 重绑定。每个请求
会快照其开始时已知的认证 ID，因此新加入的替代身份不能接管正在执行的请求。

代理会在提交下游响应前分类上游结果：

- 请求范围的 `4xx` 会停止请求，但不改变认证健康。
- 可重放时，`401` 会执行一次协调的同认证刷新和重试；再次返回 `401` 会阻止
  该凭据。
- `429` 使用 `Retry-After` 或显式 Codex 配额重置 Deadline。
- 网络故障、`408` 和可重试 `5xx` 会创建短期内存冷却。
- 模型不支持响应只创建认证和模型组合的排除，不会形成全局冷却。可缓存模型 ID
  使用安全的 128 字节标识符格式；每个认证最多保留 64 个排除，并以确定性的
  最旧条目淘汰策略限制容量。

重试使用三个独立预算。同认证 `401` 修复不计入凭据预算。一个执行轮次最多尝试
`max-retry-credentials` 个不同且可用的认证，零表示全部。轮次结束后，仅当最近
冷却不超过 `max-retry-interval` 时等待，随后最多启动 `request-retry` 个额外
轮次。Context 取消会立即中断选择、刷新和冷却等待。

跨认证重试要求路由显式可重放。符合条件的 JSON 请求只在内存中缓冲，最大包含
32 MiB，不会为重试写入磁盘。未知长度、超大、Multipart、文件、Realtime、
具有副作用的 Wham、Hosted MCP 和未知写请求保持单次执行。Responses WebSocket
Handshake 可以在 Upgrade 成功前故障转移。HTTP Stream 和 WebSocket 在下游提交
后都不会重新进入重试。

只有显式配额 Deadline、`invalid_grant` 和刷新后继续未授权的状态会存储在
`auth_health_states`。临时冷却和模型排除只保存在内存中。凭据状态 Fingerprint
会在实际 Token 材料变化后使持久化凭据故障失效，而同账户 Token 更新会保留
未过期的配额 Deadline。

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

## 会话亲和性

会话亲和性默认启用，并存储在与托管用户和用量相同的 SQLite 数据库中。

代理只接受以下显式信号：

- `Session-Id` 或 `Session_id` 请求 Header。
- 顶层 JSON `session_id` 或 `sessionId`。
- 顶层 JSON `prompt_cache_key`。
- 顶层 JSON `conversation_id` 或 `conversation.id`。

信号值会去除首尾空白，限制为 512 字节；空值、无效 UTF-8、未配对的 UTF-16
Surrogate Escape 或包含控制字符的值不会用于亲和性。JSON 请求 Body 以 Token
Stream 方式解析，不设置亲和性专用的 Body 大小上限。Replay 最多在内存中保留
64 KiB，超过该阈值后使用权限为 `0600` 的临时文件，从而仍能原样转发完整
Body。如果临时 Replay 存储无法创建或写入，检查会在产生额外无界缓冲前停止，
丢弃 Body 派生信号，并将已保存的前缀与未读取的源请求流拼接后进行原样单次
转发。无效、缺失或超大信号不会拒绝或截断代理请求，而是回退到正常轮询选择。

托管请求按稳定用户 ID 划分，因此 API Key 轮换会保留绑定，不同用户不会碰撞。
OAuth 兼容请求按认证该请求的 Access Token 所属稳定身份划分。模型名称不属于
亲和性 Key。

数据库只存储由租户范围、信号类型和规范化值派生的 SHA-256 Digest。同一请求中
观察到的 Prompt Cache、Conversation 和 Session 别名共享一个仅含 Digest 的
绑定组。首次绑定使用原子插入；并发首次请求跟随数据库获胜者。重绑定使用
Compare-and-swap，使并发故障转移收敛，而不会覆盖其他请求的获胜结果。

绑定在一小时无活动后过期。活动绑定仅在距上次持久化写入至少 30 分钟后续期，
过期行以有界批次清理。绑定在进程重启后保留。禁用认证不会删除空闲绑定；禁用
期间复用会重绑定到有效认证。删除或替换认证身份会使指向旧身份的绑定失效。

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
3. 协调认证文件和全局健康，然后解析健康的会话亲和性，或选择下一个可用凭据。
4. 将目标 URL 重写到配置的 Codex 或 ChatGPT Base。
5. 使用选中的 OAuth Access Token 替换 `Authorization`。
6. 可用时添加 ChatGPT Account ID 和兼容 Header。
7. 仅当请求可重放且尚未提交客户端响应时，应用同认证修复、不同凭据故障转移
   和有界冷却轮次。
8. 更新认证健康；选择切换到其他凭据时使用亲和性 CAS。
9. 单次转发不可重放请求，转发 HTTP Stream，或在 Handshake 成功后桥接
   WebSocket Frame。
10. 所有候选耗尽时返回确定且安全的聚合错误。
11. 为托管用户请求采集用量元数据。

正常运行期间，代理会保持已建立的 HTTP Stream。WebSocket 转发使用 Gorilla
WebSocket，并在上游 Upgrade 路径强制使用 HTTP/1.1 ALPN。服务器关闭或发生
致命存储故障时，会取消已建立的 Stream 并关闭 WebSocket 两端。响应提交或
Upgrade 成功后不会重试。不可重放的请求 Body 会保留第一次上游响应，不进行
修改。

## Chat Completions 转换

`/v1/chat/completions` 在本地实现，不直接透传：

1. 解码 Chat Completions JSON 对象。
2. 将消息、Tool、Response Format、Reasoning 和 Service Tier 转换为 Responses
   请求。
3. 强制上游使用 `stream: true` 和 `store: false`。
4. 使用与可重放 Responses 请求相同的健康感知提交前重试执行器。
5. 读取 Responses SSE Event。
6. 聚合为普通 Chat Completions 响应，或转换为 Chat Completions SSE Chunk。
7. 应用本地 Stop Sequence 过滤并记录用量。

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

### 会话亲和性绑定

绑定包含：

- 一个或多个租户范围的 SHA-256 会话 Digest。
- 仅含 Digest 的别名组标识符。
- 选中的稳定 OAuth 凭据 ID。
- 创建、续期和过期时间。

绑定表不会存储原始 Session ID、Prompt Cache Key、Conversation ID、托管 API
Key 或模型名称。

### 认证健康状态

一个权威持久化认证健康行包含：

- 稳定 OAuth 凭据 ID。
- 状态类型和安全原因。
- 可选恢复 Deadline。
- 凭据相关状态使用的 Credential Fingerprint。
- 安全的上游状态码和错误码。
- 更新时间。

该表不会存储 Access Token、Refresh Token、原始上游 Body 或短期传输冷却。

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
原始会话亲和性信号不会写入 SQLite、由管理 API 返回或写入调试日志。大型 JSON
请求 Body 可以在该请求生命周期内暂存到进程拥有、权限为 `0600` 的临时 Replay
文件中；Replay 关闭时会删除该文件，包括后续请求处理步骤替换 Replay Body 时。
该临时存储仅用于亲和性信号提取。跨认证重试 Body 只存在于内存中，并限制为
32 MiB。

服务器自身提供 HTTP。监听地址选择和传输终止属于部署环境职责。
