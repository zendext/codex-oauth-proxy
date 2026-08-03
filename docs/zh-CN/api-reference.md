# API 参考

[English](../api-reference.md) | [简体中文](api-reference.md)

本文档说明 `codex-oauth-proxy` 自有的 API 行为和受支持的代理路由范围，不复制
完整的上游 Responses API Schema。

## Base URL

示例使用：

```text
http://127.0.0.1:8317
```

OpenAI 兼容客户端 Base URL：

```text
http://127.0.0.1:8317/v1
```

## 认证

服务器从以下位置读取候选凭据：

```http
Authorization: Bearer <token>
```

以及：

```http
X-API-Key: <token>
```

两者同时存在时，任一匹配凭据都可以认证请求。

| 路由组 | 凭据 |
| --- | --- |
| `/`、`/healthz` | 无 |
| `/v1/*` | 托管的 `cop_...` 用户 API Key |
| `/v0/user/*` | 托管的 `cop_...` 用户 API Key |
| `/v0/management/*` | 配置的 `admin-api-key` |
| `/v0/local-admin/*` | 回环源地址；无需 Key |
| 部分 `/backend-api/*` 兼容路由 | 托管用户 API Key；部分路由也接受当前已加载的 Codex Access Token |

禁用用户会收到 `403`。缺失、未知或已经轮换的托管 Key 会收到 `401`。

## 会话亲和性

对于代理请求，服务器接受 `Session-Id` 或 `Session_id` Header，以及 JSON 字段
`session_id`、`sessionId`、`prompt_cache_key`、`conversation_id` 和
`conversation.id`。

有效信号会让同一个逻辑会话在模型变化、API Key 轮换、并发首次请求和进程重启
后继续使用同一个可用 OAuth 凭据。托管绑定按用户隔离；兼容 Token 绑定按匹配
到的稳定 OAuth 身份隔离。

超过 512 字节的值、空值、无效 UTF-8、未配对的 UTF-16 Surrogate Escape 和包含
控制字符的值不会用于亲和性。JSON Body 以 Token Stream 方式检查，不设置亲和性
专用的 Body 大小上限，并会在转发前原样恢复完整 Body。如果临时 Replay 存储
无法创建或写入，Body 检查会停止，Body 派生信号会被忽略，并原样转发已保存的
前缀与未读取的请求流。被忽略或缺失的信号不会导致请求失败；选择会回退到正常
轮询行为。

## 健康感知故障转移

认证健康跨会话全局生效。健康的亲和性绑定会继续使用当前 Codex 凭据；绑定凭据
被禁用、正在冷却、凭据无效、刷新后继续未授权或无法服务请求模型时，会通过
Compare-and-swap 故障转移进行重绑定。

跨凭据重试只适用于：

- 白名单路由上的只读 `GET` 和 `HEAD` 请求。
- JSON `/v1/chat/completions`。
- Responses、Responses Compact、Alpha Search、JSON Image Generation 和
  Trace Summarization。
- Upgrade 成功前的 Responses WebSocket Handshake。

可重放请求 Body 只在内存中缓冲，最大包含 32 MiB。未知长度或更大的 Body，
以及 Multipart、文件、Realtime、具有副作用的 Wham、Hosted MCP 和未知写请求
只发送一次，不会仅因无法自动重放而被拒绝。

在一次可重放执行中，`401` 首先刷新并重试同一凭据一次。随后一个凭据轮次尝试
不同且可用的凭据。轮次结束后，代理可以根据 `request-retry`、
`max-retry-credentials` 和 `max-retry-interval` 等待最近冷却并启动下一轮。
取消会立即停止等待。下游响应字节提交或 WebSocket Upgrade 成功后不会重试。

## 错误格式

项目自有 Handler 返回：

```json
{
  "error": {
    "message": "invalid API key",
    "type": "Unauthorized"
  }
}
```

代理的上游路由可能会保留上游状态码、Header 和响应 Body。请求范围的上游
`4xx` 会立即停止，且不会惩罚凭据。

