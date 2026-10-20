# WebProxy — 网页正向代理浏览器

[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![CI](https://github.com/yexuejc/webproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/yexuejc/webproxy/actions/workflows/ci.yml)

> 本地 `config.json` 已被 `.gitignore` 忽略，请用 `config.example.json` 起手。

部署在服务器上的 **Web 正向代理**：打开网页，输入任意 URL，由服务器代为请求目标站点，经过 HTML 重写 + 静态资源代理 + 重定向/Cookie/表单处理后，把"和原站一样可用"的页面回填给浏览器。核心场景是**通过服务器访问其所在内网的客户网站**（类型未知、需尽量全兼容）。

- 后端：Go 单二进制（`go 1.22`，仅依赖 `golang.org/x/net`），前后端一体
- 部署：本地 `go run` / 单二进制，或 Docker 一键部署（多阶段构建，alpine 极简镜像）
- 安全：SSRF 防护**可配置**（off / blacklist / whitelist 三档），默认挡元数据/回环/非常规协议，默认放行 RFC1918 内网

---

## 1. 功能特性

| 能力 | 说明 |
|------|------|
| 全方法转发 | GET/POST/PUT/DELETE/HEAD 等，透传 Header、Cookie、Body（表单/登录正常） |
| HTML 重写引擎 | `golang.org/x/net/html` 解析 DOM，遍历改写 `href/src/action/formaction/poster/cite/data/longdesc/xlink:href` 等 11 类 URL 属性；不误伤 `#`/`data:`/`blob:`/`javascript:`/`mailto:` 等 |
| CSS 重写 | CSS 文件、`<style>` 内联、`style` 属性中的 `url(...)` 全部改写 |
| 静态资源代理 | CSS/JS/图片/字体/音视频按原 MIME **流式**转发（不全量缓存） |
| 重定向处理 | 3xx 自动把 `Location` 改写回 `/p/` 前缀（浏览器在代理内继续跳转） |
| 新标签保持外框 | `target="_blank"` 链接/`window.open` 在新标签打开时，顶层导航自动 302 到 `/?url=` 外框页，输入框不丢（iframe 内加载不受影响） |
| 会话保持 | 按 `sid` Cookie 隔离每个浏览者的 Cookie Jar，支撑登录态、多源不串号 |
| JS 动态请求导流 | 注入 JS 代理桩，重写 `fetch` / `XMLHttpRequest` / `WebSocket` / `window.open` / 动态表单 `action`，覆盖 AJAX/动态加载 |
| 编码自适应 | 上游按 `charset` 自动识别（GBK 等回落到 `x/text` 转码），强制 `Accept-Encoding: identity` 后本地解压，重写不受压缩影响 |
| SSRF 防护 | DNS 解析 → 校验目的 IP → **pin IP 直连**（防 DNS 重绑定 TOCTOU）；协议白名单；三档模式可配置 |
| 上游证书 | 默认校验目标 HTTPS 证书；内网自签证书可配置 `insecure_skip_verify` 跳过 |

## 2. 架构总览

```
                ┌────────────────────────────────────────────┐
                │                 你的浏览器                  │
                │  ┌──────────────────────────────────────┐  │
                │  │  UI(URL输入框)  +  iframe(承载代理页)  │  │
                │  └──────────────────────────────────────┘  │
                └───────────────────┬────────────────────────┘
                                    │ HTTP
                                    ▼
┌───────────────────────────────────────────────────────────────┐
│                        webproxy 服务器                         │
│                                                               │
│   /            UI 页面（static/index.html）                    │
│   /p/<编码URL> 代理入口（所有页面/资源/表单都走这里）            │
│                                                               │
│   ① base64url 解码目标 URL                                     │
│   ② SSRF 检查：DNS 解析 → IP 分类校验 → pin 解析到的 IP 直连    │
│   ③ 转发：全方法 / Header / Body / 会话 Cookie Jar             │
│   ④ 重写：HTML DOM 遍历改写 URL；注入 JS 代理桩                 │
│   ⑤ 回填：3xx 改写 Location；静态资源按原 MIME 流式输出         │
│                                                               │
│   session: sync.Map(sid → http.CookieJar)                      │
└───────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
                        内网/公网目标站点（http/https）
```

处理链细节见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 3. 目录结构

```
webproxy/
├── main.go            # 入口：加载配置、注册路由（/ 与 /p/）
├── proxy.go           # 转发引擎：方法/Header/Body 透传、重定向改写、资源回填
├── rewriter.go        # 重写引擎：HTML DOM / CSS url() 改写 + JS 代理桩生成
├── ssrf.go            # SSRF 防护：三档模式 + IP 分类 + CIDR 白/黑名单
├── session.go         # 会话隔离：sid Cookie → 独立 Cookie Jar
├── config.json        # 运行配置（监听地址、代理前缀、SSRF 策略、证书校验）
├── static/index.html  # 前端 UI（URL 输入栏 + iframe 展示区）
├── Dockerfile         # 多阶段构建（golang 编译 / alpine 运行）
├── docker-compose.yml # compose 部署（预留配置覆盖 volume）
├── deploy.sh          # Docker 一键部署脚本
├── .dockerignore      # 构建上下文排除项
├── tests/             # 回归验证脚本（JS 桩注入/导流检查）
└── docs/              # 专题文档（配置/部署/安全/接口/架构）
```

## 4. 快速开始

### 4.1 本地运行（先跑通）

```bash
cd webproxy
go run .                    # 或先编译：go build -o webproxy . && ./webproxy
# 浏览器打开 http://<本机IP>:8765/ ，输入目标 URL（默认内网/公网 http/https）
```

> 配置读取当前目录下的 `config.json`；文件不存在则用内置安全默认值（blacklist 档，拦回环/链路本地）。

### 4.2 Docker 一键部署（上服务器/NAS）

```bash
cd webproxy
./deploy.sh                  # 构建镜像并后台启动，对外 8765
PORT=9000 ./deploy.sh        # 换个对外端口
```

或者 compose 方式：

```bash
docker compose up -d --build
```

完成后访问 `http://<服务器IP>:8765/`。日志 `docker logs -f webproxy`；停止 `docker rm -f webproxy`。

> 完整部署说明（含 NAS/Container Manager、配置挂载、升级回退）见 [docs/DEPLOY.md](docs/DEPLOY.md)。

## 5. 配置速查

配置即 `config.json`，完整字段与场景示例见 [docs/CONFIG.md](docs/CONFIG.md)。

| 字段 | 默认 | 说明 |
|------|------|------|
| `listen` | `:8765` | 监听地址；容器内固定 8765，对外端口在 `docker run -p`/compose 映射 |
| `base_path` | `/p/` | 代理前缀，服务端路由与注入 JS 同源生效，改了必须两端一致 |
| `insecure_skip_verify` | `false` | 是否跳过目标站点 HTTPS 证书校验（内网自签证书场景） |
| `ssrf.mode` | `blacklist` | `off` 不检查 / `blacklist` 黑名单 / `whitelist` 白名单 |
| `ssrf.block_loopback` | `true` | 拦 `127.0.0.0/8`、`::1` |
| `ssrf.block_link_local` | `true` | 拦 `169.254.0.0/16`（含云元数据 `169.254.169.254`） |
| `ssrf.block_private` | `false` | 是否拦 RFC1918 内网（**默认 false**：访问内网站点是核心诉求，不能一刀切） |
| `ssrf.block_reserved` | `false` | 是否拦保留段（100.64/10 CGNAT、198.18/15、240/4 等） |
| `ssrf.denied_ips` | `[]` | 黑名单附加 CIDR 列表，如 `["192.168.9.0/24"]` |
| `ssrf.denied_protocols` | file/gopher/ftp/ftps/dict/tftp/ldap/sftp | 黑名单禁用的协议 |
| `ssrf.allowed_ips` | `[]` | 白名单 CIDR 列表 |
| `ssrf.allowed_domains` | `[]` | 白名单域名（精确或子域后缀匹配） |
| `ssrf.allowed_protocols` | `["http","https"]` | 白名单放行的协议 |

**三种上线场景一一对应**：

| 场景 | 配置 |
|------|------|
| 专用服务器/完全可信网络 | `ssrf.mode: "off"` |
| 复杂网络，封部分 IP/协议 | `ssrf.mode: "blacklist"` + 填 `denied_ips/denied_protocols` |
| 只能白名单访问 | `ssrf.mode: "whitelist"` + 填 `allowed_ips/allowed_domains` |

## 6. URL 编码规范（长这样）

代理入口统一为 `/p/<目标URL>`，其中 `<目标URL>` 为 **base64url（URL 安全字符、去 `=` 填充）** 编码，前端 UI 与注入 JS、服务端解码保持一致：

| 目标 URL | 代理地址 |
|----------|----------|
| `http://example.com/` | `/p/aHR0cDovL2V4YW1wbGUuY29tLw` |
| `http://192.168.1.10:8080/` | `/p/aHR0cDovLzE5Mi4xNjguMS4xMDo4MDgwLw` |
| `https://www.baidu.com/s?wd=test` | `/p/aHR0cHM6Ly93d3cuYmFpZHUuY29tL3M_d2Q9dGVzdA` |
| `http://10.0.0.5/login` | `/p/aHR0cDovLzEwLjAuMC41L2xvZ2lu` |

页面内所有相对路径基于当前页 origin 解析成绝对 URL 后同样编码；锚点、`data:`/`blob:`/`javascript:` 等不改写。接口细节见 [docs/API.md](docs/API.md)。

## 7. 错误码

| 状态码 | 场景 |
|--------|------|
| 400 | 未带目标地址 / base64url 解码失败 / 目标 URL 无效 / 请求代理自身 |
| 403 | SSRF 拦截（响应体给出原因：loopback / linklocal / private / reserved / denied / whitelist / 协议） |
| 404 | 访问 UI 以外的根路径 |
| 500 | UI 文件缺失等内部错误 |
| 502 | 上游请求失败（连接拒绝、超时等） |

## 8. 安全须知（上线前必读）

- **入口登录鉴权尚未实现**：公网暴露前必须加（Basic Auth / 反代鉴权 / OAuth 任一），否则等于开放代理，会被滥用且放大 SSRF 敞口。
- **SSRF 防护默认开启**但策略可配：`blacklist` 档已默认拦截回环、链路本地（云元数据）、非常规协议；内网段默认放行（业务需要）。
- **同源 XSS 面**：被代理页面与 UI 同源，恶意站点 JS 理论上可触达父窗口；缓解建议见 [docs/SECURITY.md](docs/SECURITY.md)。
- `sid` Cookie 已设 `HttpOnly`，页面脚本读不到登录会话标识。
- 本地运行配置 `config.json` 已被 `.gitignore` 忽略（避免把你的部署配置/内网信息提交），请复制 `config.example.json` 后按需修改：`cp config.example.json config.json`。

## 9. 已知局限

| 局限 | 影响 | 计划 |
|------|------|------|
| 无入口登录鉴权 | 公网暴露有滥用风险 | 下一版加 |
| 307/308 重定向统一改写为 302 | 极少数"保持方法"的 POST 跳转会退化成 GET | 后续保留原状态码 |
| POST 顶层导航（表单 `target="_blank"`）不改道 | 这类新标签打开时无 UI 外框（为保 POST 语义的取舍） | 后续评估用结果页包装 |
| WebSocket 未真正代理（`ws://` 原样透传） | 依赖 WS 的站点功能不可用 | 后续做服务端 Upgrade 转发 |
| meta-refresh 的 URL 未重写 | 个别老站点跳转失效 | 后续补 |
| HTML/CSS 全量读内存 | 超大页面/频繁请求内存压力 | 后续改流式解析 |
| 会话不设过期、不主动清理 | 长期运行 sessions 缓慢增长 | 后续加 TTL 清理 |
| 反爬/重度 JS 站点可能漏 | 动态内容不全 | 规划 `render=1` 无头浏览器兜底（不默认开启） |
| **重 SPA 绝对路径 `import('/x')` 现支持**（飞牛 fnOS 类） | 通过子域名反代 `<hex(origin)>.localhost:<port>` 解决，浏览器原生把 `*.localhost` 解析到 127.0.0.1，绝对路径 import/fetch 自然落回目标 | 仅 `localhost` 访问可用；经 LAN IP 访问时 SPA 仍走 `/p/` 旧路径，绝对路径 import 可能 404 |
| 部分站点对 User-Agent/Referer 敏感（如 fnOS 按 UA 返回不同 HTML、按 Accept-Encoding 行为不同） | 个别站点渲染异常或资源 404 | 后续支持把 UA/Referer 重写为目标源 |

## 10. 本地验证记录（2026-10-19）

| 用例 | 结果 |
|------|------|
| UI 首页返回 | 200，输入栏+iframe 正常 |
| `/p/` 请求代理自身 | 400 禁止 |
| 环回 `127.0.0.1:9` | 403 loopback |
| 云元数据 `169.254.169.254` | 403 linklocal |
| 协议 `ftp://` | 403 协议被禁止 |
| 端到端（本地目标站） | HTML 属性/`<link>`/内联 style/CSS `url()` 全部改写；JS 桩注入命中；图片按 `image/png` 流出；表单 action 改写；锚点保留 |
| `go vet` | 通过 |
| JS 桩注入边界（页面脚本含 `</head>` 字面量） | 修复前：桩被字节级插入页面脚本内部（被注释吞掉/`Unexpected token ')'` 语法错误），动态请求从未导流；修复后（DOM 注入 `<head>` 末尾 + `<script>` 包裹）：页面脚本完好、桩独立成块，fetch/XHR/window.open/表单提交导流全部生效（tests/ 下两个脚本防回归） |

## 11. 测试

```bash
make test                # 等价 go test ./...
go test ./... -count=1   # 关闭缓存，强制重跑
```

- `proxy_test.go`：核心逻辑单元测试（URL base64url 编解码、子域名反代解析、SSRF IP 分类、Origin 改写、`truncate` 等），不依赖网络，CI 必跑。
- `tests/cookietest/`：端到端回归。`mock.go` 模拟 fnOS（下发 host-only + `SameSite=Lax` 的会话 Cookie），`wsclient.go` 为原生 TCP WebSocket 探针，用于验证"跨站 iframe 下服务端 Cookie Jar 保持登录态"的修复。**构建需带 tag**：

```bash
go run -tags cookietest ./tests/cookietest/mock.go
go run -tags cookietest ./tests/cookietest/wsclient.go
```

- `tests/` 下其余 `.js` 脚本为浏览器侧 JS 桩注入 / 导流检查，配合 Playwright 等手动运行。

## 12. 文档导航

| 文档 | 内容 |
|------|------|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 模块划分、一次请求的完整生命周期、关键设计决策 |
| [docs/CONFIG.md](docs/CONFIG.md) | config.json 全字段 + SSRF 三档场景化配置示例 |
| [docs/DEPLOY.md](docs/DEPLOY.md) | 本地/Docker/compose/NAS 部署、配置挂载、日志、升级与卸载 |
| [docs/SECURITY.md](docs/SECURITY.md) | 威胁模型、已实现防护、残留风险与加固清单 |
| [SECURITY.md](SECURITY.md) | 安全政策与漏洞报告入口（GitHub 安全页引用） |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献流程、本地校验、提交规范 |
| [CHANGELOG.md](CHANGELOG.md) | 版本更新记录 |
| [docs/API.md](docs/API.md) | 路由、URL 编码规范、Header 处理规则、错误码、会话机制 |

## License

本项目以 [MIT License](LICENSE) 开源。server 端默认不内置登录鉴权，**公网暴露前请务必按 [SECURITY.md](SECURITY.md) 加固（前置 Basic Auth / VPN）**，否则等于开放代理，会被滥用并放大 SSRF 敞口。
