# vidlink 常用开发与运维命令。
#
# 设计意图：把所有"应该跑哪些检查"固化下来。CI 与本地用同一组目标，
# 避免"本地过了 CI 挂"这类只由命令差异造成的失败。

BINARY  := vidlink
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## 显示所有可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 构建当前平台的二进制
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: build-arm64
build-arm64: ## 构建树莓派用的 linux/arm64 二进制
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
	  go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

.PHONY: build-all
build-all: ## 构建三个常用目标平台
	@for t in linux/amd64 linux/arm64 darwin/arm64; do \
	  os=$${t%%/*}; arch=$${t##*/}; \
	  echo "-> $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch . || exit 1; \
	done

.PHONY: run
run: ## 本地启动（默认 :8080）
	go run . 

.PHONY: fmt
fmt: ## 格式化
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 用；有未格式化文件则失败）
	@out=$$(gofmt -l . | grep -v '^refs/' || true); \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: test
test: ## 单元测试（不含实网联调）
	go test ./...

.PHONY: test-live
test-live: ## 实网联调（需要 VIDLINK_LIVE=1 与真实 Cookie）
	VIDLINK_LIVE=1 go test ./... -run Live -v

.PHONY: cover
cover: ## 覆盖率报告
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

.PHONY: check
check: fmt-check vet test ## CI 入口：格式 + 静态检查 + 测试

.PHONY: mint-douyin
mint-douyin: ## 铸造一个抖音访客身份（需要 curl 与 node；脚本仅本地保留，未随仓库发布）
	@test -x ./tools/douyin-mint/mint-identity.sh || { \
	  echo "tools/douyin-mint/ 未随仓库发布，请使用你本地保留的副本"; exit 1; }
	@./tools/douyin-mint/mint-identity.sh

.PHONY: openapi
openapi: ## 校验 OpenAPI 规格可解析
	@python3 -c "import yaml,sys; yaml.safe_load(open('docs/openapi.yaml')); print('openapi.yaml OK')"

.PHONY: docker
docker: ## 构建容器镜像
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):latest .

.PHONY: size
size: build ## 报告二进制体积与依赖数
	@echo "二进制: $$(du -h $(BINARY) | cut -f1)"
	@echo "第三方依赖: $$(go list -m all | tail -n +2 | wc -l) 个"

.PHONY: clean
clean: ## 清理构建产物
	rm -rf $(BINARY) $(BINARY)-linux-arm64 dist coverage.out