可重放候选耗尽后，代理返回安全的聚合错误：

```json
{
  "error": {
    "message": "upstream Codex service unavailable",
    "type": "proxy_error",
    "code": "upstream_unavailable"
  }
}
```

确定性聚合结果如下：

| 状态 | Code | 含义 |
| --- | --- | --- |
| `404` | `model_not_found` | 所有已知候选都因请求模型而被排除。 |
| `429` | `rate_limited` | 所有可服务候选都受配额限制，或混合故障存在明确的近期配额恢复时间。响应包含已知最早的 `Retry-After`。 |
| `503` | `auth_unavailable` | 所有候选都已禁用、凭据无效或刷新后继续未授权。 |
| `502` | `upstream_unavailable` | 网络或可重试上游故障已经耗尽，包括没有近期恢复 Deadline 的混合故障。 |

客户端取消不会生成新的代理错误。

## 服务端点

### `GET /`

返回：

```json
{"message":"codex-oauth-proxy"}
```

### `GET /healthz`

返回：

```json
{"status":"ok"}
```

健康端点只检查 HTTP Handler 正在运行，不会向上游 Codex 发起请求。

## 管理 API

只有 `admin-api-key` 非空时才能使用远程管理 API。回环客户端可以使用
`/v0/local-admin` 下的等价本地路由。所有管理响应都包含
`Cache-Control: no-store`。

### `GET /v0/management/auths`

列出所有逻辑 Codex 认证，包括已禁用和未识别的凭据：

```json
{
  "auths": [
    {
      "account_id": "acct_xxx",
      "identity_state": "identified",
      "manageable": true,
      "email": "a***@example.com",
      "source_file_count": 2,
      "enabled": true,
      "runtime_state": "cooling",
      "runtime_reason": "quota",
      "token_expires_at": "2026-08-02T00:00:00Z",
      "last_refresh_at": "2026-07-31T00:00:00Z",
      "cooldown_until": "2026-08-01T01:00:00Z",
      "cooldown_reason": "quota",
      "model_capability_known": true,
      "known_supported_models": ["gpt-5.3-codex"],
      "model_exclusions": [],
      "session_binding_count": 3,
      "active_connection_count": 1,
      "last_error": {
        "code": "rate_limit_exceeded",
        "status": 429
      }
    }
  ]
}
```

`runtime_state` 为 `active`、`cooling` 或 `unavailable`。模型排除会单独
报告，因为它们只影响一个模型，而不是整个认证。只有运行时目录已知按认证支持
集合时，才会填充已知支持模型。

无法恢复账户 ID 的认证返回 `identity_state: "unidentified"`、
`manageable: false`，并且没有 `account_id`。它仍可用于兼容代理流量，但不能
接受按账户定位的变更。

响应永远不会包含 OAuth Token、原始认证 JSON、源文件路径、原始会话标识符或
原始上游错误 Body。

### `POST /v0/management/auths/refresh`

对一个已识别认证强制执行 OAuth 刷新：

```json
{"account_id":"acct_xxx"}
```

认证处于禁用状态时也允许此操作，并且始终进入现有按认证 Refresh
Singleflight。OAuth 操作总 Deadline 为 30 秒。成功会清除凭据相关故障，但
不会启用认证、清除配额或模型冷却，也不会改变会话绑定。

响应：

```json
{"auth": { "...": "更新后的安全认证状态" }}
```

Token 端点或持久化失败只返回脱敏错误，不包含 Token 或上游原始 Body。

### `POST /v0/management/auths/enable`

### `POST /v0/management/auths/disable`

请求：

```json
{"account_id":"acct_xxx"}
```

这些操作通过刷新使用的同一同步临时文件和原子重命名路径，更新该账户每个认证
文件中的 `disabled` 字段。写入前会先解析并验证所有源文件。多文件写入失败时
会尽可能回滚已经修改的源文件，并且绝不会返回混合状态的成功响应。

