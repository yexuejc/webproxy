# WebProxy 便捷命令
# 用法：make <target>

BINARY      := webproxy
PKG         := .
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint clean run docker help

all: fmt vet test build

## build: 编译二进制到 ./webproxy
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## test: 运行单元测试
test:
	go test ./...

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化代码
fmt:
	gofmt -w .

## lint: 检查格式是否合规（CI 用，不修改文件）
lint:
	@test -z "$$(gofmt -l . | grep -v vendor)" || (echo "以下文件未 gofmt："; gofmt -l . | grep -v vendor; exit 1)

## run: 本地运行（config.json 缺失时自动用内置安全默认值）
run:
	go run .

## docker: 构建镜像
docker:
	docker build -t webproxy .

## clean: 清理构建产物
clean:
	rm -f $(BINARY) $(BINARY).exe

## help: 显示本帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
