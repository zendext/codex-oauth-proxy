# Usage and Observability

[English](usage-and-observability.md) |
[简体中文](zh-CN/usage-and-observability.md)

The proxy records local usage statistics for requests authenticated with managed
user API keys. SQLite is the source of truth; the optional Grafana bundle reads
the management API and does not maintain a second usage database.

## Attribution

A usage record is created only when proxy authentication resolves a stored
managed user and API key.

Recorded identity includes:

- User ID.
- API key ID and masked metadata.
- Selected OAuth auth file ID.
- Request ID.

Requests accepted only through a currently loaded Codex OAuth access token on
internal compatibility routes are not attributed to a managed user and are not
included in usage totals.

## Counters

Usage records can include:

- Request count.
- Failed request count.
- Input tokens.
- Output tokens.
- Reasoning tokens.
- Cached input tokens.
- Cache read tokens.
- Cache creation tokens.
- Total tokens.

A request counts as failed when the final status is `400` or higher, or when no
final upstream status is available.

Token counters are extracted from JSON, SSE, and final WebSocket response
events. The proxy records available counters; an upstream response without usage
metadata can still contribute request and failure counts.

## Dimensions

Usage is separated by:

- User.
- API key.
- Model.
- Reasoning effort.
- Service tier.
- Selected OAuth auth file.

Blank model, reasoning, or auth values are normalized to `unknown`.

Service tiers are normalized:

| Wire value | Stored value |
| --- | --- |
| empty or unrecognized | `standard` |
| `fast` | `fast` |
| `priority` | `fast` |

## Buckets and Retention

Records are aggregated into UTC 10-minute buckets. A bucket key includes the
identity and model dimensions, so requests in the same period but with different
models, reasoning effort, service tier, or OAuth files remain separable.

Bucket data is retained for 30 days. Old buckets are pruned while new usage is
recorded.

The built-in rolling windows are:

- `5h`: 30 ten-minute buckets.
- `7d`: 1,008 ten-minute buckets.

Today's user usage starts at `00:00:00` UTC.

## User Usage

Managed users can query:

```text
GET /v0/user/usage/today
```

The response includes total counters and model breakdowns for the authenticated
user and current API key.

## Management Snapshot

Administrators can query:

```text
GET /v0/management/usage
```

Optional filters:

```text
user_id=usr_xxx
api_key_id=key_xxx
```

Each result contains 5-hour and 7-day totals plus model, reasoning, and service
tier dimensions.

The equivalent local CLI command is:

```bash
codex-oauth-proxy admin usage snapshot
```

## Timeseries

Administrators can query:

```text
GET /v0/management/usage/timeseries
```

Supported windows:

- `5h`
- `24h`
- `7d`
- `30d`
- `today`

Supported steps:

- `10m`
- `30m`
- `1h`
- `6h`
- `1d`

When `step` is empty or `auto`, the defaults are:

| Window | Automatic step |
| --- | --- |
| `5h` | `10m` |
| `7d` | `6h` |
| `30d` | `1d` |
| `24h`, `today` | `1h` |

Supported grouping dimensions:

- `user`
- `api_key`
- `model`
- `reasoning_effort`
- `service_tier`

Pass multiple `group_by` query values or a comma-separated value. The default is
`user`.

Use `fill=zero` to produce explicit zero-valued points for missing
group-and-time combinations. Without it, only stored buckets are returned.

CLI example:

```bash
codex-oauth-proxy admin usage timeseries \
  --window 7d \
  --step 1h \
  --group-by user,model \
  --fill zero
```

## Disable Tracking

```yaml
usage:
  enabled: false
  debug-openai-response: false
```

Disabling tracking stops new records. Existing SQLite buckets remain until they
are pruned by later enabled writes or removed by the operator.

## Debug Diagnostics

Normal request diagnostics:

```yaml
debug: true
```

Additional usage metadata:

```yaml
debug: true
usage:
  enabled: true
  debug-openai-response: true
```

Usage diagnostics include safe request IDs, model dimensions, masked key
metadata, status, and token summaries. Response bodies and plaintext secrets are
not intentionally logged.

## Grafana Dashboard

The optional bundle under `observability/` provisions Grafana and the Infinity
data source plugin:

```bash
export CODEX_OAUTH_PROXY_ADMIN_API_KEY="admin-change-me"
docker compose \
  -f docker-compose.yml \
  -f observability/docker-compose.dashboard.yml \
  up -d
```

The configured value must match `admin-api-key` in `config.yaml`.

Open Grafana at:

```text
http://localhost:3000
```

Default credentials:

```text
User: admin
Password: admin
```

Optional overrides:

```bash
export GRAFANA_ADMIN_USER="admin"
export GRAFANA_ADMIN_PASSWORD="change-me"
export GRAFANA_PORT="3000"
```

The default data source calls:

```text
http://codex-oauth-proxy:8317/v0/management/usage/timeseries
```

Override the proxy origin when Grafana is outside the default Compose project:

```bash
export CODEX_OAUTH_PROXY_URL="http://host.docker.internal:8317"
```

## Dashboard Troubleshooting

If panels show `No data`, first confirm that requests authenticated with managed
user keys have produced usage buckets.

Ensure provisioning files are readable:

```bash
chmod -R a+rX observability/grafana
```

Grafana persists provisioned state in the `grafana-storage` volume. After
changing dashboard or data source files, recreate that volume if Grafana still
shows stale content:

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

This removes Grafana state only. Proxy users and usage buckets remain in the
proxy SQLite database.