启用不会发起 OAuth 请求，也不会清除健康状态、冷却、模型排除、活动请求或会话
绑定。禁用完成后会把认证排除在新选择之外。现有 HTTP、SSE 和 WebSocket 请求
继续运行；空闲绑定会一直保留到后续复用、过期、显式清理或认证删除。

### `POST /v0/management/auths/cooldown/clear`

请求：

```json
{"account_id":"acct_xxx"}
```

响应：

```json
{
  "cleared": true,
  "auth": { "...": "更新后的安全认证状态" }
}
```

只能清理基于时间的配额或 `429`、网络、`408` 和可重试 `5xx` 冷却。此操作
不会清除禁用状态、`invalid_grant`、缺失或无效的刷新凭据、持续未授权状态或
模型专用排除；这些情况下 `cleared` 为 `false`。

### `POST /v0/management/session-bindings/clear`

请求必须且只能选择一个范围。

一个用户的一条原始会话 Key：

```json
{
  "user_id": "usr_xxx",
  "session_key": "raw-session-key"
}
```

一个用户的全部绑定：

```json
{"user_id":"usr_xxx"}
```

用户范围要求省略 `session_key`。如果该字段存在但为空、只含空白或其他无效值，
则返回 `400`，并且不会清理任何绑定。

指向一个已识别认证的全部绑定：

```json
{"account_id":"acct_xxx"}
```

响应：

```json
{"deleted_count":2}
```

精确会话清理会在服务端从原始 Key 计算所有受支持的租户范围会话 Digest，并
删除完整 Alias Group。原始 Key 永远不会返回、记录日志或持久化。不提供无条件
全局清理，也不提供把会话迁移到管理员指定认证的操作。

### `POST /v0/management/users`

创建用户和初始 API Key。

请求：

```json
{
  "name": "alice",
  "enabled": true
}
```

`enabled` 可选，默认为 `true`。

响应状态：`201 Created`

```json
{
  "user": {
    "id": "usr_xxx",
    "name": "alice",
    "enabled": true,
    "created_at": "2026-07-30T00:00:00Z",
    "updated_at": "2026-07-30T00:00:00Z"
  },
  "api_key": {
    "id": "key_xxx",
    "user_id": "usr_xxx",
    "key_prefix": "cop_...",
    "masked_key": "cop_...abcd",
    "enabled": true,
    "created_at": "2026-07-30T00:00:00Z"
  },
  "api_key_value": "cop_plaintext_returned_once"
}
```

可能的错误：

- 名称为空或无效时返回 `400`。
- 名称不区分大小写地重复时返回 `409`。

### `GET /v0/management/users`

列出用户及其有效 Key 元数据。

可选 Query：

```text
enabled=true
enabled=false
```

响应：

```json
{
  "users": [
    {
      "user": {
        "id": "usr_xxx",
        "name": "alice",
        "enabled": true,
        "created_at": "2026-07-30T00:00:00Z",
        "updated_at": "2026-07-30T00:00:00Z"
      },
      "api_key": {
        "id": "key_xxx",
        "user_id": "usr_xxx",
        "key_prefix": "cop_...",
        "masked_key": "cop_...abcd",
        "enabled": true,
        "created_at": "2026-07-30T00:00:00Z"
      }
    }
  ]
}
```

### `GET /v0/management/users/{user_id}`

返回一个用户及有效 Key 元数据。用户不存在时返回 `404`。

### `PATCH /v0/management/users/{user_id}`

更新一个或两个字段：

```json
{
  "name": "alice2",
  "enabled": false
}
```

返回更新后的用户和有效 Key 元数据。

### `POST /v0/management/users/{user_id}/api-key/reset`

禁用此前的有效 Key，并返回一个新 Key。

响应状态：`200 OK`

响应格式与创建用户相同，并且只包含一次 `api_key_value`。

### `GET /v0/management/usage`

返回滚动用量快照。

可选 Query：

```text
user_id=usr_xxx
api_key_id=key_xxx
```

响应：

