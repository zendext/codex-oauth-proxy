# Codex OAuth Proxy Dashboard

This optional bundle runs Grafana with a pre-provisioned dashboard for user-level
token usage over time. It reads the proxy management API directly through the
Grafana Infinity data source plugin.

## Start

Set the same admin API key that is configured in `config.yaml`:

```bash
export CODEX_OAUTH_PROXY_ADMIN_API_KEY="admin-change-me"
docker compose -f docker-compose.yml -f observability/docker-compose.dashboard.yml up -d
```

Open Grafana at `http://localhost:3000`.

Default credentials:

- User: `admin`
- Password: `admin`

Override them with:

```bash
export GRAFANA_ADMIN_USER="admin"
export GRAFANA_ADMIN_PASSWORD="change-me"
export GRAFANA_PORT="3000"
```

## Data Source

The dashboard calls:

```text
GET /v0/management/usage/timeseries
```

The default Compose setup uses `http://codex-oauth-proxy:8317`. Override it when
Grafana should call a proxy outside this Compose project:

```bash
export CODEX_OAUTH_PROXY_URL="http://host.docker.internal:8317"
```

## Granularity

The API reads the proxy's existing 10-minute usage buckets and can aggregate
them for dashboard views:

- `window=5h&step=10m`
- `window=24h&step=1h`
- `window=7d&step=6h`
- `window=today&step=1h`

Supported `group_by` values are `user`, `api_key`, `model`, `service_tier`, and
`reasoning_effort`. Multiple values can be comma-separated.
