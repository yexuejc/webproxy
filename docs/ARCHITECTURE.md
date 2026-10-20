# 架构设计

## 1. 模块划分

| 模块 | 文件 | 职责 |
|------|------|------|
| 入口/路由 | `main.go` | 加载 `config.json`（缺失则用内置安全默认），注册 `/`（UI）与 `/p/`（代理入口，前缀由 `base_path` 决定） |
| 转发引擎 | `proxy.go` | 解码目标 URL → SSRF 前置检查 → 构造上游请求（方法/Header/Body 透传）→ 重定向改写 → 按 Content-Type 分流回填 |
| 重写引擎 | `rewriter.go` | HTML DOM 遍历改写 URL 属性；CSS `url()` 改写；生成并注入 JS 代理桩；base64url 编解码 |
| SSRF 防护 | `ssrf.go` | 协议检查、DNS 解析、IP 分类（loopback/link-local/private/reserved/denied/whitelist）、CIDR 匹配 |
| 会话隔离 | `session.go` | `sid` Cookie → 独立 `http.CookieJar`（sync.Map），按浏览者隔离登录态 |
| 前端 UI | `static/index.html` | URL 输入栏 + iframe；输入后编码为 `/p/<b64url>` 加载到 iframe |
| 部署 | `Dockerfile` / `docker-compose.yml` / `deploy.sh` | 多阶段构建、compose、一键脚本 |

## 2. 一次请求的完整生命周期

以浏览器访问 `http://192.168.1.10:8080/login` 为例：

```
[浏览器]  iframe.src = /p/aHR0cDovLzE5Mi4xNjguMS4xMDo4MDgwL2xvZ2lu
   │
   ▼
[webproxy] proxyHandler
   │
   ├─1. getSessionID(sid Cookie，无则下发 HttpOnly 新 sid)
   │
   ├─2. base64url(RawURLEncoding) 解码 → 目标 URL
   │      解码失败/无 Scheme/无 Host          → 400
   │      目标 Host == 代理自身 Host          → 400（防自引用死循环）
   │
   ├─3. ssrfCfg.resolveAndCheck(target)
   │      net.LookupIP 解析 → 逐 IP 分类校验（模式见 CONFIG.md）
   │      命中拦截                              → 403（响应体给出原因）
   │
   ├─4. 构造上游请求
   │      方法透传；非 GET/HEAD 透传 Body（表单/JSON 登录正常）
   │      Header 透传（剥 hop-by-hop）；
   │      强制 Accept-Encoding: identity（拿原文自己做重写）
   │
   ├─5. http.Client 发起请求
   │      Transport.DialContext 用第 3 步解析出的 IP 直连（pin）
   │        └─ 防 DNS 重绑定 TOCTOU：校验后域名即使改解析也不影响本次连接
   │      Jar = 会话 CookieJar（跨请求保持登录态）
   │      CheckRedirect = ErrUseLastResponse（不自动跟随，自己改写）
   │      TLS MinVersion 1.2；InsecureSkipVerify 可配（内网自签证书）
   │      DisableKeepAlives = true（每个上游连接一次即弃，简单优先）
   │
   ├─6. 收到响应后分流
   │      3xx 且有 Location：解析成绝对 URL → 改写成 /p/... → 回 302
   │      text/html：全量读入 → DOM 重写 → 注入 JS 桩 → 输出
   │      text/css ：全量读入 → url() 重写 → 输出
   │      其他（图片/JS/字体/音视频）：按原 StatusCode + 原 Header 流式 io.Copy
   │
   ▼
[目标站点 192.168.1.10:8080]
```

## 3. URL 改写规则（重写引擎）

### 3.1 HTML 属性

遍历改写（`urlAttrs`）：`href`、`src`、`action`、`formaction`、`data`、`poster`、`cite`、`background`、`longdesc`、`xlink:href`、`lowsrc`。

- 相对/绝对 URL 先 `base.ResolveReference` 解析成绝对 URL，再编码为 `/p/<b64url>`；
- 只改写 `http/https` 协议（其他协议即便解析出来也不碰）；
- **不改写**：`#` 锚点、`javascript:`、`data:`、`blob:`、`mailto:`、`tel:`、`about:`、`chrome:`、`filesystem:`、已带 `/p/` 前缀的（防重复包装）;
- 解析结果 Host 等于代理自身时跳过（防把 UI 自身资源再代理一次）。

### 3.2 CSS

`<style>` 内联文本、`style` 属性的 `url(...)`、以及 `text/css` 响应体，统一走同一改写函数：`url(inner)` → `url("/p/<b64url(abs)>)`。保留原引号风格语义，输出统一双引号形式。

### 3.3 编码自适应