```json
{
  "usage": [
    {
      "user_id": "usr_xxx",
      "name": "alice",
      "api_key_id": "key_xxx",
      "masked_key": "cop_...abcd",
      "windows": {
        "5h": {
          "request_count": 1,
          "total_tokens": 100
        },
        "7d": {
          "request_count": 2,
          "total_tokens": 200
        }
      },
      "models": []
    }
  ]
}
```

计数器对象还可能包含：

- `failed_request_count`
- `input_tokens`
- `output_tokens`
- `reasoning_tokens`
- `cached_input_tokens`
- `cache_read_tokens`
- `cache_creation_tokens`

### `GET /v0/management/usage/timeseries`

Query 参数：

| 参数 | 默认值 | 可选值 |
| --- | --- | --- |
| `window` | `7d` | `5h`、`24h`、`7d`、`30d`、`today` |
| `step` | `auto` | `10m`、`30m`、`1h`、`6h`、`1d` |
| `group_by` | `user` | 重复传递或用逗号分隔 `user`、`api_key`、`model`、`reasoning_effort`、`service_tier` |
| `fill` | 无 | `none`、`zero` |
| `user_id` | 空 | 一个用户 ID |
| `api_key_id` | 空 | 一个 API Key ID |

响应：

```json
{
  "window": "7d",
  "step": "1h",
  "start": "2026-07-23T00:00:00Z",
  "end": "2026-07-30T00:10:00Z",
  "group_by": ["user"],
  "series": [
    {
      "bucket_start": "2026-07-30T00:00:00Z",
      "user_id": "usr_xxx",
      "name": "alice",
      "request_count": 1,
      "total_tokens": 100
    }
  ]
}
```

窗口、Step、分组、Fill 或过滤条件无效时返回 `400`。

## 用户 API

这些路由要求使用被访问数据所属用户的托管 Key。

### `GET /v0/user/api-key`

返回认证用户和当前 Key 元数据，永远不会返回明文。

### `POST /v0/user/api-key/reset`

轮换认证用户的 Key，并只返回一次 `api_key_value`。用于发起重置请求的 Key
会立即被禁用。

### `GET /v0/user/usage/today`

返回从 UTC `00:00:00` 到当前用量桶的总计：

```json
{
  "user_id": "usr_xxx",
  "api_key_id": "key_xxx",
  "date": "2026-07-30",
  "request_count": 2,
  "input_tokens": 100,
  "output_tokens": 50,
  "total_tokens": 150,
  "models": []
}
```

## OpenAI 兼容 API

### `GET /v1/models`

