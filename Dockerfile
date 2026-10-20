# 多阶段构建：Go 编译 + 极简运行
FROM golang:1.23-alpine AS build
# 国内/受限网络访问 proxy.golang.org 会被拒，改用国内模块代理镜像（多个 fallback）
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,https://mirrors.aliyun.com/goproxy/,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/webproxy .

FROM alpine:3.20
# ca-certificates 用于校验目标站点 HTTPS 证书
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/webproxy /app/webproxy
COPY config.json /app/config.json
COPY static/index.html /app/static/index.html

EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8765/ >/dev/null 2>&1 || exit 1

CMD ["/app/webproxy"]
