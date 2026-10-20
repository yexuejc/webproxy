# 部署指南

覆盖四种形态：本地跑通 → 单二进制上服务器 → Docker 一键 → 上 NAS。生产变更前先备份 `config.json`，确认变更窗口与回退方式。

## 1. 本地跑通（开发/验证）

前置：Go 1.22+（go.mod 声明 `go 1.22`，开发机 1.23 验证通过）。

```bash
cd webproxy
go run .                # 直接跑
# 或编译后运行：
go build -o webproxy . && ./webproxy
```

浏览器打开 `http://127.0.0.1:8765/`，输入目标 URL 回车。局域网联调时用 `http://<本机IP>:8765/`。

调试提示：
- 要看 SSRF 拦截原因：`curl -s "http://127.0.0.1:8765/p/$(printf '%s' 'http://127.0.0.1:9/' | base64 | tr '+/' '-_' | tr -d '=')"` 会返回 403 + 原因;
- 代理到本机目标站时，先临时把 `config.json` 的 `ssrf.block_loopback` 改为 `false`，调完改回 `true`。

## 2. 单二进制上服务器（无 Docker）

```bash
# 在有 Go 的机器上交叉编译（服务器是 Linux amd64 时）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o webproxy .
# 拷贝 webproxy、config.json、static/ 到服务器同一目录（三者相对位置必须保持）
scp -r webproxy config.json static user@server:/opt/webproxy/
ssh user@server 'cd /opt/webproxy && ./webproxy'
```

`static/index.html` 与 `config.json` 都是**运行时相对路径读取**，缺一个 UI 就 500。建议配 systemd：

```ini
# /etc/systemd/system/webproxy.service
[Unit]
Description=webproxy
After=network.target

[Service]
WorkingDirectory=/opt/webproxy
ExecStart=/opt/webproxy/webproxy
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now webproxy
journalctl -u webproxy -f   # 看日志
```

## 3. Docker 一键部署（推荐上服务器/NAS）

前置：Docker 已装、daemon 在跑、当前用户有权限（不在 `docker` 组就 `sudo` 或加组）。

```bash
cd webproxy
./deploy.sh                  # 默认对外 8765
PORT=9000 ./deploy.sh        # 自定义对外端口（WEBPROXY_PORT 亦可）
```

脚本做三件事：检查 Docker → `docker build` 构建镜像（首次联网拉 `golang:1.23-alpine`、`alpine:3.20`）→ 清理同名旧容器后 `docker run -d --restart unless-stopped -p ${PORT}:8765`。

完成后访问 `http://<服务器IP>:8765/`。

常用运维命令：

```bash
docker logs -f webproxy          # 跟踪日志
docker rm -f webproxy            # 停止并删除容器
docker images | grep webproxy    # 看镜像
```

### 3.1 compose 方式

```bash
docker compose up -d --build
```

`docker-compose.yml` 默认映射 `8765:8765`；换端口改 `ports` 段。需要实时覆盖配置（不重打镜像）时取消 volumes 注释：

```yaml
    volumes:
      - ./config.json:/app/config.json:ro
```

> 挂载后改宿主机 `config.json` 重启容器即生效：`docker compose restart`。

### 3.2 镜像说明

- 多阶段构建：编译静态二进制（`CGO_ENABLED=0`），运行基座 `alpine:3.20`，打进去 `ca-certificates`（校验目标 HTTPS）与二进制 + `config.json` + `static/index.html`；
- 容器内固定监听 `8765`（改监听要改 `config.json` 并重新构建）；
- 带 HEALTHCHECK（每 30s `wget` 探一次 `/`）；
- `.dockerignore` 已排除 `webproxy.exe`、`.git`、测试目录，构建上下文干净。

### 3.3 构建时拉取 Go 依赖失败 / 离线构建

`docker compose up -d` 或 `docker build` 在第一步 `RUN go mod download` 报类似
`dial tcp 142.250.x.x:443: connect: connection refused`（目标是 `proxy.golang.org`），
是**构建环境无法直连 Google Go 模块代理**所致（国内/受限网络常见），与代码无关。

**方案 A（推荐，适用大多数网络）**：`Dockerfile` 已把模块代理改为国内镜像：

```dockerfile
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,https://mirrors.aliyun.com/goproxy/,direct
```

直接重跑即可：

```bash
docker compose up -d --build
# 或
./deploy.sh
```

**方案 B（服务器完全离线 / 纯内网，连 goproxy.cn 也不通）**：依赖已通过 `go mod vendor` 预先打包进 `vendor/`，用离线 Dockerfile 构建，全程不联网：

```bash
# vendor/ 已由开发机（能联网）通过 go mod vendor 生成，直接拷到服务器构建：
docker build -f Dockerfile.offline -t webproxy:latest .
docker run -d --restart unless-stopped -p 8765:8765 --name webproxy webproxy:latest
# compose 用户：把 docker-compose.yml 的 build.dockerfile 改为 Dockerfile.offline
```

> 若 `vendor/` 缺失（如换了依赖后），在有网的机器执行 `go mod vendor` 重新生成，再拷到服务器。
> `.dockerignore` 已确认**未**排除 `vendor/`，离线 `COPY vendor/` 可正常命中。

## 4. 上 NAS（群晖 DSM / 威联通 QNAP）

镜像本身无特殊要求，用 NAS 自带的容器管理即可：

**群晖 DSM 7+（Container Manager）**
1. 套件中心安装 Container Manager；
2. 项目 → 新增 → 上传 `webproxy` 目录（或docker-compose.yml 内容粘贴），启动；
3. 或直接用映像方式：把 `webproxy` 目录传到 NAS，ssh 进 NAS 跑 `./deploy.sh`。

**威联通（Container Station）**
1. App Center 安装 Container Station；
2. "创建应用程序"导入 `docker-compose.yml`，或镜像页拉取/构建后创建容器，端口映射 `8765`。

NAS 提示：
- NAS 与目标内网必须**路由可达**——部署在 NAS 上时它看到的内网就是 NAS 所在网段；
- 只给家里/办公网用就够，别映射到公网；确需公网见 [SECURITY.md](SECURITY.md) 的加固清单；
- NAS 的存储空间放配置即可，镜像不占数据卷。

## 5. 升级与回退

| 操作 | 命令 |
|------|------|
| 升级到新代码 | 更新源码目录后 `./deploy.sh`（自动重建+换容器）；compose 用 `docker compose up -d --build` |
| 只改配置 | 本地改 `config.json` → `./deploy.sh`；或挂载 volume 方式下 `docker compose restart` |
| 回退 | 代码回滚后重跑 `./deploy.sh`；配置回退=恢复备份的 `config.json` 再重启 |
| 完全卸载 | `docker rm -f webproxy && docker rmi webproxy:latest` |

## 6. 网络与防火墙 checklist

- [ ] 服务器到目标内网路由可达（`curl -I http://目标IP` 在服务器上先验证）；
- [ ] 对外端口已在安全组/防火墙放行（只放需要的端口，如 9000）；
- [ ] 公网暴露前已完成入口鉴权（见 SECURITY.md）；
- [ ] `config.json` 的 SSRF 档位与实际上线环境匹配（off/blacklist/whitelist）；
- [ ] 内网目标为 HTTPS 自签证书时已设 `insecure_skip_verify: true`。
