# 服务器进程

[English](SPEC.md) | [简体中文](SPEC.zh-CN.md)

`cmd/server` 负责进程启动、关闭和管理 CLI。代理行为属于 `codex-proxy`。

## 职责

- 解析 `serve` 和 `admin` 命令。省略命令时运行服务器。
- 加载配置，构造代理 handler，并以 10 秒请求头超时同时监听 Codex 和 Grok 地址；不设置会切断请求体的读写超时。
- 在中断、`SIGTERM`、handler context 取消或致命存储错误时停止。HTTP 优雅关闭给 10 秒，超时后强制关闭剩余连接。
- 调用运行中服务器的回环管理 API。CLI 不读取服务器配置，也不发送 `admin-api-key`。

## 边界

允许依赖标准库、CLI 解析器和 `codex-proxy`。本模块不得重新实现路由、凭证选择、SQLite 访问或上游协议转换。

`--local-model` 会被接受并忽略。运行时模型同步保持启用。
