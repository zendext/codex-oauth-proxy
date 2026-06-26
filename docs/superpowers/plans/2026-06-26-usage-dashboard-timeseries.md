# Usage Dashboard Timeseries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only management API and optional Grafana Compose assets for user-level token usage over time.

**Architecture:** The proxy remains the source of truth for usage data. The new management endpoint reads existing 10-minute SQLite usage buckets, optionally aggregates them into coarser steps, and returns JSON that Grafana can consume through an API data source. Dashboard deployment files live under `observability/` and are not part of the default `docker-compose.yml`.

**Tech Stack:** Go, SQLite, existing `net/http` routing, Docker Compose, Grafana provisioning.

---

### Task 1: Timeseries Store Tests

**Files:**
- Modify: `internal/codexonly/usage_test.go`
- Modify: `internal/codexonly/usage.go`

- [ ] **Step 1: Write a failing store test**

Add a test that records usage in two 10-minute buckets for two users, calls `GetUsageTimeseries` with `Window: "5h"`, `Step: "10m"`, and `GroupBy: []string{"user"}`, then expects one row per user per bucket with token totals aggregated by user.

- [ ] **Step 2: Run the targeted test**

Run: `go test -run TestGetUsageTimeseries -count=1 ./internal/codexonly`

Expected: fail because `GetUsageTimeseries` and related types do not exist.

- [ ] **Step 3: Implement minimal store support**

Add request/response structs, validate `window`, validate `step`, validate `group_by`, compute UTC start/end bounds, and aggregate `usage_buckets` by the requested time step and dimensions.

- [ ] **Step 4: Re-run the targeted test**

Run: `go test -run TestGetUsageTimeseries -count=1 ./internal/codexonly`

Expected: pass.

### Task 2: Management API Tests

**Files:**
- Modify: `internal/codexonly/server_test.go`
- Modify: `internal/codexonly/server.go`

- [ ] **Step 1: Write a failing endpoint test**

Add a test for `GET /v0/management/usage/timeseries?window=5h&step=10m&group_by=user` that authenticates with the admin API key and checks the JSON envelope contains `window`, `step`, `group_by`, and non-empty `series`.

- [ ] **Step 2: Run the targeted test**

Run: `go test -run TestManagementUsageTimeseries -count=1 ./internal/codexonly`

Expected: fail with `404` because the route is not wired yet.

- [ ] **Step 3: Wire route and query parsing**

Add a `/usage/timeseries` management route before `/usage`, parse `window`, `step`, `group_by`, `user_id`, and `api_key_id`, call `GetUsageTimeseries`, and return the result as JSON.

- [ ] **Step 4: Re-run the targeted test**

Run: `go test -run TestManagementUsageTimeseries -count=1 ./internal/codexonly`

Expected: pass.

### Task 3: Dashboard Compose Assets

**Files:**
- Create: `observability/docker-compose.dashboard.yml`
- Create: `observability/grafana/provisioning/datasources/proxy-json.yml`
- Create: `observability/grafana/provisioning/dashboards/codex-oauth-proxy.yml`
- Create: `observability/grafana/dashboards/usage-timeseries.json`
- Create: `observability/README.md`

- [ ] **Step 1: Add optional Grafana service**

Create a Compose override that runs Grafana on `${GRAFANA_PORT:-3000}` and provisions local dashboards. Keep it separate from the default proxy service.

- [ ] **Step 2: Add dashboard provisioning**

Configure Grafana to load dashboard JSON from `/var/lib/grafana/dashboards`.

- [ ] **Step 3: Add API data source placeholder**

Provide a JSON/API data source provisioning file that points at `${CODEX_OAUTH_PROXY_URL:-http://codex-oauth-proxy:8317}` and documents the required admin key environment value.

- [ ] **Step 4: Add README usage**

Document the command `docker compose -f docker-compose.yml -f observability/docker-compose.dashboard.yml up -d` and required environment variables.

### Task 4: Verification

**Files:**
- Modify: Go files touched by earlier tasks

- [ ] **Step 1: Format Go code**

Run: `gofmt -w .`

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: pass.

- [ ] **Step 3: Verify compile**

Run: `go build -o test-output ./cmd/server && rm test-output`

Expected: pass.
