# 开发

[English](../development.md) | [简体中文](development.md)

## 要求

- Go 1.26 或更高版本。
- Git。
- 只有验证镜像或 Compose 时才需要 Docker。

## 构建与运行

```bash
go build -o codex-oauth-proxy ./cmd/server
codex-oauth-proxy --config config.yaml
```

服务器也接受：

```bash
codex-oauth-proxy serve --config config.yaml
```

`--local-model` 仍作为兼容性 No-op 接受。

## 测试

运行完整测试：

```bash
go test ./...
```

运行单个测试：

```bash
go test -v -run TestName ./path/to/pkg
```

必须执行的编译验证：

```bash
go build -o test-output ./cmd/server && rm test-output
```

验证前格式化 Go 变更：

```bash
gofmt -w .
```

## 项目边界

变更应保持在当前 Codex 专用范围内：

- 不提供管理 UI。
- 不提供 SDK 嵌入层。
- 不提供通用 Provider 转换。
- 不提供 Plugin Host。
- 不提供替代存储后端。
- 不提供 Catch-all 代理路由。

凭据刷新和关闭流程可以使用超时。不得添加会中断已建立上游 Stream 或
WebSocket 连接的 Read/Write Timeout。

## 代码导航

| 范围 | 文件 |
| --- | --- |
| 入口与生命周期 | `cmd/server/main.go`、`cmd/server/cli.go` |
| 管理 CLI | `cmd/server/admin_*.go` |
| 配置与 OAuth | `internal/codexonly/config.go`、`auth.go`、`refresh.go` |
| 路由与代理 | `internal/codexonly/server.go`、`usage_proxy.go` |
| 运行时模型目录 | `internal/codexonly/models.go` |
| Chat 兼容 | `internal/codexonly/chat_completions.go` |
| 用户与 SQLite | `internal/codexonly/user_store.go` |
| 用量查询 | `internal/codexonly/usage.go` |
| 嵌入式模型目录 | `internal/codexonly/codex_client_models.json` |
| Grafana 资源 | `observability/` |

测试与所属 Package 放在一起。HTTP 测试使用 `httptest`，Store 测试使用临时
SQLite 数据库。

## 嵌入式目录维护

嵌入式目录是最终运行时回退。其 `go:generate` 指令固定到显式的官方
`openai/codex` Git Ref，不会在运行时发现最新版本：

```bash
go generate ./internal/codexonly
go run ./cmd/update-model-catalog --ref rust-v0.146.0 --check
```

更新前必须先从官方仓库确认最新稳定 Codex CLI Release，然后同时更新固定的
`--ref`、`DefaultCodexUA` 和 vendored JSON。Updater 会验证下载的目录并以
原子方式写入。

## 文档策略

英文是项目文档的权威语言。

当前状态文档必须保持同步：

| 英文 | 中文 |
| --- | --- |
| `README.md` | `README.zh-CN.md` |
| `docs/<name>.md` | `docs/zh-CN/<name>.md` |

修改用户可见行为、配置、命令、API、架构或运维方式时：

1. 对照代码和测试验证行为。
2. 更新英文来源。
3. 在同一次变更中更新对应的简体中文文档。
4. 保持标题、链接、示例、字段名和行为等价。

`AGENTS.md`、`LICENSE`、工作流文件、配置注释和源代码注释不属于双语镜像。

## 发布

推送以 `v` 开头的 Tag 会运行 `.github/workflows/release.yml`：

```bash
git tag v0.8.0
git push origin v0.8.0
```

工作流会：

- 运行 `go test -count=1 ./...`。
- 构建 Linux `amd64` 二进制。
- 将二进制和 SHA-256 文件发布到 GitHub Releases。
- 构建并推送 Linux `amd64` Docker 镜像。
- 发布 Release Tag、语义版本别名和 `latest`。

需要的仓库 Secret：

- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

发布镜像命名空间：

```text
zendext/codex-oauth-proxy
```
