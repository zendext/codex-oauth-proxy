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

代理的上游路由可能会保留上游状态码、Header 和响应 Body。

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
`/v0/local-admin` 下的等价本地路由。

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

不包含 `client_version` 时，返回 OpenAI 风格的模型列表：

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

Query 中存在 `client_version` Key 时，响应使用嵌入式 Codex CLI 模型目录格式：

```json
{"models":[]}
```

实际列表来自 `internal/codexonly/codex_client_models.json`。除非启用
`allow-fast-mode`，否则会移除 Fast Tier 元数据。

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
