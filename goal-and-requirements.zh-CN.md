# Codex OAuth 代理

[English](goal-and-requirements.md) | [简体中文](goal-and-requirements.zh-CN.md)

## 目标

让本地操作者把已有的 Codex CLI OAuth 凭证，通过托管 API key 提供给 Codex CLI
和支持的客户端使用，而不把这些 OAuth 凭证交给客户端。

## 问题

Codex CLI 把可用的 OAuth 凭证存在磁盘上，但需要使用这些凭证的客户端仍然缺少一个受控的本地入口。直接分享 OAuth 文件会暴露可刷新的凭证，也没有按用户划分的 key、故障转移和用量边界。

## 受众

这个产品给已经拥有 Codex CLI 凭证、想为自己的 Codex 兼容客户端运行一个本地代理的操作者使用。它不是托管的多租户服务。（未确认：预期操作者是否始终是一个人，还是也可能管理一个小型共享的本地部署。）

## 能力

- 代理白名单内的 Codex 请求和选定的 ChatGPT 兼容请求。
- 通过第二个本地入口，使用本机已有的 Grok CLI 登录代理 Grok Build 模型请求。
- 用托管的 `cop_...` API key 认证调用方。
- 刷新、选择并在 Codex OAuth 凭证之间做故障转移。
- 让会话保持在一个健康凭证上，同时不存储原始会话标识。
- 在本地把 Chat Completions 和 Zed Edit Prediction 请求转换成代理使用的 Codex Responses 流程。
- 记录托管用户的用量，并通过管理 API 和可选的 Grafana 套件提供这些数据。
- 默认提供回环管理；只有显式启用时才提供远程管理。

## 非目标

- 通用提供商转换、管理界面、SDK 嵌入、插件宿主和可替换存储后端被排除。这些是产品边界，不是尚未完成的工作。
- 代理不实现 Codex 或 Grok OAuth 登录流程。
- 进程不终止 TLS。监听地址选择和传输终止由部署环境负责。
- 发布制品只有 Linux amd64。

## 原则

- 实现保持 Codex 专用，并且小到可以作为一个 Go 进程运行。
- 把托管 API key、可选的管理密钥和 Codex OAuth token 当作彼此独立的凭证。
- 所需的 SQLite 状态变得不可靠后，不得继续代理。
- 不得用服务器读写超时切断已经建立的流或 WebSocket 连接。
