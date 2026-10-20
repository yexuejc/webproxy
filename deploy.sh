#!/usr/bin/env bash
# webproxy 一键 Docker 部署脚本
# 用法: ./deploy.sh   （PORT=9000 ./deploy.sh 可改对外端口）
set -euo pipefail
cd "$(dirname "$0")"

PORT="${WEBPROXY_PORT:-8765}"
NAME="webproxy"
IMAGE="webproxy:latest"

echo "==> [1/3] 检查 Docker"
if ! command -v docker >/dev/null 2>&1; then
  echo "未检测到 docker，请先安装 Docker 再运行本脚本" >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "Docker 守护进程未运行/无权限，请先启动 Docker 或将当前用户加入 docker 组" >&2
  exit 1
fi

echo "==> [2/3] 构建镜像 ${IMAGE}（首次会拉取基础镜像，需联网）"
docker build -t "${IMAGE}" .

if docker ps -a --format '{{.Names}}' | grep -q "^${NAME}\$"; then
  echo "    停止并清理旧容器 ${NAME}"
  docker stop "${NAME}" >/dev/null 2>&1 || true
  docker rm   "${NAME}" >/dev/null 2>&1 || true
fi

echo "==> [3/3] 启动容器（对外端口 ${PORT}）"
docker run -d \
  --name "${NAME}" \
  --restart unless-stopped \
  -p "${PORT}:8765" \
  "${IMAGE}"

cat <<EOF

部署完成 🎉
  访问:      http://<服务器IP>:${PORT}/
  改端口:    PORT=9000 ./deploy.sh
  看日志:    docker logs -f ${NAME}
  停止删除:  docker rm -f ${NAME}
说明:
  - 配置与 SSRF 策略改 webproxy/config.json 后重跑 ./deploy.sh 生效（容器内用内嵌配置）。
  - 也可改用: docker compose up -d --build
EOF
