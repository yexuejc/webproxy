# 贡献指南（Contributing）

感谢你考虑为 WebProxy 做贡献！本文说明如何本地开发、提交规范与代码要求。

## 开发环境

- Go 1.22+（CI 使用 `go-version-file: go.mod`）
- 无需额外依赖（仅 `golang.org/x/net`、`golang.org/x/text`，已 vendor）

```bash
git clone https://github.com/yexuejc/webproxy.git
cd webproxy
go run .                      # 默认监听 :8765
```

## 本地校验（提交前请全部跑通）

```bash
make fmt                     # gofmt -w .
make vet                     # go vet ./...
make test                    # go test ./...
make build                   # go build -o webproxy .
```

也可直接用 `go` 命令等价执行。CI 会自动跑同一套（`gofmt` / `go vet` / `go build` / `go test`）。

> `tests/cookietest` 下的 `mock.go` / `wsclient.go` 是用于端到端回归的**独立小程序**，带 `//go:build cookietest` 标签，不会参与 `go test ./...`。
> 需要手动构建时：
> ```bash
> go run -tags cookietest ./tests/cookietest/mock.go      # 启动 mock fnOS
> go run -tags cookietest ./tests/cookietest/wsclient.go  # 原生 TCP WS 客户端探针
> ```

## 提交规范

- 单一职责：一个 PR 解决一个问题/一类改动。
- 提交信息建议遵循 `类型: 简述`，例如 `fix: 修复跨站 iframe 下 SameSite Cookie 被扣发`、`feat: 新增 whitelist 模式`。
- 保持 `gofmt` 格式、`go vet` 无告警。

##  issue / PR 模板

- Bug：请附复现步骤、目标站点类型（内网/公网）、代理日志（`WEBPROXY_DEBUG=1`）。
- 新功能：先开 Issue 讨论设计，避免重复劳动。

## 许可

提交贡献即表示你同意以本仓库 [LICENSE](LICENSE)（MIT）条款授权你的贡献。
