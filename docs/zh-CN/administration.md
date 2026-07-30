# 管理

[English](../administration.md) | [简体中文](administration.md)

代理提供两个管理入口：

- 由内置管理 CLI 使用、仅允许回环地址访问的本地路由。
- 由 `admin-api-key` 保护的可选远程管理 API。

两个入口调用相同的用户和用量 Handler，并操作同一个 SQLite 数据库。

## 本地管理 CLI

CLI 默认连接 `http://127.0.0.1:8317`：

```bash
codex-oauth-proxy admin users list
```

它调用 `/v0/local-admin/*`。服务器只在请求远端地址为回环 IP
（`127.0.0.1` 或 `::1`）时接受该路由，不需要也不会发送
`admin-api-key`。

使用 `--url` 连接同一本地服务的其他端口或路径：

```bash
codex-oauth-proxy admin users list \
  --url http://127.0.0.1:8318
```

任意管理命令都可以使用 `--json` 输出机器可读数据：

```bash
codex-oauth-proxy admin users list --json
```

全局管理 Flag 可以放在叶子命令之后。

## 用户命令

### 列出用户

```bash
codex-oauth-proxy admin users list
codex-oauth-proxy admin users list --enabled true
codex-oauth-proxy admin users list --enabled false
```

### 创建用户

```bash
codex-oauth-proxy admin users create alice
```

用户名必填，并且不区分大小写地保持唯一。新用户默认启用，并获得一个有效的
生成 API Key。

创建输出会包含一次明文 Key。

### 获取用户

```bash
codex-oauth-proxy admin users get usr_xxx
```

### 重命名用户

```bash
codex-oauth-proxy admin users update usr_xxx --name alice2
```

### 启用或禁用用户

```bash
codex-oauth-proxy admin users enable usr_xxx
codex-oauth-proxy admin users disable usr_xxx
```

禁用用户后，该用户不能认证代理或用户 API 请求。存储的 API Key 仍然存在，
重新启用用户后可以继续使用。

### 重置 API Key

```bash
codex-oauth-proxy admin users reset-key usr_xxx
```

重置 Key 会：

- 禁用该用户此前所有有效 Key。
- 创建一个新的有效 Key。
- 只返回一次新明文 Key。
- 不删除归属到旧 Key ID 的历史用量。

## 用量命令

### 滚动快照

```bash
codex-oauth-proxy admin usage snapshot
codex-oauth-proxy admin usage snapshot --user-id usr_xxx
codex-oauth-proxy admin usage snapshot --api-key-id key_xxx
```

人类可读输出会显示滚动 5 小时和 7 天窗口的请求数与 Token 总量。

### 时间序列

```bash
codex-oauth-proxy admin usage timeseries \
  --window 7d \
  --step 1h \
  --group-by user
```

可用 Flag：

| Flag | 可选值 |
| --- | --- |
| `--window` | `5h`、`24h`、`7d`、`30d`、`today` |
| `--step` | `auto`、`10m`、`30m`、`1h`、`6h`、`1d` |
| `--group-by` | 逗号分隔的 `user`、`api_key`、`model`、`reasoning_effort`、`service_tier` |
| `--fill` | `none` 或 `zero` |
| `--user-id` | 一个用户 ID |
| `--api-key-id` | 一个 API Key ID |

默认窗口为 `7d`，默认分组为 `user`。自动 Step 大小取决于所选窗口。

## Docker 管理

服务器在容器中运行时，宿主机 CLI 连接在容器看来不是回环连接。需要在代理
容器内运行 CLI：

```bash
docker exec codex-oauth-proxy \
  /codex-oauth-proxy/codex-oauth-proxy admin users list
```

容器外脚本或 Grafana 应启用并使用远程管理 API。

## 远程管理 API

设置非空 Key：

```yaml
admin-api-key: "replace-with-a-long-random-secret"
```

然后使用以下任一方式认证 `/v0/management/*` 请求：

```http
Authorization: Bearer <admin-api-key>
```

或：

```http
X-API-Key: <admin-api-key>
```

`admin-api-key` 为空时，远程管理路由返回 `404`。回环客户端仍可使用本地管理
路由。

示例：

```bash
curl http://127.0.0.1:8317/v0/management/users \
  -H 'Authorization: Bearer admin-change-me'
```

请求和响应格式参见 [API 参考](api-reference.md)。

## 用户自助服务

托管用户 API Key 可以访问：

- 当前用户和 Key 元数据。
- API Key 重置。
- 当天用量。

通过用户 API 重置 Key 会使当前请求使用的凭据失效，并返回新的明文 Key。调用方
必须切换到新 Key。

## Key 处理

- 托管 Key 使用 `cop_` 前缀。
- 持久化时只存储用于认证的 SHA-256 Hash。
- 列表和详情响应包含 `key_prefix` 和 `masked_key`，不包含明文。
- 只有创建和重置操作返回明文。
- 一个用户同时只能有一个启用的 API Key。
- 重置 Key 会立即禁用旧 Key。
