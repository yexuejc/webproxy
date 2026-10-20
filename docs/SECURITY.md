# 安全说明

## 1. 威胁模型（按危害排序）

| # | 威胁 | 说明 | 现状 |
|---|------|------|------|
| 1 | **开放代理被滥用** | 无鉴权暴露公网 → 被扫到当成免费代理/跳板，耗流量、引溯源投诉 | **未防护**（鉴权未实现，见第 3 节加固项） |
| 2 | **SSRF（服务端请求伪造）** | 攻击者借你的服务器打内网/云元数据 `169.254.169.254`/`127.0.0.1` 管理口/`file://` 等 | **默认防护**，三档可配（见第 2 节） |
| 3 | **同源 XSS** | 被代理页面与 UI 同源（iframe 无 sandbox），恶意站点 JS 可触达父窗口/cookie（`sid` 已 HttpOnly） | **部分防护**，缓解项见第 3 节 |
| 4 | **会话 grow** | `sessions` 无 TTL，长期运行缓慢增长 | 已知，重启即清（规划 TTL 清理） |
| 5 | 上游证书风险 | 目标 HTTPS 证书默认校验；`insecure_skip_verify` 打开后中间人可解密内网流量 | 由配置项控制，公网目标不建议开 |

## 2. 已实现的防护

| 防护 | 实现 |
|------|------|
| 协议白/黑名单 | 默认仅 `http/https`；blacklist 档禁用 `file/gopher/ftp/ftps/dict/tftp/ldap/sftp` |
| IP 分类拦截 | 默认拦 `127/8`、`::1`（loopback）、`169.254/16`（link-local，含云元数据）；可选 `block_private`、`block_reserved` |
| 附加黑名单 | `denied_ips` CIDR 列表，逐 IP 命中即拒 |
| 白名单模式 | `whitelist` 档仅放行 `allowed_ips` / `allowed_domains`（精确+子域） |
| DNS 重绑定防护 | 先 `LookupIP` 校验、再用**校验过的 IP pin 连接**，消除 TOCTOU 窗口 |
| 自引用防护 | 目标 Host == 代理 Host 直接 400，防 `/p/` 咬尾 |
| 会话隔离 | `sid` Cookie（`HttpOnly`，`Path=/`）→ 独立 CookieJar，多使用者登录态不串 |
| 响应头清洗 | 剥离上游 `X-Frame-Options`/`CSP`/`COOP`（iframe 嵌入与 JS 注入所需）、`Content-Length`（重写后长度必变）、`Location`（统一改写） |
| TLS 加固 | 上游 TLS 最低 1.2 |

> SSRF 分档与场景配置的完整说明见 [CONFIG.md](CONFIG.md) 第 2 节。

## 3. 上线前加固清单

### 3.1 入口鉴权（最高优先级）

当前**没有任何登录机制**。公网暴露前，任选其一：

1. **反代 Basic Auth（最省事）**：nginx/Traefik/Caddy 前置，认证通过才转发到 8765；
2. **VPN/零信任**：只通过 Tailscale/WireGuard/企业 VPN 访问，代理只听内网 `listen`；
3. 后续版本计划内置账号登录（当前未实现，别依赖）。

反代示例（nginx，同时解决 HTTPS 和 Basic Auth）：

```nginx
server {
  listen 443 ssl;
  server_name proxy.example.com;
  ssl_certificate     /etc/nginx/cert.pem;
  ssl_certificate_key /etc/nginx/key.pem;

  auth_basic "webproxy";
  auth_basic_user_file /etc/nginx/.htpasswd;

  location / {
    proxy_pass http://127.0.0.1:8765;
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;    # 如后续启用 WS
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 300s;
  }
}
```

### 3.2 SSRF 策略收紧建议

| 上线形态 | 建议 |
|----------|------|
| 专用服务器、网络平面干净 | `mode: "off"` 可接受；仍建议 `denied_ips` 写上同机管理网段 |
| 共享服务器/VM | `mode: "blacklist"`（默认）+ `block_private: true`，私网按站点放开 `denied_ips` 的补集用白名单补 |
| 公网多人可用 | `mode: "whitelist"` + 入口鉴权，双闸门 |

### 3.3 同源 XSS 缓解（可选，有取舍）

给 UI 的 iframe 加 `sandbox="allow-scripts allow-forms allow-popups"`（**不**加 `allow-same-origin`）可让被代理页面跑在独立源，父窗口不再可触达。代价：沙箱源不带 `sid` Cookie，登录态隔离会失效，需等服务端改为按 iframe 参数/子域传会话再启用。单机自用、且不访问不可信站点时可维持现状。

### 3.4 运维面

- 只暴露必要端口，安全组做源 IP 限制更佳；
- `docker logs`/systemd journal 定期巡检 403 记录（403 的突增=有人在扫你的代理）；
- 升级/改配置前备份 `config.json`，保留可回退版本。

## 4. 残留风险如实说明

1. **无鉴权 = 无访问控制**：在补上 3.1 之前，任何能访问到端口的人都能使用该代理，请勿直接公网裸奔；
2. **黑名单是"默认放行"语义**：blacklist 档放行未列出的一切目标，它防的是"经典被打点"，不是"最小权限"；要最小权限用 whitelist；
3. **同源面**：见 3.3，未启用 sandbox 前，请只把不可信站点的问题留在"知情"层面；
4. **性能/资源**：默认纯 Go 转发开销极低，无已知拒绝服务放大点（无全量缓存），但 HTML/CSS 走内存，超大页面有内存压力（规划流式）。
