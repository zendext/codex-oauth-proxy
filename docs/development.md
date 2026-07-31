# Development

[English](development.md) | [简体中文](zh-CN/development.md)

## Requirements

- Go 1.26 or later.
- Git.
- Docker only for image or Compose verification.

## Build and Run

```bash
go build -o codex-oauth-proxy ./cmd/server
codex-oauth-proxy --config config.yaml
```

The server also accepts:

```bash
codex-oauth-proxy serve --config config.yaml
```

`--local-model` remains accepted as a compatibility no-op.

## Test

Run the complete suite:

```bash
go test ./...
```

Run one test:

```bash
go test -v -run TestName ./path/to/pkg
```

Required compile verification:

```bash
go build -o test-output ./cmd/server && rm test-output
```

Format Go changes before verification:

```bash
gofmt -w .
```

## Project Boundaries

Keep changes aligned with the current Codex-specific scope:

- No management UI.
- No SDK embedding layer.
- No generic provider translation.
- No plugin host.
- No alternate storage backend.
- No catch-all proxy routing.

Timeouts are appropriate for credential refresh and shutdown. Do not add read or
write timeouts that would cut off established upstream streams or WebSocket
connections.

## Code Map

| Area | Files |
| --- | --- |
| Entrypoint and lifecycle | `cmd/server/main.go`, `cmd/server/cli.go` |
| Admin CLI | `cmd/server/admin_*.go` |
| Configuration and OAuth | `internal/codexonly/config.go`, `auth.go`, `refresh.go` |
| Routing and proxy | `internal/codexonly/server.go`, `usage_proxy.go` |
| Runtime model catalogs | `internal/codexonly/models.go` |
| Chat compatibility | `internal/codexonly/chat_completions.go` |
| Users and SQLite | `internal/codexonly/user_store.go` |
| Usage queries | `internal/codexonly/usage.go` |
| Embedded model catalog | `internal/codexonly/codex_client_models.json` |
| Grafana assets | `observability/` |

Tests are kept next to their owning package. HTTP tests use `httptest`; store
tests use temporary SQLite databases.

## Embedded Catalog Maintenance

The embedded catalog is a final runtime fallback. Its `go:generate` directive
pins an explicit official `openai/codex` Git ref and never discovers the latest
release at runtime:

```bash
go generate ./internal/codexonly
go run ./cmd/update-model-catalog --ref rust-v0.146.0 --check
```

When updating it, first verify the latest stable Codex CLI release from the
official repository. Then update the pinned `--ref`, `DefaultCodexUA`, and the
vendored JSON together. The updater validates the downloaded catalog and writes
it atomically.

## Documentation Policy

English is the authoritative project documentation language.

Current-state documentation must remain synchronized:

| English | Chinese |
| --- | --- |
| `README.md` | `README.zh-CN.md` |
| `docs/<name>.md` | `docs/zh-CN/<name>.md` |

When changing user-visible behavior, configuration, commands, APIs,
architecture, or operations:

1. Verify the behavior against code and tests.
2. Update the English source.
3. Update the matching Simplified Chinese document in the same change.
4. Keep headings, links, examples, field names, and behavior equivalent.

`AGENTS.md`, `LICENSE`, workflow files, configuration comments, and source code
comments are not part of the bilingual mirror.

## Release

Pushing a tag beginning with `v` runs `.github/workflows/release.yml`:

```bash
git tag v0.8.0
git push origin v0.8.0
```

The workflow:

- Runs `go test -count=1 ./...`.
- Builds a Linux `amd64` binary.
- Publishes the binary and SHA-256 file to GitHub Releases.
- Builds and pushes the Linux `amd64` Docker image.
- Publishes the release tag, semantic version aliases, and `latest`.

Required repository secrets:

- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

The published image namespace is:

```text
zendext/codex-oauth-proxy
```
