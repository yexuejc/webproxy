# 更新日志（Changelog）

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/) 精神，版本号参考 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增
- 单元测试套件（`proxy_test.go`），覆盖 URL 编解码、子域名反代解析、SSRF IP 分类、Origin 改写等核心逻辑；`go test ./...` 可跑通。
- 开源工程件：LICENSE（MIT）、根级 SECURITY.md、CONTRIBUTING.md、CHANGELOG.md、Makefile、CI 工作流（`.github/workflows/ci.yml`）。
- `config.example.json`：默认 `ssrf.mode: "blacklist"` 的安全示例配置（取代原 `off` 的 `config.json`）。

### 变更
- `tests/cookietest` 的 `mock.go` / `wsclient.go` 加 `//go:build cookietest` 标签，避免与 `go test ./...` 冲突。
- `.gitignore` 排除调试日志、构建产物、调试散文件与本地 `config.json`。

## [0.1.0] - 2026-10-19

### 新增
- 网页正向代理核心：子域名反代 + `/p/` 整 URL 反代双入口。
- HTML DOM 重写引擎 + CSS `url()` 重写 + JS 代理桩（fetch / XHR / WebSocket / window.open / 表单导流）。
- 服务端按 `sid` 隔离的会话 Cookie Jar，解决跨站 iframe 下 `SameSite=Lax` Cookie 被扣发导致的登录死循环。
- SSRF 三档防护（off / blacklist / whitelist）+ DNS 重绑定（TOCTOU）防护。
- Docker 多阶段构建 + compose + 离线构建（vendor）支持。
