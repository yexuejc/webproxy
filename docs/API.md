# 接口与 URL 规范

## 1. 路由

| 路径 | 方法 | 说明 |
|------|------|------|
| `/` | GET | 前端 UI（`static/index.html`）。其他根路径（如 `/foo`）返回 404 |
| `/?url=<base64url>` | GET | 带 `url` 参数时 UI 自动回填输入框并把目标装进 iframe（新标签 302 落地页） |
| `{base_path}`（默认 `/p/`） | 任意 | 代理入口，见第 2 节。页面、静态资源、表单提交都走这里；顶层导航时 302 改道到 `/?url=`（见 3.3） |

`base_path` 可通过 `config.json` 的 `base_path` 修改；服务端路由、UI 的 iframe 地址、注入 JS 的 `PREFIX` 三处同源生效，无需手工同步。

## 2. 代理 URL 格式

```
/p/<base64url(目标URL)>
```

编码规则（与 UI 输入框 `enc()`、注入 JS `enc()` 完全一致）：

1. UTF-8 编码字符串为字节；
2. 标准 base64；
3. `+` → `-`、`/` → `_`（URL 安全字符）；
4. **去掉尾部 `=` 填充**（Go 侧用 `base64.RawURLEncoding` 解码）。

示例：

| 目标 URL | 代理地址 |
|----------|----------|
| `http://example.com/` | `/p/aHR0cDovL2V4YW1wbGUuY29tLw` |
| `http://192.168.1.10:8080/` | `/p/aHR0cDovLzE5Mi4xNjguMS4xMDo4MDgwLw` |
| `https://www.baidu.com/s?wd=test` | `/p/aHR0cHM6Ly93d3cuYmFpZHUuY29tL3M_d2Q9dGVzdA` |
| `http://10.0.0.5/login` | `/p/aHR0cDovLzEwLjAuMC41L2xvZ2lu` |

页面内的 URL 一律先按当前页 origin 解析为绝对 URL 再编码；`#`/`data:`/`blob:`/`javascript:`/`mailto:`/`tel:`/`about:`/`chrome:`/`filesystem:` 及已带前缀的不改写。

## 3. 请求处理规则

### 3.1 转发到上游

| 项 | 行为 |
|----|------|
| Method | 原样透传（GET/POST/PUT/DELETE/HEAD/…） |
| Body | GET/HEAD 不带；其余原样透传（表单、JSON 正常） |
| Header | 透传除 hop-by-hop（`Connection`/`Proxy-Connection`/`Keep-Alive`/`Proxy-Authenticate`/`Proxy-Authorization`/`TE`/`Trailer`/`Transfer-Encoding`/`Upgrade`）以外的全部 |
| Accept-Encoding | 强制 `identity`（拿原文重写） |
| Cookie | 使用**会话级** CookieJar：由 `sid` Cookie 选定，跨请求累积/回传该浏览者的登录态 |
| 连接 | pin 第 2 步校验过的 IP 直连；TLS ≥ 1.2；`InsecureSkipVerify` 可配 |
| 重定向 | 不自动跟随（`ErrUseLastResponse`），见 3.2 |

### 3.2 3xx 重定向改写

上游返回 3xx 且带 `Location` 时：解析成绝对 URL → 编码为 `/p/...` → 以 **302** 回给浏览器，由浏览器在代理内继续跳。注意：

- 统一归一为 302，不保留 301/307/308 语义（307/308 的 POST 会退化为 GET，详见 README 已知局限）；
- 每一跳都会重新过 SSRF 检查，跳转链全程受控；
- `Location` 为空或解析失败时按普通响应处理（`Location` 头被剥离）。

### 3.3 顶层导航改道（新标签自动带外框）

`/p/xxx` 被顶层文档打开时（新标签页、地址栏直开，典型场景：页面里 `target="_blank"` 的链接），此时没有 UI 外框，服务端直接 **302 改道到 `/?url=xxx`**，由外框页把内容装进 iframe——任何新标签都保留地址栏外框。

判定条件（满足其一即改道，需同时 `Method=GET` 且不带 `?_wpframe=1`）：

| 信号 | 说明 |
|------|------|
| `Sec-Fetch-Dest: document` | 现代浏览器顶层导航（iframe 内是 `iframe`/`frame`，不命中） |
| 无 `Sec-Fetch-Dest` 且 `Accept` 以 `text/html` 开头 | 老浏览器兜底；fetch/AJAX 的 Accept 为 `*/*` 不命中 |

不改道的例外：iframe 内加载（`Sec-Fetch-Dest: iframe/frame`）、UI 外框页 iframe 主动携带的 `?_wpframe=1` 标记、以及 **POST 等非 GET 顶层导航**（保住表单语义，此时新标签无外框，属已知取舍）。

### 3.4 响应回填

| Content-Type | 行为 |
|--------------|------|
| `text/html` | 全量读入 → charset 识别 → DOM 重写 → **DOM 方式在 `<head>` 末尾追加 JS 桩节点** → UTF-8 输出 |
| `text/css` | 全量读入 → `url()` 重写 → 输出 |
| 其他 | 原 StatusCode + 原 Header 流式 `io.Copy`（图片/JS/字体/音视频不全量缓存） |

响应头处理：剥除 hop-by-hop 与 `X-Frame-Options`/`Content-Security-Policy`/`Content-Security-Policy-Report-Only`/`Cross-Origin-Opener-Policy`/`Content-Length`/`Location`；其余原样回传（目标站 `Set-Cookie` 也会透传并进会话 Jar）。

## 4. 会话机制

- 首次请求无 `sid` Cookie → 生成 16 字节随机 hex，`Set-Cookie: sid=...; Path=/; HttpOnly`；
- 服务端 `sync.Map` 维护 `sid → http.CookieJar`：同一浏览器访问不同目标站，登录态互不干扰；
- 会话**不设过期、不主动清理**，重启进程即全部清空（已知局限，规划加 TTL）。

## 5. 错误码

| 状态码 | 触发条件 | 响应体 |
|--------|----------|--------|
| 400 | `/p/` 后未带编码串 | `缺少目标地址` |
| 400 | base64url 解码失败 | `目标地址解码失败` |
| 400 | 无 Scheme 或 Host | `目标地址无效` |
| 400 | 目标 Host 等于代理自身 Host | `禁止请求代理自身` |
| 403 | SSRF 拦截 | `SSRF 拦截: 目标地址被拦截(loopback): 127.0.0.1`（原因含 loopback / linklocal / private / reserved / denied / whitelist / 协议类） |
| 404 | 非 `/` 的 UI 路径 | Go 默认 404 |
| 500 | `static/index.html` 缺失等 | 相应错误文案 |
| 502 | 上游连接失败/超时 | `上游请求失败: ...` |

## 6. 注入的 JS 代理桩（摘要）

HTML 响应中注入的脚本会重写：

| 对象 | 行为 |
|------|------|
| `fetch` | URL 先转 `/p/`（字符串与 `Request` 两种入参都处理） |
| `XMLHttpRequest.open` | URL 先转 `/p/` |
| `WebSocket` | 非 `http(s)` 协议（`ws://`）原样透传；服务端未实现 WS 双向通道 |
| `window.open` | URL 转 `/p/` |
| `submit` 事件（捕获阶段） | 动态设置的 form `action` 指向非代理主机时改写 |

`BASE` 取当前页面对应的目标 origin，相对 URL 先 `new URL(u, BASE)` 解析，非 `http(s)` 原样返回——与后端 `resolveURL` 语义一致。
