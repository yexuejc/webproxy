# 配置说明（config.json）

配置文件位于程序工作目录下的 `config.json`，**启动时加载一次**，修改后需重启生效。文件缺失时使用内置安全默认值（等价于下文默认列）。

## 1. 字段总览

```json
{
  "listen": ":8765",
  "base_path": "/p/",
  "insecure_skip_verify": false,
  "ssrf": {
    "mode": "blacklist",
    "block_loopback": true,
    "block_link_local": true,
    "block_reserved": false,
    "block_private": false,
    "denied_ips": [],
    "denied_protocols": ["file", "gopher", "ftp", "ftps", "dict", "tftp", "ldap", "sftp"],
    "allowed_ips": [],
    "allowed_domains": [],
    "allowed_protocols": ["http", "https"]
  }
}
```

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `listen` | string | `:8765` | 监听地址。`:8765` 监听全部网卡；`127.0.0.1:8765` 只监听本机 |
| `base_path` | string | `/p/` | 代理 URL 前缀。**改动影响两端**：服务端路由与注入 JS 的 `PREFIX` 同源读取该值，无需手工同步；但已收藏/外发的 `/p/` 链接会失效 |
| `insecure_skip_verify` | bool | `false` | 跳过目标站点 HTTPS 证书校验。仅用于内网自签证书站点；公网目标不应开启 |
| `ssrf.mode` | string | `blacklist` | `off` / `blacklist` / `whitelist`，见第 2 节 |
| `ssrf.block_loopback` | bool | `true` | 拦 `127.0.0.0/8`、`::1`。仅在 blacklist 档生效 |
| `ssrf.block_link_local` | bool | `true` | 拦 `169.254.0.0/16`（云元数据 `169.254.169.254` 在此段）及 IPv6 链路本地。仅在 blacklist 档生效 |
| `ssrf.block_private` | bool | `false` | 拦 RFC1918（10/8、172.16/12、192.168/16）等私网。**默认 false**：本工具的核心场景就是访问内网站点，如需收紧再打开 |
| `ssrf.block_reserved` | bool | `false` | 拦保留段：`100.64.0.0/10`（CGNAT）、`192.0.0.0/24`、`198.18.0.0/15`、`240.0.0.0/4` |
| `ssrf.denied_ips` | []string | `[]` | 附加黑名单，CIDR 格式，如 `["192.168.9.0/24", "172.16.5.0/24"]`。命中即拦 |
| `ssrf.denied_protocols` | []string | 见示例 | 黑名单禁用的协议。注意 blacklist 档只拦这里列出的 + 上述 IP 分类命中的；`allowed_protocols` 在黑名单档不生效 |
| `ssrf.allowed_ips` | []string | `[]` | 白名单 IP/CIDR。whitelist 档下，目标解析出的所有 IP 必须命中其一（或域名命中 `allowed_domains`） |
| `ssrf.allowed_domains` | []string | `[]` | 白名单域名，精确匹配或子域后缀匹配（`example.com` 覆盖 `a.example.com`） |
| `ssrf.allowed_protocols` | []string | `["http","https"]` | whitelist 档的协议白名单 |

> 校验粒度的两个细节：
> 1. blacklist 档对域名解析出的**每一个 IP** 分类，任一命中即拒；
> 2. whitelist 档要求**所有**解析 IP 命中白名单（IP 段或域名任一满足即可）。

## 2. SSRF 三档模式与场景

上线环境不确定时，策略分三档；对应三种真实网络情况：

### 2.1 `mode: "off"` —— 专用服务器、完全可信

不做出向 IP/协议检查，仅做 DNS 解析并把解析结果 pin 住连接。适用于：服务器是你自己的专用机、网络平面干净、不存在被打一下就有损失的内网邻接。

```json
{ "ssrf": { "mode": "off" } }
```

### 2.2 `mode: "blacklist"` —— 复杂网络，封部分 IP/协议

默认放行，仅拦截配置命中的目标。默认值已拦住最典型的危险面（回环、云元数据、非常规协议），同时**默认放行私网**，不挡"访问客户网站"这个核心诉求。在此基础上按需收紧：

```json
{
  "ssrf": {
    "mode": "blacklist",
    "block_private": true,
    "denied_ips": ["172.16.5.0/24", "192.168.100.0/24"],
    "denied_protocols": ["file", "gopher", "ftp", "ftps", "dict", "tftp", "ldap", "sftp"]
  }
}
```

上面示例：禁一切私网 + 额外封两段内网 + 禁非常规协议（公网站点仍可访问）。

### 2.3 `mode: "whitelist"` —— 只能白名单访问

只允许 `allowed_ips`（CIDR）或 `allowed_domains`（精确/子域）内的目标，协议仅 `allowed_protocols`。最严格，适合"客户站点范围明确"的上线形态：

```json
{
  "ssrf": {
    "mode": "whitelist",
    "allowed_ips": ["10.0.0.0/24", "192.168.1.10/32"],
    "allowed_domains": ["crm.customer-a.com"],
    "allowed_protocols": ["http", "https"]
  }
}
```

抓取一段常见配置片段的效果：访问 `10.0.0.7` 放行、访问 `github.com` 拒（"目标地址被拦截(whitelist)"）。

## 3. 常见配置组合

| 场景 | 配置要点 |
|------|----------|
| 本地开发调试 | 默认即可；若要代理到本机目标站（`127.0.0.1`）需临时把 `block_loopback` 设 `false`，**调完务必改回 true** |
| 内网客户站点多为自签 HTTPS | `insecure_skip_verify: true` |
| 上 NAS、只给局域网用 | 默认 blacklist；把 `listen` 设为 NAS 内网地址更稳妥 |
| 上公网 | 先加入口鉴权（见 SECURITY.md），建议 `mode: "whitelist"` 并把代理整体挂反代 HTTPS 之后 |
| 多实例/同一服务器再套一层路径 | 改 `base_path`（如 `/wp/`），注意 UI 与外链同时受影响 |

## 4. 生效方式

- 本地：改 `config.json` → 重启进程；
- Docker：`config.json` 打进镜像，改完需重跑 `./deploy.sh`（或 compose 挂载 volume 覆盖，见 [DEPLOY.md](DEPLOY.md) 第 4 节）；
- 配置文件 JSON 语法错误：启动直接 panic 并给出解析错误，不会带病运行。
