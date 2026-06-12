# User API Key Management API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build API-only user and API key management with SQLite-backed users, admin APIs, user APIs, and proxy authentication through stored user API keys.

**Architecture:** Add a small SQLite store under `internal/codexonly`, wire it into `Server`, and keep existing static `api-keys` behavior as a proxy-only path. HTTP handlers remain in the codex-only server and expose `/v0/management/users...` and `/v0/user/api-key...`.

**Tech Stack:** Go `database/sql`, pure-Go SQLite driver `modernc.org/sqlite`, existing `net/http` handler tests, `gofmt`, `go test ./...`.

---

### Task 1: Config And SQLite Store

**Files:**
- Modify: `go.mod`
- Modify: `internal/codexonly/config.go`
- Create: `internal/codexonly/user_store.go`
- Test: `internal/codexonly/config_auth_test.go`
- Test: `internal/codexonly/user_store_test.go`

- [ ] **Step 1: Write failing config and store tests**

Add tests for YAML fields, default database path resolution, user creation, duplicate names, key reset, and API key authentication.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/codexonly -run 'TestLoadConfigAppliesCodexOnlyDefaults|TestUserStore' -v`

Expected: fails because `AdminAPIKey`, `Database`, and user store symbols do not exist.

- [ ] **Step 3: Implement config and SQLite store**

Add `admin-api-key`, `database.path`, SQLite migrations for `users` and `api_keys`, key generation, hash/masking helpers, and store methods for create/list/get/update/reset/authenticate.

- [ ] **Step 4: Run focused tests**

Run: `go test ./internal/codexonly -run 'TestLoadConfigAppliesCodexOnlyDefaults|TestUserStore' -v`

Expected: passes.

### Task 2: Management And User APIs

**Files:**
- Modify: `internal/codexonly/server.go`
- Test: `internal/codexonly/server_test.go`

- [ ] **Step 1: Write failing HTTP API tests**

Add tests for disabled management APIs, admin-authenticated user creation/list/detail/update/reset, user self key lookup/reset, and rejection of static proxy keys for user APIs.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/codexonly -run 'TestManagement|TestUserAPI' -v`

Expected: fails with 404 or unauthorized because the new routes do not exist.

- [ ] **Step 3: Implement HTTP handlers**

Wire the SQLite store into `NewHandler`, add admin and user auth helpers, route `/v0/management/users`, `/v0/management/users/{id}`, `/v0/management/users/{id}/api-key/reset`, `/v0/user/api-key`, and `/v0/user/api-key/reset`.

- [ ] **Step 4: Run focused HTTP tests**

Run: `go test ./internal/codexonly -run 'TestManagement|TestUserAPI' -v`

Expected: passes.

### Task 3: Proxy Authentication Integration

**Files:**
- Modify: `internal/codexonly/server.go`
- Test: `internal/codexonly/server_test.go`

- [ ] **Step 1: Write failing proxy auth tests**

Add tests showing stored user API keys authenticate proxy routes, disabled users are rejected, re-enabled users work again, static `api-keys` still work, and `api-keys: []` local mode still works.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/codexonly -run 'TestProxy' -v`

Expected: stored user API key cases fail before proxy auth consults the SQLite store.

- [ ] **Step 3: Integrate stored key auth into proxy routes**

Change proxy authorization to check static keys, stored user API keys, local empty-key mode, and upstream Codex access tokens in the existing allowed backend cases.

- [ ] **Step 4: Run focused proxy tests**

Run: `go test ./internal/codexonly -run 'TestProxy' -v`

Expected: passes.

### Task 4: Docs And Final Verification

**Files:**
- Modify: `config.example.yaml`
- Modify: `README.md`

- [ ] **Step 1: Update docs**

Document `admin-api-key`, `database.path`, management APIs, user APIs, and the stored user API key proxy path.

- [ ] **Step 2: Format and test**

Run: `gofmt -w .`

Run: `go test ./...`

Run: `go build -o test-output ./cmd/server && rm test-output`

Expected: all commands exit 0.
