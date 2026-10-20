# tests — 回归验证脚本

配套本地目标站（python -m http.server）使用，验证 HTML 重写与 JS 桩注入：

- `check_js.js <out.html>`：提取输出 HTML 中所有 `<script>` 块做语法编译检查；断言桩被 `<script>` 包裹、位于 `</head>` 前、页面原脚本完好、body 顶部无裸 JS 文本。
- `check_stub_run.js <out.html>`：提取 JS 桩，在最小 DOM 存根环境执行，验证 fetch/XHR/window.open/表单提交均被导流到 `/p/` 前缀。
- `verify_browser.js`：CDP 连 headless Chrome，验证 UI 主页 JS 零异常、`go()` 生成的代理地址与后端一致。
- `verify_newtab.js`：CDP 连 headless Chrome，验证新标签顶层打开 `/p/xxx` 会 302 到 `/?url=` 外框、输入框回填、iframe 内容正常加载（需先起 headless Chrome 调试端口 9222）。

背景：曾出现页面内联脚本含 `</head>` 字面量时，字节级注入把桩插进页面脚本内部，导致 `Unexpected token ')'` 报错；已改为 DOM 注入（`<head>` 末尾追加 script 节点），这两个脚本用于防回归。
- `verify_browser.js`：用 CDP 连本地 headless Chrome 做**真实浏览器**验证——打开 `http://127.0.0.1:8765/`、监听 `Runtime.exceptionThrown`、模拟 `go()` 验证生成的 iframe 代理地址；用于确认 UI 页面无 JS 报错。需本机已装 Chrome/Edge 且 webproxy 在 8765 运行。

## cookietest — 端到端会话回归

`tests/cookietest/` 下两个独立程序（带 `//go:build cookietest` 标签，**不参与** `go test ./...`；分别位于各自目录，避免同包双 `main` 冲突）：

- `mock.go`：模拟 fnOS，下发 host-only + `SameSite=Lax` 的会话 Cookie，提供 `/login`、`/app/token`、`/websocket` 接口。
- `wsclient/main.go`：原生 TCP WebSocket 客户端，验证带/不带 Cookie 时的会话保持。

构建与运行（需带 tag）：

```bash
go run -tags cookietest ./tests/cookietest
go run -tags cookietest ./tests/cookietest/wsclient <host:port> <cookie值>
```

用于回归验证"跨站 iframe 下 `SameSite=Lax` Cookie 被浏览器扣发时，服务端 Cookie Jar 仍能保持登录态"的修复。
