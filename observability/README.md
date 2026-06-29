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

## Troubleshooting

If the dashboard loads but panels show `No data`, first verify that the mounted
dashboard and provisioning files are readable by the Grafana container:

```bash
chmod -R a+rX observability/grafana
```

Grafana persists provisioned data in the `grafana-storage` volume. If you change
dashboard or data source provisioning after the first start and the UI still
shows stale panels, recreate the Grafana volume:

```bash
docker compose -f docker-compose.yml -f observability/docker-compose.dashboard.yml down
docker volume rm codex-oauth-proxy_grafana-storage
docker compose -f docker-compose.yml -f observability/docker-compose.dashboard.yml up -d
```

This only resets Grafana state. Proxy users and usage buckets live in the proxy
database, not in the Grafana volume.

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
- `window=30d&step=1d`
- `window=today&step=1h`

Supported `group_by` values are `user`, `api_key`, `model`, `service_tier`, and
`reasoning_effort`. Multiple values can be comma-separated.
