# Service Tier Usage Dimension Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Track Codex Fast mode usage separately from standard usage by adding a `service_tier` dimension.

**Architecture:** Capture the wire `service_tier` value from HTTP and WebSocket requests, normalize it to user-facing values (`standard` or `fast`), and persist it as a usage dimension. Extend usage tables and migrations so existing rows become `standard`, while new Fast requests aggregate under `fast`.

**Tech Stack:** Go `net/http`, Gorilla WebSocket, SQLite through `database/sql`, existing `go test ./...` suite.

---

### Task 1: Add Failing Coverage

**Files:**
- Modify: `internal/codexonly/usage_test.go`
- Modify: `internal/codexonly/server_test.go`

- [x] Add tests showing `service_tier: "priority"` is rejected when Fast mode is disabled.
- [x] Add tests showing Fast requests record usage under `service_tier: "fast"`.
- [x] Add tests showing missing service tier records as `service_tier: "standard"`.
- [x] Add migration tests proving old usage rows gain `service_tier = "standard"`.

### Task 2: Implement Metadata Capture

**Files:**
- Modify: `internal/codexonly/usage_proxy.go`
- Modify: `internal/codexonly/server.go`
- Modify: `internal/codexonly/chat_completions.go`

- [x] Add `ServiceTier` to request usage metadata.
- [x] Normalize wire values so `priority` and `fast` become `fast`, and blank values become `standard`.
- [x] Use the same Fast detection in the disabled-Fast request guard.

### Task 3: Extend Usage Storage

**Files:**
- Modify: `internal/codexonly/usage.go`
- Modify: `internal/codexonly/user_store.go`

- [x] Add `service_tier` to usage records, buckets, dimensions, threshold state, and threshold events.
- [x] Update primary keys and grouping queries to include `service_tier`.
- [x] Add schema migrations that preserve existing totals as `standard`.

### Task 4: Verify

**Files:**
- Modify: `README.md` if user-facing usage output changes need documentation.

- [x] Run `gofmt -w` on changed Go files.
- [x] Run focused tests for service tier behavior.
- [x] Run `go test ./...`.
- [x] Run `go build -o test-output ./cmd/server && rm test-output`.
