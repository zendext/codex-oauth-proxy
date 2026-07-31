# 用量与可观测性

[English](../usage-and-observability.md) |
[简体中文](usage-and-observability.md)

代理为使用托管用户 API Key 认证的请求记录本地用量统计。SQLite 是唯一事实
来源；可选 Grafana 组件通过管理 API 读取数据，不维护第二份用量数据库。

## 归属

只有代理认证解析出已存储的托管用户和 API Key 时，才会创建用量记录。

记录的身份包括：

- 用户 ID。
- API Key ID 和脱敏元数据。
- 选中的稳定 OAuth 凭据 ID。
- 请求 ID。

内部兼容路由上，仅通过当前已加载 Codex OAuth Access Token 接受的请求不会
归属到托管用户，也不计入用量总计。

## 计数器

用量记录可能包含：

- 请求数。
- 失败请求数。
- Input Token。
- Output Token。
- Reasoning Token。
- Cached Input Token。
- Cache Read Token。
- Cache Creation Token。
- Token 总量。

最终状态为 `400` 或更高，或者没有可用的最终上游状态时，请求计为失败。

Token 计数器从 JSON、SSE 和最终 WebSocket Response Event 中提取。代理会记录
能够获得的计数器；没有用量元数据的上游响应仍可能计入请求数和失败请求数。

## 维度

用量按照以下维度分离：

- 用户。
- API Key。
- 模型。
- Reasoning Effort。
- Service Tier。
- 选中的稳定 OAuth 凭据。

空模型、Reasoning 或认证值会规范化为 `unknown`。

Service Tier 规范化规则：

| Wire Value | 存储值 |
| --- | --- |
| 空或无法识别 | `standard` |
| `fast` | `fast` |
| `priority` | `fast` |

## 桶与保留

记录聚合到 UTC 10 分钟桶中。桶 Key 包含身份和模型维度，因此同一时段内使用
不同模型、Reasoning Effort、Service Tier 或 OAuth 凭据的请求仍然可以区分。

桶数据保留 30 天。记录新用量时会清理旧桶。

内置滚动窗口：

- `5h`：30 个 10 分钟桶。
- `7d`：1,008 个 10 分钟桶。

用户当天用量从 UTC `00:00:00` 开始计算。

## 用户用量

托管用户可以查询：

```text
GET /v0/user/usage/today
```

响应包含认证用户和当前 API Key 的总计与模型拆分。

## 管理快照

管理员可以查询：

```text
GET /v0/management/usage
```

可选过滤条件：

```text
user_id=usr_xxx
api_key_id=key_xxx
```

每个结果包含 5 小时和 7 天总计，以及模型、Reasoning 和 Service Tier 维度。

等价的本地 CLI 命令：

```bash
codex-oauth-proxy admin usage snapshot
```

## 时间序列

管理员可以查询：

```text
GET /v0/management/usage/timeseries
```

支持的窗口：

- `5h`
- `24h`
- `7d`
- `30d`
- `today`

支持的 Step：

- `10m`
- `30m`
- `1h`
- `6h`
- `1d`

`step` 为空或为 `auto` 时，默认值如下：

| 窗口 | 自动 Step |
| --- | --- |
| `5h` | `10m` |
| `7d` | `6h` |
| `30d` | `1d` |
| `24h`、`today` | `1h` |

支持的分组维度：

- `user`
- `api_key`
- `model`
- `reasoning_effort`
- `service_tier`

可以传递多个 `group_by` Query Value，或使用逗号分隔的值。默认为 `user`。

使用 `fill=zero` 为缺失的分组与时间组合生成显式零值点。不使用该参数时，只
返回实际存储的桶。

CLI 示例：

```bash
codex-oauth-proxy admin usage timeseries \
  --window 7d \
  --step 1h \
  --group-by user,model \
  --fill zero
```

## 禁用统计

```yaml
usage:
  enabled: false
  debug-openai-response: false
```

禁用统计会停止写入新记录。现有 SQLite 桶会保留，直到后续启用写入时被清理，
或由运维者移除。

## 调试诊断

普通请求诊断：

```yaml
debug: true
```

额外用量元数据：

```yaml
debug: true
usage:
  enabled: true
  debug-openai-response: true
```

用量诊断包含安全的请求 ID、模型维度、脱敏 Key 元数据、状态、请求结果和
Token 摘要。Chat Completions 结果会区分 `success`、`upstream_failure` 和
`client_canceled`。不会有意记录响应 Body 和明文 Secret。

## Grafana 仪表盘

`observability/` 下的可选组件会配置 Grafana 和 Infinity Data Source Plugin：

```bash
export CODEX_OAUTH_PROXY_ADMIN_API_KEY="admin-change-me"
docker compose \
  -f docker-compose.yml \
  -f observability/docker-compose.dashboard.yml \
  up -d
```

配置值必须与 `config.yaml` 中的 `admin-api-key` 一致。

在以下地址打开 Grafana：

```text
http://localhost:3000
```

默认凭据：

```text
User: admin
Password: admin
```

可选覆盖：

```bash
export GRAFANA_ADMIN_USER="admin"
export GRAFANA_ADMIN_PASSWORD="change-me"
export GRAFANA_PORT="3000"
```

默认 Data Source 调用：

```text
http://codex-oauth-proxy:8317/v0/management/usage/timeseries
```

Grafana 位于默认 Compose 项目之外时，覆盖代理 Origin：

```bash
export CODEX_OAUTH_PROXY_URL="http://host.docker.internal:8317"
```

## 仪表盘排障

Panel 显示 `No data` 时，先确认使用托管用户 Key 认证的请求已经生成用量桶。

确保 Provisioning 文件可读：

```bash
chmod -R a+rX observability/grafana
```

Grafana 将 Provisioning 状态持久化到 `grafana-storage` Volume。修改仪表盘或
Data Source 文件后，如果 Grafana 仍然显示旧内容，可以重建该 Volume：

```bash
docker compose \
  -f docker-compose.yml \
  -f observability/docker-compose.dashboard.yml \
  down
docker volume rm codex-oauth-proxy_grafana-storage
docker compose \
  -f docker-compose.yml \
  -f observability/docker-compose.dashboard.yml \
  up -d
```

这只会删除 Grafana 状态。代理用户和用量桶仍保留在代理 SQLite 数据库中。