上游返回先用 `charset.NewReader` 按 Content-Type/首字节嗅探字符集（GBK/UTF-8 等），解析重写后再以 UTF-8 输出——所以中文页面重写不乱码。

## 4. JS 代理桩（覆盖动态请求）

JS 桩通过 **DOM 方式**注入：HTML 解析重写完成后，在 `<head>` 末尾追加一个 `<script>` 节点（桩代码作为其文本子节点），再整体 Render 输出。在页面上下文内重写：

| 目标 | 行为 |
|------|------|
| `window.fetch` | 字符串/Request 入参的 URL 先过 `px()` 转 `/p/` 再请求 |
| `XMLHttpRequest.prototype.open` | 同上 |
| `window.WebSocket` | 非 `http(s)` 协议（`ws://`/`wss://`）原样透传；服务端未实现 WS 双向通道 |
| `window.open` | 新窗口 URL 转 `/p/` |
| `document` submit（捕获阶段） | 表单 `action` 解析后指向非代理主机时才改写，覆盖 JS 动态设置的 action |

`px()` 逻辑：相对 URL 先 `new URL(u, BASE)` 解析（BASE = 当前代理页面对应的目标 origin），非 `http(s)` 原样返回，否则编码为 `/p/<b64url>`。UI 输入框的 `enc()` 与注入桩的 `enc()` 算法一致（UTF-8 → base64 → `+/→-_` → 去尾部 `=`）。

> **为什么必须 DOM 注入**：字节级搜索 `</head>` 定位注入点不可靠——页面内联 `<script>`/模板字符串里出现 `</head>` 字面量时（`document.write` 整页模板的常见写法），桩会被插进页面自身脚本内部：落在 `//` 注释里则静默失效，落在字符串/表达式里则引号失衡直接语法报错（线上实例出现过 `Unexpected token ')'`）。解析后以 DOM 节点追加到 `<head>` 末尾，结构上不可能落进脚本文本区。

## 5. 关键设计决策（为什么这么做）

| 决策 | 理由 |
|------|------|
| **pin 解析后的 IP 直连** | 先校验再连接的经典 TOCTOU 漏洞：域名解析一次、连接时再解析一次，中间可换 IP 绕过黑名单。pin 后攻击者无法借 DNS 重绑定打到内网 |
| **不自动跟随重定向（ErrUseLastResponse）** | 302 是页面流程的一部分，交给浏览器在代理内继续跳转，服务端无状态、无重定向风暴；同时每次跳转都重新过 SSRF 检查，跳转链上的每一跳都受控 |
| **剥离目标站的 `X-Frame-Options` / `CSP` / `COOP`** | UI 用 iframe 承载。目标站这些响应头会阻止被嵌入/阻止注入 JS 生效，与代理形态冲突，必须剥 |
| **顶层导航 302 改道到外框（`/?url=`）** | `target="_blank"` 链接/`window.open` 会在新标签顶层打开 `/p/xxx`，若无处理则新标签没有 UI 外框。服务端按 `Sec-Fetch-Dest: document`（老浏览器按 `Accept: text/html` 前缀兜底）识别顶层导航后 302 到 `/?url=xxx`，由外框 iframe 重新加载（UI 的 iframe 带 `?_wpframe=1` 标记防循环）；AJAX（`Accept: */*`）与 iframe 内导航不受影响，POST 顶层不改道以保表单语义 |
| **强制 `Accept-Encoding: identity`** | 若让目标站返回 gzip/br，本地重写前还要解压；直接要原文，逻辑单一、开销小 |
| **按 sid 隔离 CookieJar** | 多人用同一实例时登录态不串号；本地单人可以忽略但架构保留 |
| **CSS/HTML 全量读内存，其他流式** | 重写必须拿到完整文档；而图片/视频等大文件走 `io.Copy` 流式，不全量缓存 |
| **SSRF 默认放行 RFC1918、拦死链路本地** | 访问内网站点是核心诉求（不能一刀切禁内网）；而 `169.254.169.254` 云元数据、`127.0.0.1` 管理口是最典型的被打的面，默认拦 |

## 6. 数据流总览（简化）

```
浏览器 ──GET /p/<b64>──▶ webproxy ──GET 目标URL──▶ 目标站
       ◀──HTML(已重写)──            ◀──HTML(原文)──
浏览器 ──GET /p/<b64>(img)──▶ webproxy ──GET 图片──▶ 目标站
       ◀──image/png 流式──          ◀──image/png──
浏览器 ──POST /p/<b64>(表单)──▶ webproxy ──POST 表单──▶ 目标站
       ◀──302 /p/<b64>(下跳页)──    ◀──302 原文Location──
```
