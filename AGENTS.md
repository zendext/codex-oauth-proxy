# AGENTS.md

Go 1.26+ local proxy for Codex CLI traffic using Codex OAuth credentials.

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o codex-oauth-proxy ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--local-model` (compatibility no-op)

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- Auth material defaults to `~/.codex`
- Docker Compose defaults to mounting `./auths` as `/root/.codex`
- No management UI, SDK embedding, provider translation, plugin host, or alternate storage backends

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/codexonly/` — Config loading, Codex auth file parsing/refresh, route whitelist, reverse proxy, and tests
- `internal/codexonly/codex_client_models.json` — Embedded Codex CLI model catalog returned from `/v1/models?client_version=...`

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Avoid leaking secrets/tokens in logs or test output
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed during credential refresh and server shutdown. Do not add read/write timeouts that would cut off established upstream streaming or WebSocket traffic.