不包含 `client_version` 时，返回从 `codex-user-agent` 派生版本所对应同步目录
的 OpenAI 风格视图：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5.4",
      "object": "model",
      "owned_by": "openai"
    }
  ]
}
```

Query 中存在 `client_version` Key 时，响应使用该精确规范化版本的 Codex CLI
目录格式。空值使用相同的已配置 User-Agent 版本：

```json
{"models":[]}
```

版本为冷缓存或已过期时，代理会为每个有效逻辑凭据并发 Fetch 已认证上游目录，
等待所有结果，然后返回确定性的模型 Slug 并集。重复 Slug 只选择一个完整的
按认证对象，不会跨账户合并字段。

成功的按认证 Snapshot TTL 为三小时。刷新失败时复用过期 Snapshot，并安排一个
去重的后台任务执行三次额外重试。一个认证失败不会阻止其他成功认证。如果没有
任何认证拥有可用 Snapshot，则回退到
`internal/codexonly/codex_client_models.json`。

版本最大为 64 个安全 ASCII 字节；无效值返回 `400`。内存中最多保留 16 个
规范化版本并使用 LRU 淘汰。除非启用 `allow-fast-mode`，否则会移除 Fast Tier
元数据。

### `POST /v1/chat/completions`

该端点将 Chat Completions 输入转换为上游 Responses 请求。代理始终向上游请求
Stream，然后聚合响应或转换回 Chat Completions SSE。

必填字段：

- `model`
- `messages`

支持的行为包括：

- `system`、`developer`、`user`、`assistant` 和 `tool` 消息。
- 文本和 `image_url` 消息内容。
- Function Tool、Tool Choice、并行 Tool Call、Tool Call 历史和 Tool 输出。
- `stream` 和 `stream_options.include_usage`。
- 使用 `json_object` 或 `json_schema` 的 `response_format`。
- `reasoning`、`reasoning_effort` 和 `extra_body.reasoning`。
- `verbosity`。
- 字符串或列表形式的 `stop`。
- `service_tier`。

Reasoning Effort 别名会被规范化：

| 输入 | 上游值 |
| --- | --- |
| `minimal` | `low` |
| `max` | `xhigh` |

Sampling 和其他未知 Chat Completions 字段不会自动转发。需要完整 Responses
行为的客户端应直接调用 `/v1/responses`。

转换器要求上游恰好出现一个成功终态 Event。`response.completed` 表示完整成功；
`response.incomplete` 表示部分成功，并使用以下标准 Chat Completions
`finish_reason`：

| Incomplete Reason | `finish_reason` |
| --- | --- |
| `max_tokens`、`max_output_tokens` | `length` |
| `content_filter` | `content_filter` |
| 未知值 | `length` |

Incomplete Reason 的优先级高于 `tool_calls`。响应不会包含非标准
`native_finish_reason` 字段。

对于流式请求，代理会等待第一个有效上游 Event，再提交下游 SSE 响应。提交前的
故障使用与 HTTP 故障相同的 OAuth 刷新、健康冷却、模型故障转移和请求错误规则
分类。提交后的故障会发送一个经过脱敏的 OpenAI Error Envelope `data:` Event，
然后关闭 Stream，且不发送 Finish Chunk 或 `[DONE]`。

成功终态前 EOF、格式错误或超大的 Event、重复终态以及终态后的数据均视为
故障。单个上游 SSE Event 限制为 50 MB。代理按 Output Index 协调
`response.output_item.done` 快照与终态 Output；非流式响应以终态 Output 为准，
流式响应只能补发缺失后缀。本地 Stop 匹配在跨 Event 边界时保持 UTF-8 安全，
命中后会抑制后续文本和 Tool Delta，同时继续读取终态和用量。

示例：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer cop_...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.4",
    "messages": [
      {"role": "user", "content": "Hello"}
    ],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

## Responses 与兼容路由

代理转发以下白名单 Codex 路由，但不定义其完整上游请求 Schema：

这些原生 Responses HTTP 和 WebSocket 路由保持透明；上述 Chat Completions
终态校验和错误转换不会修改其 Event 或 Frame Payload。

| 常用方法 | 公共路径 |
| --- | --- |
| `POST`、`GET`、WebSocket Upgrade | `/v1/responses` |
| `POST` | `/v1/responses/compact` |
| `POST` | `/v1/alpha/search` |
| `POST` | `/v1/images/generations` |
| `POST` | `/v1/images/edits` |
| `POST` | `/v1/memories/trace_summarize` |
| `POST` | `/v1/realtime/calls` |
| `GET`、WebSocket Upgrade | `/v1/realtime` |

图片生成通常通过 Responses API 的图片 Tool 使用。两个 `/v1/images/*` 路径是
有限的兼容路由，其他图片路径不会被代理。

禁用 Fast 模式时，包含 `service_tier: "fast"` 或 `"priority"` 的请求会在
转发上游之前被拒绝。

## Codex CLI 内部兼容

以下路径仅用于支持已知 Codex CLI 行为，不是稳定的第三方 API 契约：

- 白名单 `/v1` Codex 路由的 `/backend-api/codex` 别名。
- `POST /files`
- `POST /files/{file_id}/uploaded`
- `/backend-api/wham/usage`
- `/backend-api/wham/profiles/me`
- `/backend-api/wham/accounts/check`
- `/backend-api/wham/accounts/send_add_credits_nudge_email`
- `/backend-api/wham/apps`
- `/backend-api/ps/mcp`

代理不会暴露任意 `/backend-api/*` 路径。通用客户端应使用受支持的 `/v1/*`
和 `/v0/*` 接口。
